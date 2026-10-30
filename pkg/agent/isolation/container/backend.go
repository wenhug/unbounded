// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package container is the isolation backend that runs the node stack in a
// privileged OCI container with systemd as PID 1. The agent unpacks the rootfs
// image, generates an OCI runtime bundle (config.json), and an agent-owned
// systemd unit supervises it with runc. The host's docker and containerd are
// not used.
//
// PROVISIONAL: the Azure Container Linux node facts in the design (section 13)
// have not been collected. The backend is exercised only against fake runtimes
// and stubbed hosts, never on a real node. Comments marked PROVISIONAL name
// assumptions that have not been verified there.
//
// The backend registers itself with the isolation registry. Its operations on
// a live host (unpack, start, repave, reset) are composed by
// cmd/agent/internal/containerops, which the daemon and bootstrap use.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// Runner runs a host command and returns its stdout. It is a seam for tests,
// shared with the none backend.
type Runner = none.Runner

// Streamer starts a long running host command and returns its stdout.
type Streamer func(ctx context.Context, name string, args ...string) (io.ReadCloser, error)

// Unpacker materializes the image rootfs into dir. It is injected because the
// OCI unpack code lives in the phase packages, which depend on this package.
type Unpacker func(ctx context.Context, n isolation.NodeSpec, dir string) error

// Backend runs the node stack in a privileged OCI container.
type Backend struct {
	Log    *slog.Logger
	Layout Layout
	// RuntimePath is the runc or crun executable. Empty means DefaultRuntimePath.
	RuntimePath string
	// HelperPath is the agent binary providing node-lifecycle hooks.
	HelperPath string
	// SystemdDir is where the unit is written. Empty means SystemdDir.
	SystemdDir string
	// Host is used by preflight and SELinux detection. Nil means DefaultHostFS.
	Host *HostFS
	// Run executes host commands. Nil uses executil.OutputCmd.
	Run Runner
	// Stream starts streaming commands (logs). Nil uses os/exec.
	Stream Streamer
	// Unpack materializes the rootfs. Nil requires the rootfs to exist already.
	Unpack Unpacker
	// StatDevice describes a granted device node. Nil stats the real device.
	StatDevice func(path string) (DeviceInfo, error)
	// MountInfoPath is read before removing a rootfs. Empty means /proc/self/mountinfo.
	MountInfoPath string
	// SourceRoot prefixes same-path source directories created by Prepare.
	// Empty means the real host root; tests use a temporary directory.
	SourceRoot string
	// WritableCheck overrides the writable path probe. Nil uses none.CheckWritable.
	WritableCheck func(paths ...string) error
	// RuntimeProvisioned tells Preflight that the agent installs the runtime
	// later in bootstrap, so a missing runtime file is expected on a clean host.
	// The runtime is still required to be executable when it is present.
	RuntimeProvisioned bool
	// RefreshGrants, when set, replaces the recorded grants at Regenerate. The
	// pre-start hook uses it to follow devices and driver libraries that a host
	// reboot or driver change moved since Prepare. It receives the grants
	// recorded by Prepare and returns the ones to render and record from now on.
	RefreshGrants func(ctx context.Context, recorded []isolation.Grant) ([]isolation.Grant, error)
	// UnitRequires lists host units the supervising unit requires and orders
	// after, such as the LocalDNS network unit.
	UnitRequires []string
}

var _ isolation.Backend = (*Backend)(nil)

func init() {
	// deps is a *Backend carrying the agent's wiring (layout, runner, unpacker).
	// Without it the registry returns a backend with production defaults, which
	// is what a caller that only needs Status, Exec or Logs wants.
	isolation.Register(isolation.ModeContainer, func(deps any) (isolation.Backend, error) {
		b := &Backend{Log: slog.Default()}
		if d, ok := deps.(*Backend); ok && d != nil {
			*b = *d
		}

		return b, nil
	})
}

// Mode implements isolation.Backend.
func (*Backend) Mode() isolation.Mode { return isolation.ModeContainer }

