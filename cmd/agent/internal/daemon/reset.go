// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgent removes the unbounded-agent and all associated resources, stopping
// the daemon first. The daemon's own operation path stops it last instead, so
// that ordering stays with the caller.
func ResetAgent(log *slog.Logger) phases.Task {
	return ownedReset(log, installstate.DefaultStore(), phases.Serial(log, StopDaemon(log), resetResources(log)))
}

// ResetAgentForReprovision is ResetAgent for the reprovision handoff: it leaves
// the handoff's unit, binary, configuration and journal in place.
func ResetAgentForReprovision(log *slog.Logger) phases.Task {
	return ownedReset(log, installstate.DefaultStore(), phases.Serial(log, StopDaemon(log), resetNodeResources(log)))
}

type lifecycleTask struct {
	name string
	run  func(context.Context) error
}

func (t lifecycleTask) Name() string                 { return t.name }
func (t lifecycleTask) Do(ctx context.Context) error { return t.run(ctx) }

func ownedReset(log *slog.Logger, store *installstate.Store, inner phases.Task) phases.Task {
	// The composed name keeps the underlying cleanup sequence visible to callers
	// and to the reset ordering test.
	return lifecycleTask{name: "owned-reset(" + inner.Name() + ")", run: func(ctx context.Context) error {
		lock, err := store.AcquireLock()
		if err != nil {
			return err
		}

		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("release reset lock", "error", err)
			}
		}()

		return resetUnderLock(ctx, log, store, inner)
	}}
}

// recordForTeardown returns the record reset should mark as resetting.
//
// Any record it cannot read is replaced rather than obeyed. Reset is about to
// delete it, so refusing to proceed protects nothing and costs everything:
// decide rejects the same unreadable record, so start is refused too, and the
// host is left with no way out through either path. The guide tells operators
// to keep this file intact, so it must not be the thing that strands them.
func recordForTeardown(log *slog.Logger, store *installstate.Store) (installstate.Record, error) {
	r, err := store.Load()
	if err == nil {
		return r, nil
	}

	if !errors.Is(err, installstate.ErrNotFound) {
		log.Warn("installation record is unreadable; replacing it for teardown", "error", err)
	}

	return installstate.NewRecord("legacy-reset", "legacy-reset")
}

func resetUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, inner phases.Task) error {
	r, err := recordForTeardown(log, store)
	if err != nil {
		return err
	}

	r.Phase = installstate.Resetting
	if err := store.Save(r); err != nil {
		return err
	}
	// Cancel recovery waiting on ownership before removing its executable.
	if err := stopRecoveryUnit(ctx, log); err != nil {
		return err
	}

	paths := teardownSyncPaths(store.Root())
	if hostIsNone() {
		// The node's own directory may be on another filesystem than /etc.
		paths = append(paths, none.DefaultBase)
	}

	if hostIsContainer() {
		paths = append(paths, container.DefaultBase)
	}

	return durableReset(ctx, store, inner, paths, unix.Syncfs)
}

// teardownSyncPaths returns the directories whose filesystems a teardown has to
// persist: those holding the agent's files under either root, and the one the
// host root link is in. They are resolved now, while the host root still leads
// to the files; durableReset opens the nearest existing ancestor of each.
func teardownSyncPaths(storeRoot string) []string {
	return []string{
		"/etc",
		"/var/lib/machines",
		filepath.Dir(hostroot.Path),
		hostroot.Resolve(),
		hostroot.LegacyPath,
		storeRoot,
	}
}

func stopRecoveryUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "stop", goalstates.DaemonRecoveryUnit); err != nil {
		out, inspectErr := executil.OutputCmd(ctx, log, "systemctl", "show", goalstates.DaemonRecoveryUnit, "--property=LoadState", "--value")
		if inspectErr != nil || strings.TrimSpace(out) != "not-found" {
			return fmt.Errorf("stop daemon recovery: %w", err)
		}
	}

	return nil
}

