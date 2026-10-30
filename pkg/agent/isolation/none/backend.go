// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Runner runs a host command and returns its stdout. It is a seam for tests.
type Runner func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error)

// Backend runs the node stack directly on the host.
type Backend struct {
	Log    *slog.Logger
	Layout Layout
	// SystemdDir is where units are written. Empty means SystemdDir.
	SystemdDir string
	// Host is used by preflight. The zero value is DefaultHostFS.
	Host *HostFS
	// Run executes host commands. Nil uses executil.OutputCmd.
	Run Runner
	// Units is the template input written by Prepare.
	Units UnitSpec
}

var _ isolation.Backend = (*Backend)(nil)

// Stack lists the agent-owned units in start order. Stop uses the reverse.
var stack = []string{UnitContainerd, UnitKubelet}

// Mode implements isolation.Backend.
func (*Backend) Mode() isolation.Mode { return isolation.ModeNone }

// Requirements implements isolation.Backend. /usr need not be writable.
func (b *Backend) Requirements() isolation.Requirements {
	return isolation.Requirements{
		HostCommands:  []string{"systemctl"},
		WritablePaths: []string{"/etc", b.Layout.base(), "/var"},
		After:         []string{"network-online.target"},
	}
}

// Preflight refuses to proceed when the host already has a kubelet or
// containerd the agent does not own, or a required path is read only. It
// mutates nothing.
func (b *Backend) Preflight() error {
	host := DefaultHostFS
	if b.Host != nil {
		host = *b.Host
	}

	var errs []error

	if findings := host.DetectUnmanaged(b.Layout.base()); len(findings) > 0 {
		for _, f := range findings {
			errs = append(errs, errors.New(f))
		}

		errs = append(errs, errors.New("use isolation mode nspawn or container, or remove the existing kubelet and containerd"))
	}

	if err := CheckWritable(b.Requirements().WritablePaths...); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
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

// root is the identity mapping: the node stack shares the host filesystem.
func root(version string) isolation.NodeRoot {
	return isolation.DirRoot{SlotName: version}
}

// Prepare installs the version directory and writes the agent-owned units. The
// artifacts themselves are placed in the returned layout's bin directory by the
// installers; the current link is not touched until Activate.
func (b *Backend) Prepare(ctx context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if _, err := b.Layout.Ensure(n.Slot); err != nil {
		return nil, err
	}

	units := b.Units
	if units.BinDir == "" {
		units.BinDir = b.Layout.BinDir()
	}

	if _, err := WriteUnits(b.unitDir(), units); err != nil {
		return nil, fmt.Errorf("write units: %w", err)
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		return nil, fmt.Errorf("daemon-reload: %w", err)
	}

	return root(n.Slot), nil
}

// Activate points the current link at the root's version and returns the
// version it replaced.
func (b *Backend) Activate(r isolation.NodeRoot) (string, error) {
	return b.Layout.Flip(r.Slot())
}

// Start starts the stack in order.
func (b *Backend) Start(ctx context.Context, _ isolation.NodeRoot) error {
	for _, u := range stack {
		if err := b.systemctl(ctx, "enable", "--now", u); err != nil {
			return fmt.Errorf("start %s: %w", u, err)
		}
	}

	return nil
}

// Stop stops the stack in reverse order. Every unit is attempted so a failure
// on one does not leave the others running.
func (b *Backend) Stop(ctx context.Context, _ isolation.NodeRoot) error {
	var errs []error

	for i := len(stack) - 1; i >= 0; i-- {
		if err := b.systemctl(ctx, "stop", stack[i]); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", stack[i], err))
		}
	}

	return errors.Join(errs...)
}

// Restart restarts containerd then kubelet. The host is never rebooted.
func (b *Backend) Restart(ctx context.Context, _ isolation.NodeRoot) error {
	for _, u := range stack {
		if err := b.systemctl(ctx, "restart", u); err != nil {
			return fmt.Errorf("restart %s: %w", u, err)
		}
	}

	return nil
}

