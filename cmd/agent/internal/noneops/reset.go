// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// gantryHostsMarker identifies the containerd hosts file the agent wrote for
// Gantry. A file without it belongs to someone else and is left alone.
const gantryHostsMarker = "# Managed by unbounded-agent for Gantry."

// Teardown removes a none node. Its tasks slot into the same positions in the
// reset order as the nspawn machine tasks they replace: stop, then config, then
// the node itself, with the host-global cleanup the caller owns in between and
// after.
type Teardown struct {
	Log *slog.Logger
	// Layout, SystemdDir and Run are as in Params.
	Layout     none.Layout
	SystemdDir string
	Run        none.Runner

	// Root prefixes every host path the teardown removes outside the layout and
	// systemd directory. Empty is the real host; tests use a temporary tree.
	Root string
	// MountInfo is the mount table. Empty means /proc/self/mountinfo.
	MountInfo string
	// Proc is the procfs scanned for container shims. Empty means /proc.
	Proc string
}

func (t *Teardown) logger() *slog.Logger { return hostnode.Logger(t.Log) }

func (t *Teardown) path(p string) string { return filepath.Join(t.Root, p) }

func (t *Teardown) backend() *none.Backend {
	proc := t.Proc
	if proc == "" {
		proc = "/proc"
	}

	return &none.Backend{Log: t.logger(), Layout: t.Layout, SystemdDir: t.SystemdDir, Run: t.Run, Proc: proc}
}

// Present reports whether the host has any trace of a none node: the layout
// directory or an agent-owned unit. Reset uses it to choose the none teardown
// even after the applied config is gone.
func (t *Teardown) Present() bool {
	if _, err := os.Lstat(filepath.Dir(t.Layout.CurrentLink())); err == nil {
		return true
	}

	dir := t.SystemdDir
	if dir == "" {
		dir = none.SystemdDir
	}

	for _, u := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS} {
		if _, err := os.Lstat(filepath.Join(dir, u)); err == nil {
			return true
		}
	}

	return false
}

type task struct {
	name string
	do   func(context.Context) error
}

func (t task) Name() string                 { return t.name }
func (t task) Do(ctx context.Context) error { return t.do(ctx) }

// Present is the package level form of Teardown.Present for the real host.
func Present() bool { return (&Teardown{}).Present() }

// Stop returns the task that stops the node stack's units.
func (t *Teardown) Stop() phases.Task {
	return task{"stop-none-node", func(ctx context.Context) error {
		b := t.backend()

		var errs []error

		// The units may be absent on a half installed node; Destroy tolerates
		// that, Stop must too.
		for _, u := range []string{none.UnitKubelet, none.UnitContainerd, none.UnitLocalDNS} {
			if _, err := os.Lstat(filepath.Join(unitDir(t.SystemdDir), u)); err != nil {
				continue
			}

			if _, err := b.Exec(ctx, nil, execSpec("systemctl", "stop", u)); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", u, err))
			}
		}

		return errors.Join(errs...)
	}}
}

// RemoveConfig returns the task that removes the configuration the agent wrote
// at the shared host paths.
func (t *Teardown) RemoveConfig() phases.Task {
	return task{"remove-none-config", func(context.Context) error {
		var errs []error

		files := []string{
			goalstates.ContainerdConfigPath,
			goalstates.NvidiaRuntimeDropInPath,
			goalstates.CDISpecFile,
			goalstates.KubeletAPIServerCACertPath,
			filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSSliceUnit),
			ModulesLoadPath,
		}
		for _, f := range files {
			if err := os.Remove(t.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}

		if err := t.removeGantryHosts(); err != nil {
			errs = append(errs, err)
		}

		if err := os.RemoveAll(t.path("/etc/unbounded/localdns")); err != nil {
			errs = append(errs, err)
		}

		// Directories the agent created and nothing else is expected in. Remove
		// only when empty so anything an operator or a workload put there stays.
		for _, d := range []string{goalstates.ContainerdConfDropInDir, goalstates.CDISpecDir, goalstates.ContainerdDefaultHostsDir, goalstates.KubeletStaticPodManifestsDir, goalstates.KubeletPKIDir} {
			if err := os.Remove(t.path(d)); err != nil && !errors.Is(err, os.ErrNotExist) && !IsNotEmpty(err) {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	}}
}

func (t *Teardown) removeGantryHosts() error {
	p := t.path(goalstates.ContainerdDefaultHostsPath)

	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if !bytes.Contains(data, []byte(gantryHostsMarker)) {
		return nil
	}

	return os.Remove(p)
}

// Destroy returns the task that removes the node: units, drop-ins and versions
// through the backend (which also ends container shims), then the node's data
// directories after unmounting whatever pods left mounted below them.
func (t *Teardown) Destroy() phases.Task {
	return task{"remove-none-node", func(ctx context.Context) error {
		errs := []error{t.backend().Destroy(ctx, none.Root{Layout: t.Layout})}

		errs = append(errs, t.removeData(ctx))

		return errors.Join(errs...)
	}}
}

// dataDirs are node state with no meaning once the node is gone. In the nspawn
// mode they live in the machine rootfs and disappear with it.
var dataDirs = []string{"/var/lib/kubelet", "/var/lib/containerd", goalstates.CNIConfigDir, goalstates.CNIBinDir}

func (t *Teardown) removeData(ctx context.Context) error {
	var errs []error

	if err := hostnode.UnmountBelow(ctx, t.logger(), t.Run, t.MountInfo, hostnode.KubeletDir, "/var/lib/containerd"); err != nil {
		errs = append(errs, err)
	}

	for _, d := range dataDirs {
		if err := os.RemoveAll(t.path(d)); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// IsNotEmpty reports whether err says a directory could not be removed because
// it still has entries.
func IsNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}

func unitDir(d string) string { return hostnode.OrDefault(d, none.SystemdDir) }