// Requirements implements isolation.Backend. The runtime artifact is installed
// under /opt/unbounded by the agent; cgroup v2 and the SELinux label are checked
// by Preflight rather than expressed here.
func (b *Backend) Requirements() isolation.Requirements {
	return isolation.Requirements{
		HostCommands:  []string{"systemctl", b.runtimePath()},
		WritablePaths: []string{"/etc", "/opt", "/var"},
		After:         []string{"network-online.target"},
	}
}

func (b *Backend) runtimePath() string {
	if b.RuntimePath == "" {
		return DefaultRuntimePath
	}

	return b.RuntimePath
}

func (b *Backend) host() HostFS {
	if b.Host != nil {
		return *b.Host
	}

	return DefaultHostFS
}

func (b *Backend) unitDir() string {
	if b.SystemdDir == "" {
		return SystemdDir
	}

	return b.SystemdDir
}

func (b *Backend) logger() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}

	return b.Log
}

func (b *Backend) checkWritable(paths ...string) error {
	if b.WritableCheck != nil {
		return b.WritableCheck(paths...)
	}

	return none.CheckWritable(paths...)
}

func (b *Backend) run(ctx context.Context, name string, args ...string) (string, error) {
	if b.Run != nil {
		return b.Run(ctx, b.logger(), name, args...)
	}

	return executil.OutputCmd(ctx, b.logger(), name, args...)
}

func (b *Backend) systemctl(ctx context.Context, args ...string) error {
	_, err := b.run(ctx, "systemctl", args...)
	return err
}

func (b *Backend) root(slot string) isolation.NodeRoot {
	return Root{SlotName: slot, Dir: b.Layout.RootfsDir(slot), HostRoot: b.SourceRoot}
}

// NodeRoot returns the node root of a slot without preparing it.
func (b *Backend) NodeRoot(slot string) (isolation.NodeRoot, error) {
	if err := ValidSlot(slot); err != nil {
		return nil, err
	}

	return b.root(slot), nil
}

func (b *Backend) statDevice(path string) (DeviceInfo, error) {
	if b.StatDevice != nil {
		return b.StatDevice(path)
	}

	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return DeviceInfo{}, err
	}

	typ := "c"

	switch st.Mode & unix.S_IFMT {
	case unix.S_IFBLK:
		typ = "b"
	case unix.S_IFCHR:
	default:
		return DeviceInfo{}, fmt.Errorf("%s is not a device node", path)
	}

	return DeviceInfo{Type: typ, Major: int64(unix.Major(uint64(st.Rdev))), Minor: int64(unix.Minor(uint64(st.Rdev))), Mode: uint32(st.Mode & 0o777)}, nil //nolint:gosec // device numbers and mode bits fit
}

// RenderBundle writes config.json for the slot from the grants, the current
// SELinux state and the devices on the host. It is idempotent and atomic.
func (b *Backend) RenderBundle(slot string, grants []isolation.Grant) error {
	in := BundleInput{
		RootfsDir: b.Layout.RootfsDir(slot),
		SELinux:   b.host().SELinuxMode(),
		Grants:    slices.Clone(grants),
		Devices:   map[string]DeviceInfo{},
	}

	for i, g := range grants {
		if g.Kind != isolation.GrantDevice || g.Source == "" {
			continue
		}

		// A device grant can name a directory of device nodes (for example an
		// NVIDIA device directory); there is no node to describe, so it is
		// exposed as a bind mount.
		if fi, err := os.Stat(filepath.Join(b.SourceRoot, g.Source)); err == nil && fi.IsDir() {
			in.Grants[i].Kind = isolation.GrantMount

			continue
		}

		info, err := b.statDevice(g.Source)
		if err != nil {
			return fmt.Errorf("device grant %s: %w", g.Source, err)
		}

		in.Devices[g.Source] = info
	}

	in.ExtraBinds = b.extraBinds()

	data, err := RenderConfig(in)
	if err != nil {
		return err
	}

	return fsutil.WriteFileAtomic(b.Layout.ConfigPath(slot), data, 0o644)
}

