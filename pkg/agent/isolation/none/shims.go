// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// shimComm is the process name of the per-pod containerd shim. containerd runs
// with KillMode=process so that pods survive a containerd restart, which also
// means they survive stopping the unit. Tearing the node down has to end them.
// The name is truncated to 15 characters in /proc/<pid>/comm.
const shimComm = "containerd-shim"

// killShims sends SIGKILL to every containerd shim visible under proc. The
// preflight guarantees the host had no containerd of its own, so every shim
// belongs to the node stack the agent installed. A process that exits between
// the scan and the signal is not an error.
func killShims(proc string) error {
	entries, err := os.ReadDir(proc)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("scan processes: %w", err)
	}

	var errs []error

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		comm, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm"))
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(comm)), shimComm) {
			continue
		}

		if err := killProcess(pid); err != nil {
			errs = append(errs, fmt.Errorf("kill containerd shim %d: %w", pid, err))
		}
	}

	return errors.Join(errs...)
}

// killProcess is a seam so tests never signal a real process.
var killProcess = func(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}