func durableReset(ctx context.Context, store *installstate.Store, inner phases.Task, paths []string, syncfs func(int) error) error {
	var handles []*os.File

	defer func() {
		for _, f := range handles {
			_ = f.Close() //nolint:errcheck // Read-only directory descriptor; teardown sync errors are returned below.
		}
	}()

	for _, path := range paths {
		for {
			if _, err := os.Stat(path); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}

			parent := filepath.Dir(path)
			if parent == path {
				return fmt.Errorf("no filesystem ancestor for %s", path)
			}

			path = parent
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}

		handles = append(handles, f)
	}

	if err := inner.Do(ctx); err != nil {
		return err
	}

	if err := fsutil.SyncOpenFilesystems(handles, syncfs); err != nil {
		return err
	}

	return store.Remove()
}

// hostIsNone reports whether the host runs, or has traces of, a host-native
// node, from the applied config or from the node's own units and directory. The
// second source matters for a reset retried after the applied config is gone.
// Reading the applied config can fail on a damaged host; reset must still
// proceed, so any failure falls back to the traces.
var hostIsNone = func() bool {
	if applied, err := appliedconfig.Read(); err == nil && applied.Mode == isolation.ModeNone {
		return true
	}

	return noneops.Present()
}

// hostIsContainer reports whether the host runs, or has traces of, a node in an
// OCI container, from the applied config or from the node's units and
// directory, for the same reasons as hostIsNone. PROVISIONAL: container mode is
// experimental.
var hostIsContainer = func() bool {
	if applied, err := appliedconfig.Read(); err == nil && applied.Mode == isolation.ModeContainer {
		return true
	}

	return containerops.Present()
}

// resetResources builds the reset sequence. The order is the same for every
// mode: daemon unit, stop, WireGuard keys, config, root, bpffs, network and
// routes, bootstrap unit, artifacts, systemd reload. Only the node steps differ.
// For a host-native node the stop, config and root steps are the none
// teardown's, and there is no per-machine bpffs mount to clean.
func resetResources(log *slog.Logger) phases.Task {
	return phases.Serial(log, resetNodeResources(log), RemoveReprovisionHandoff(log))
}

// resetNodeResources is the reset without the cleanup of a reprovision
// handoff. The handoff's own reset uses it, since the handoff runs from the
// directory the cleanup removes.
func resetNodeResources(log *slog.Logger) phases.Task {
	switch {
	case hostIsNone():
		return resetResourcesNone(log, noneTeardown(log))
	case hostIsContainer():
		return resetResourcesContainer(log, containerTeardown(log))
	default:
		return resetResourcesNSpawn(log)
	}
}

// resetResourcesContainer is the reset for a container node: stop the
// containers, remove their bundles, rootfs and units, then the same host-global
// cleanup. There is no per-machine bpffs mount: the container uses the host's.
func resetResourcesContainer(log *slog.Logger, td *containerops.Teardown) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		td.Stop(),
		reset.RemoveWireGuardKeys(log),
		td.RemoveConfig(),
		td.Destroy(),
		reset.CleanupNetwork(log),
		// Before the artifacts, so a failure stops the reset while the host is
		// still recognizably installed.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

func resetResourcesNone(log *slog.Logger, td *noneops.Teardown) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		td.Stop(),
		reset.RemoveWireGuardKeys(log),
		td.RemoveConfig(),
		td.Destroy(),
		reset.CleanupNetwork(log),
		// Before the artifacts, so a failure stops the reset while the host is
		// still recognizably installed.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

func resetResourcesNSpawn(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		phases.Parallel(log,
			reset.StopMachine(log, goalstates.NSpawnMachineKube1),
			reset.StopMachine(log, goalstates.NSpawnMachineKube2),
		),
		reset.RemoveWireGuardKeys(log),
		phases.Parallel(log,
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube1),
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupNetwork(log),
		// Before the artifacts, so a failure stops the reset while the host is
		// still recognizably installed.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

func releaseInstallationLock(log *slog.Logger, lock *installstate.Lock) {
	if err := lock.Release(); err != nil {
		log.Error("release installation lock", "error", err)
	}
}