// extraBinds are the nspawn machine's other host binds. A bind whose source is
// absent is left out, because the runtime refuses a bundle with a missing
// source; the pre-start hook re-renders the bundle, so a source that appears
// later (staged offline archives) is picked up on the next start.
func (b *Backend) extraBinds() []ExtraBind {
	var out []ExtraBind

	for _, e := range []ExtraBind{
		{Source: ImageArchiveHostDir, Target: ImageArchiveDir, ReadOnly: true},
		{Source: KernelModulesDir, Target: KernelModulesDir, ReadOnly: true},
	} {
		if _, err := os.Stat(filepath.Join(b.SourceRoot, e.Source)); err != nil {
			continue
		}

		out = append(out, e)
	}

	return out
}

// Regenerate re-renders config.json from the NodeSpec recorded by Prepare. The
// node-lifecycle pre-start hook calls it so SELinux or device changes since the
// last start are picked up. When RefreshGrants is set the grants it returns
// replace the recorded ones, and are recorded, so the next start begins from
// them.
func (b *Backend) Regenerate(ctx context.Context, slot string) error {
	if err := ValidSlot(slot); err != nil {
		return err
	}

	data, err := os.ReadFile(b.Layout.SpecPath(slot))
	if err != nil {
		return fmt.Errorf("read recorded node spec: %w", err)
	}

	var n isolation.NodeSpec
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("decode recorded node spec: %w", err)
	}

	if b.RefreshGrants != nil {
		grants, err := b.RefreshGrants(ctx, n.Grants)
		if err != nil {
			return fmt.Errorf("refresh grants: %w", err)
		}

		if !reflect.DeepEqual(grants, n.Grants) {
			n.Grants = grants

			specData, err := json.Marshal(n)
			if err != nil {
				return err
			}

			if err := fsutil.WriteFileAtomic(b.Layout.SpecPath(slot), specData, 0o644); err != nil {
				return err
			}
		}
	}

	return b.RenderBundle(slot, n.Grants)
}

// Prepare materializes the slot: rootfs, same-path source directories,
// config.json, the recorded node spec and the supervising unit. It is safe to
// repeat.
func (b *Backend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if err := ValidSlot(n.Slot); err != nil {
		return nil, err
	}

	rootfs := b.Layout.RootfsDir(n.Slot)

	if b.Unpack != nil {
		if err := b.Unpack(ctx, n, rootfs); err != nil {
			return nil, fmt.Errorf("unpack rootfs: %w", err)
		}
	} else if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("rootfs %s is not present and no unpacker is configured", rootfs)
	}

	for _, s := range SamePathMounts {
		if err := os.MkdirAll(filepath.Join(b.SourceRoot, s.Path), 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", s.Path, err)
		}
	}

	specData, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}

	if err := fsutil.WriteFileAtomic(b.Layout.SpecPath(n.Slot), specData, 0o644); err != nil {
		return nil, err
	}

	if err := b.RenderBundle(n.Slot, n.Grants); err != nil {
		return nil, err
	}

	if err := b.writeUnit(n.Slot); err != nil {
		return nil, err
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return nil, fmt.Errorf("daemon-reload: %w", err)
	}

	return b.root(n.Slot), nil
}

func (b *Backend) writeUnit(slot string) error {
	unit := RenderUnit(UnitSpec{
		Slot:        slot,
		RuntimePath: b.runtimePath(),
		BundleDir:   b.Layout.BundleDir(slot),
		HelperPath:  b.HelperPath,
		After:       b.Requirements().After,
		Requires:    b.UnitRequires,
	})
	if err := fsutil.WriteFileAtomic(filepath.Join(b.unitDir(), UnitName(slot)), unit, 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	return nil
}

// RewriteUnit re-renders the supervising unit of a prepared slot and reloads
// systemd, without restarting a running container. It is how a moved helper
// path reaches the hooks of an existing unit.
func (b *Backend) RewriteUnit(ctx context.Context, slot string) error {
	if err := ValidSlot(slot); err != nil {
		return err
	}

	if err := b.writeUnit(slot); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}

	return nil
}