// Destroy stops and disables the units, removes them and removes the version
// directory (and the whole base directory when it was the last version).
func (b *Backend) Destroy(ctx context.Context, r isolation.NodeRoot) error {
	var errs []error

	if err := b.Stop(ctx, r); err != nil {
		errs = append(errs, err)
	}

	for _, u := range stack {
		if err := b.systemctl(ctx, "disable", u); err != nil {
			errs = append(errs, fmt.Errorf("disable %s: %w", u, err))
		}

		if err := os.Remove(filepath.Join(b.unitDir(), u)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	if err := b.systemctl(ctx, "daemon-reload"); err != nil {
		errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
	}

	// The active version cannot be removed individually, so a destroy of the
	// whole node removes everything the agent installed.
	if err := b.Layout.RemoveAll(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// Status reports running when every unit is active.
func (b *Backend) Status(ctx context.Context, _ isolation.NodeRoot) (isolation.Status, error) {
	for _, u := range stack {
		if _, err := b.run(ctx, "systemctl", "is-active", "--quiet", u); err != nil {
			return isolation.Status{Detail: u + " is not active"}, nil //nolint:nilerr // an inactive unit is a status, not a failure
		}
	}

	return isolation.Status{Running: true}, nil
}

// Exec runs the command on the host, which is the node stack's environment.
func (b *Backend) Exec(ctx context.Context, _ isolation.NodeRoot, cmd isolation.ExecSpec) (isolation.ExecResult, error) {
	if len(cmd.Args) == 0 {
		return isolation.ExecResult{}, errors.New("exec: no command")
	}

	out, err := b.run(ctx, cmd.Args[0], cmd.Args[1:]...)

	return isolation.ExecResult{Stdout: out}, err
}

// Logs streams the journal for an agent-owned unit. With no unit it covers
// both.
func (*Backend) Logs(ctx context.Context, _ isolation.NodeRoot, opts isolation.LogOptions) (io.ReadCloser, error) {
	args := []string{"--no-pager"}

	if opts.Unit != "" {
		args = append(args, "--unit="+agentUnit(opts.Unit))
	} else {
		for _, u := range stack {
			args = append(args, "--unit="+u)
		}
	}

	if opts.Follow {
		args = append(args, "--follow")
	}

	if opts.Lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(opts.Lines))
	}

	cmd := exec.CommandContext(ctx, "journalctl", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &cmdReader{ReadCloser: stdout, cmd: cmd}, nil
}

// agentUnit maps the names users know to the agent-owned unit names.
func agentUnit(name string) string {
	switch name {
	case "kubelet", "kubelet.service":
		return UnitKubelet
	case "containerd", "containerd.service":
		return UnitContainerd
	default:
		return name
	}
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

// Upgrade switches to an installed version: stop the stack, flip the link,
// start the stack, and wait for ready. If any step after the flip fails the
// link is flipped back and the previous version is started again. The node is
// briefly down.
func (b *Backend) Upgrade(ctx context.Context, r isolation.NodeRoot, waitReady func(context.Context) error) error {
	if err := b.Stop(ctx, r); err != nil {
		// Whatever stopped is restarted so a failed stop does not leave the
		// node down; the version has not changed.
		return errors.Join(fmt.Errorf("stop before upgrade: %w", err), b.Start(context.WithoutCancel(ctx), r))
	}

	prev, err := b.Activate(r)
	if err != nil {
		return errors.Join(fmt.Errorf("activate %s: %w", r.Slot(), err), b.Start(context.WithoutCancel(ctx), r))
	}

	err = b.Start(ctx, r)
	if err == nil && waitReady != nil {
		err = waitReady(ctx)
	}

	if err == nil {
		return nil
	}

	err = fmt.Errorf("upgrade to %s: %w", r.Slot(), err)

	if prev == "" {
		return err
	}

	// Roll back with a context that survives the cancellation that may have
	// caused the failure.
	rb := context.WithoutCancel(ctx)

	var rbErrs []error

	if serr := b.Stop(rb, r); serr != nil {
		rbErrs = append(rbErrs, serr)
	}

	if _, ferr := b.Layout.Flip(prev); ferr != nil {
		rbErrs = append(rbErrs, fmt.Errorf("flip back to %s: %w", prev, ferr))
	}

	if serr := b.Start(rb, root(prev)); serr != nil {
		rbErrs = append(rbErrs, fmt.Errorf("start %s: %w", prev, serr))
	}

	if len(rbErrs) > 0 {
		return errors.Join(err, fmt.Errorf("rollback to %s failed: %w", prev, errors.Join(rbErrs...)))
	}

	return fmt.Errorf("%w (rolled back to %s)", err, prev)
}
