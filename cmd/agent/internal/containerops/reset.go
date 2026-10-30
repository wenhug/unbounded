// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Teardown removes a container node. Its tasks slot into the same positions in
// the reset order as the nspawn machine tasks they replace: stop, then config,
// then the node itself, with the host-global cleanup the caller owns in between
// and after.
type Teardown struct {
	Log *slog.Logger
	// Layout, SystemdDir, RuntimePath and Run are as in Params.
	Layout      container.Layout
	SystemdDir  string
	RuntimePath string
	Run         container.Runner

	// Root prefixes every host path the teardown removes outside the layout and
	// systemd directory. Empty is the real host; tests use a temporary tree.
	Root string
	// MountInfo is the mount table. Empty means /proc/self/mountinfo.
	MountInfo string
}

func (t *Teardown) logger() *slog.Logger {
	if t.Log == nil {
		return slog.Default()
	}

	return t.Log
}

func (t *Teardown) path(p string) string { return filepath.Join(t.Root, p) }

func (t *Teardown) unitDir() string {
	if t.SystemdDir == "" {
		return container.SystemdDir
	}

	return t.SystemdDir
}

func (t *Teardown) node(slot string) *Node {
	return New(Params{
		Log: t.logger(), Slot: slot, Layout: t.Layout, SystemdDir: t.SystemdDir, RuntimePath: t.RuntimePath,
		Run: t.Run, HostRoot: t.Root, MountInfoPath: t.MountInfo,
	})
}

func (t *Teardown) runtimeDir() string {
	rt := t.RuntimePath
	if rt == "" {
		rt = container.DefaultRuntimePath
	}

	return filepath.Dir(rt)
}

// slots are both blue/green slots; reset removes whichever exist.
var slots = []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2}

// Present reports whether the host has any trace of a container node: the
// layout directory or an agent-owned unit. Reset uses it to choose the
// container teardown even after the applied config is gone.
func (t *Teardown) Present() bool {
	if _, err := os.Lstat(t.Layout.RootfsDir("")); err == nil {
		return true
	}

	for _, slot := range slots {
		if _, err := os.Lstat(filepath.Join(t.unitDir(), container.UnitName(slot))); err == nil {
			return true
		}
	}

	return false
}

// Present is the package level form of Teardown.Present for the real host.
func Present() bool { return (&Teardown{}).Present() }

type task struct {
	name string
	do   func(context.Context) error
}

func (t task) Name() string                 { return t.name }
func (t task) Do(ctx context.Context) error { return t.do(ctx) }

// Stop returns the task that stops both slots' containers. A slot that is not
// there is skipped; the node stack inside is stopped first when the container
// runs.
func (t *Teardown) Stop() phases.Task {
	return task{"stop-container-node", func(ctx context.Context) error {
		var errs []error

		for _, slot := range slots {
			if !t.slotPresent(slot) {
				continue
			}

			if err := t.node(slot).Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", slot, err))
			}
		}

		return errors.Join(errs...)
	}}
}

func (t *Teardown) slotPresent(slot string) bool {
	if _, err := os.Lstat(filepath.Join(t.unitDir(), container.UnitName(slot))); err == nil {
		return true
	}

	_, err := os.Lstat(t.Layout.RootfsDir(slot))

	return err == nil
}

// RemoveConfig returns the task that removes the configuration the agent wrote
// outside the slots: the host kernel modules list (written by bootstrap).
func (t *Teardown) RemoveConfig() phases.Task {
	return task{"remove-container-config", func(context.Context) error {
		if err := os.Remove(t.path(noneops.ModulesLoadPath())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	}}
}

// Destroy returns the task that removes the nodes: for each slot the unit,
// bundle and rootfs through the backend, then the shared data the node left on
// the host (pod mounts, kubelet state, CNI binaries and configuration), the
// layout directory and the host runtime.
func (t *Teardown) Destroy() phases.Task {
	return task{"remove-container-node", func(ctx context.Context) error {
		var errs []error

		// Pod volumes propagate to the host under /var/lib/kubelet, so unmount
		// before any removal that could follow a bind mount.
		if err := t.unmountShared(ctx); err != nil {
			errs = append(errs, err)
		}

		for _, slot := range slots {
			if !t.slotPresent(slot) {
				continue
			}

			if err := t.node(slot).Destroy(ctx); err != nil {
				errs = append(errs, fmt.Errorf("destroy %s: %w", slot, err))
			}
		}

		for _, d := range []string{"/var/lib/kubelet", goalstates.CNIConfigDir, goalstates.CNIBinDir, "/var/lib/unbounded/container-images"} {
			if err := os.RemoveAll(t.path(d)); err != nil {
				errs = append(errs, err)
			}
		}

		// Last, and only when the slots are gone, so a failed destroy keeps the
		// directory that explains what is left.
		if len(errs) == 0 {
			errs = append(errs, t.removeLayout()...)
		}

		return errors.Join(errs...)
	}}
}

// removeLayout removes the layout directory (slots and bundles) and the host
// runtime the agent installed. The runtime directory is removed only when it is
// empty afterwards, since it is shared by both slots and nothing else is
// expected there.
func (t *Teardown) removeLayout() []error {
	var errs []error

	if err := os.RemoveAll(t.Layout.RootfsDir("")); err != nil {
		errs = append(errs, err)
	}

	rt := t.RuntimePath
	if rt == "" {
		rt = container.DefaultRuntimePath
	}

	if err := os.Remove(rt); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}

	if err := os.Remove(t.runtimeDir()); err != nil && !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "directory not empty") {
		errs = append(errs, err)
	}

	return errs
}

func (t *Teardown) unmountShared(ctx context.Context) error {
	td := &noneops.Teardown{Log: t.logger(), MountInfo: t.MountInfo}
	if t.Run != nil {
		td.Run = none.Runner(t.Run)
	}

	return td.UnmountBelow(ctx, "/var/lib/kubelet")
}