// Start enables and starts the unit. Starting a running unit is a no-op.
func (b *Backend) Start(ctx context.Context, r isolation.NodeRoot) error {
	if err := ValidSlot(r.Slot()); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "enable", "--now", UnitName(r.Slot())); err != nil {
		return fmt.Errorf("start %s: %w", UnitName(r.Slot()), err)
	}

	return nil
}

// Stop stops the unit and removes any stale runtime state. Stopping a stopped
// container succeeds.
func (b *Backend) Stop(ctx context.Context, r isolation.NodeRoot) error {
	if err := ValidSlot(r.Slot()); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "stop", UnitName(r.Slot())); err != nil {
		return fmt.Errorf("stop %s: %w", UnitName(r.Slot()), err)
	}

	b.deleteContainer(ctx, r.Slot())

	return nil
}

// deleteContainer removes leftover runtime state. A container that does not
// exist is the normal case, so failure is only logged.
func (b *Backend) deleteContainer(ctx context.Context, slot string) {
	if _, err := b.run(ctx, b.runtimePath(), "delete", "--force", ContainerID(slot)); err != nil {
		b.logger().Debug("runtime delete (best effort)", "slot", slot, "error", err)
	}
}

// Restart restarts the container only, never the host.
func (b *Backend) Restart(ctx context.Context, r isolation.NodeRoot) error {
	if err := ValidSlot(r.Slot()); err != nil {
		return err
	}

	if err := b.systemctl(ctx, "restart", UnitName(r.Slot())); err != nil {
		return fmt.Errorf("restart %s: %w", UnitName(r.Slot()), err)
	}

	return nil
}

// SlotExists reports whether the slot has a rootfs or a supervising unit on the
// host, even a half built one.
func (b *Backend) SlotExists(slot string) bool {
	if ValidSlot(slot) != nil {
		return false
	}

	for _, p := range []string{b.Layout.RootfsDir(slot), filepath.Join(b.unitDir(), UnitName(slot))} {
		if _, err := os.Lstat(p); err == nil {
			return true
		}
	}

	return false
}

// Destroy stops and removes the unit, then removes the bundle and rootfs. Every
// step is attempted so a failure on one does not strand the others. The rootfs
// is not removed while anything is still mounted below it, since removing a
// live bind mount would delete host data.
func (b *Backend) Destroy(ctx context.Context, r isolation.NodeRoot) error {
	slot := r.Slot()
	if err := ValidSlot(slot); err != nil {
		return err
	}

	var errs []error

	// A slot whose unit was never written (an interrupted Prepare) or is
	// already gone has nothing for systemd to stop; asking would fail with
	// "not loaded" and hide the cleanup that follows.
	unitPath := filepath.Join(b.unitDir(), UnitName(slot))

	if _, err := os.Lstat(unitPath); err == nil {
		if err := b.Stop(ctx, r); err != nil {
			errs = append(errs, err)
		}

		if err := b.systemctl(ctx, "disable", UnitName(slot)); err != nil {
			errs = append(errs, fmt.Errorf("disable %s: %w", UnitName(slot), err))
		}

		if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}

		if err := b.systemctl(ctx, "daemon-reload"); err != nil {
			errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
		}
	} else {
		b.deleteContainer(ctx, slot)
	}

	if err := b.removeTree(b.Layout.BundleDir(slot)); err != nil {
		errs = append(errs, err)
	}

	if err := b.removeRootfs(slot); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (b *Backend) removeTree(dir string) error {
	return os.RemoveAll(dir)
}

func (b *Backend) removeRootfs(slot string) error {
	dir := b.Layout.RootfsDir(slot)

	path := b.MountInfoPath
	if path == "" {
		path = "/proc/self/mountinfo"
	}

	mounted, err := mountsBelow(path, dir)
	if err != nil {
		return fmt.Errorf("refusing to remove %s: %w", dir, err)
	}

	if len(mounted) > 0 {
		return fmt.Errorf("refusing to remove %s: still mounted: %s", dir, strings.Join(mounted, ", "))
	}

	return os.RemoveAll(dir)
}

