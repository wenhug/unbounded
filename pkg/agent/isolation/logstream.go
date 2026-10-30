// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"io"
	"os/exec"
	"strconv"
)

// JournalTailArgs returns the journalctl flags for the follow and line-count
// options. Each backend supplies the flags that select its units or machine,
// because they differ by mode.
func JournalTailArgs(opts LogOptions) []string {
	var args []string

	if opts.Follow {
		args = append(args, "--follow")
	}

	if opts.Lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(opts.Lines))
	}

	return args
}

// StartLogStream starts a command and returns its standard output. Closing the
// reader waits for the command, so a stream that was cut short does not leave a
// zombie.
func StartLogStream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
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
