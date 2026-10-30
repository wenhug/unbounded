// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nspawn is the isolation backend that runs the node stack in a
// systemd-nspawn machine. It owns every use of machinectl and
// systemd-run --machine so the rest of the agent stays backend neutral.
package nspawn

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strconv"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// MachinesDir is where systemd-nspawn machine root filesystems live.
const MachinesDir = "/var/lib/machines"

// Root returns the NodeRoot for the machine slot, rooted at its directory
// under MachinesDir.
func Root(slot string) isolation.DirRoot {
	return RootAt(slot, MachinesDir+"/"+slot)
}

// RootAt returns the NodeRoot for slot rooted at dir.
func RootAt(slot, dir string) isolation.DirRoot {
	return isolation.DirRoot{SlotName: slot, Dir: dir}
}

// Exec runs args inside the machine named by slot using
// systemd-run --machine=<slot> --pipe --wait. Stdout is streamed at Debug and
// stderr at Error, and the captured stdout is returned.
func Exec(ctx context.Context, log *slog.Logger, slot string, args ...string) (string, error) {
	return executil.MachineRun(ctx, log, slot, args...)
}

// Logs returns the machine journal for the slot, via journalctl --machine.
func Logs(ctx context.Context, slot string, opts isolation.LogOptions) (io.ReadCloser, error) {
	args := []string{"--machine=" + slot, "--no-pager"}
	if opts.Unit != "" {
		args = append(args, "--unit="+opts.Unit)
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

type cmdReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReader) Close() error {
	err := c.ReadCloser.Close()
	_ = c.cmd.Wait() //nolint:errcheck // exit status of a closed log stream is not meaningful

	return err
}