// mountsBelow returns mount points at or below dir from a mountinfo file.
func mountsBelow(mountinfo, dir string) ([]string, error) {
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return nil, err
	}

	var out []string

	prefix := dir + string(filepath.Separator)

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		mp := strings.ReplaceAll(fields[4], `\040`, " ")
		if mp == dir || strings.HasPrefix(mp, prefix) {
			out = append(out, mp)
		}
	}

	return out, nil
}

// runtimeState is the subset of `runtime state` output used by Status.
type runtimeState struct {
	Status string `json:"status"`
}

// Status reports running when the unit is active and the runtime reports the
// container running.
func (b *Backend) Status(ctx context.Context, r isolation.NodeRoot) (isolation.Status, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.Status{}, err
	}

	if _, err := b.run(ctx, "systemctl", "is-active", "--quiet", UnitName(r.Slot())); err != nil {
		return isolation.Status{Detail: UnitName(r.Slot()) + " is not active"}, nil //nolint:nilerr // an inactive unit is a status, not a failure
	}

	return b.RuntimeStatus(ctx, r)
}

// RuntimeStatus reports whether the OCI runtime considers the container
// running, without requiring the supervising unit to be active. The
// node-lifecycle post-start hook needs this form: it runs as ExecStartPost, when
// the unit is still activating and `systemctl is-active` fails, so Status would
// never report running from inside the hook. PROVISIONAL: the activating state
// during ExecStartPost is systemd's documented behavior, not observed here.
func (b *Backend) RuntimeStatus(ctx context.Context, r isolation.NodeRoot) (isolation.Status, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.Status{}, err
	}

	out, err := b.run(ctx, b.runtimePath(), "state", ContainerID(r.Slot()))
	if err != nil {
		return isolation.Status{Detail: "runtime state: " + err.Error()}, nil //nolint:nilerr // an unreachable container is a status, not a failure
	}

	var st runtimeState
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return isolation.Status{Detail: "runtime state: unparsable output"}, nil //nolint:nilerr // reported in Detail
	}

	if st.Status != "running" {
		return isolation.Status{Detail: "container is " + st.Status}, nil
	}

	return isolation.Status{Running: true}, nil
}

// Exec runs a command in the container via `<runtime> exec`.
func (b *Backend) Exec(ctx context.Context, r isolation.NodeRoot, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return isolation.ExecResult{}, err
	}

	if len(cmd.Args) == 0 {
		return isolation.ExecResult{}, errors.New("exec: no command")
	}

	args := append([]string{"exec", ContainerID(r.Slot())}, cmd.Args...)
	out, err := b.run(ctx, b.runtimePath(), args...)

	return isolation.ExecResult{Stdout: out}, err
}

// Logs streams the container journal by running journalctl in the container.
func (b *Backend) Logs(ctx context.Context, r isolation.NodeRoot, opts isolation.LogOptions) (io.ReadCloser, error) {
	if err := ValidSlot(r.Slot()); err != nil {
		return nil, err
	}

	args := []string{"exec", ContainerID(r.Slot()), "journalctl", "--no-pager"}
	if opts.Unit != "" {
		args = append(args, "--unit="+opts.Unit)
	}

	if opts.Follow {
		args = append(args, "--follow")
	}

	if opts.Lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(opts.Lines))
	}

	if b.Stream != nil {
		return b.Stream(ctx, b.runtimePath(), args...)
	}

	return startStream(ctx, b.runtimePath(), args...)
}

func startStream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &cmdReader{ReadCloser: stdout, cmd: cmd}, nil
}

type cmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReader) Close() error {
	err := c.ReadCloser.Close()
	_ = c.cmd.Wait() //nolint:errcheck // exit status of a closed log stream is not meaningful

	return err
}
