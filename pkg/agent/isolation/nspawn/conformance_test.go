// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

// machineState stands in for the host: the lifecycle Ops flip it, and the
// systemd-run stub on PATH reads it, so Status and Exec see what Start did.
type machineState struct {
	mu       sync.Mutex
	dir      string
	prepared bool
	fail     map[isolationtest.Op]error
}

func (m *machineState) runningFile() string { return filepath.Join(m.dir, "running") }

func (m *machineState) take(op isolationtest.Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.fail[op]
	delete(m.fail, op)

	return err
}

func (m *machineState) setRunning(on bool) error {
	if !on {
		if err := os.Remove(m.runningFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	}

	return os.WriteFile(m.runningFile(), nil, 0o644)
}

func (m *machineState) ops() Ops {
	return Ops{
		Prepare: func(_ context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
			if err := m.take(isolationtest.OpPrepare); err != nil {
				return nil, err
			}

			m.mu.Lock()
			m.prepared = true
			m.mu.Unlock()

			return Root(n.Slot), nil
		},
		Start: func(_ context.Context, _ isolation.NodeRoot) error {
			if err := m.take(isolationtest.OpStart); err != nil {
				return err
			}

			m.mu.Lock()
			defer m.mu.Unlock()

			if !m.prepared {
				return errors.New("machine not prepared")
			}

			return m.setRunning(true)
		},
		Stop: func(_ context.Context, _ isolation.NodeRoot) error {
			if err := m.take(isolationtest.OpStop); err != nil {
				return err
			}

			return m.setRunning(false)
		},
		Restart: func(_ context.Context, _ isolation.NodeRoot) error {
			if err := m.take(isolationtest.OpRestart); err != nil {
				return err
			}

			return m.setRunning(true)
		},
		Destroy: func(_ context.Context, _ isolation.NodeRoot) error {
			m.mu.Lock()
			m.prepared = false
			m.mu.Unlock()

			return m.setRunning(false)
		},
	}
}

// TestConformance runs the shared suite with host commands stubbed on PATH.
// systemd-run succeeds only while the machine state says running, which is how
// a real machine rejects a command sent to a machine that is not up.
func TestConformance(t *testing.T) {
	isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
		m := &machineState{dir: t.TempDir(), fail: map[isolationtest.Op]error{}}

		bin := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bin, "systemd-run"), []byte(`#!/bin/sh
[ -e "`+m.runningFile()+`" ] || { echo "Failed to connect to machine" >&2; exit 1; }
shift 3
exec "$@"
`), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\necho log line\n"), 0o755))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		return &Backend{Ops: m.ops()}, isolationtest.Options{
			Spec:         isolation.NodeSpec{Slot: "kube1", Version: "v1.34.0"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			Fail: func(op isolationtest.Op, err error) {
				m.mu.Lock()
				m.fail[op] = err
				m.mu.Unlock()
			},
		}
	})
}

func TestUnwiredBackendFailsConformanceOperations(t *testing.T) {
	b := &Backend{}

	_, err := b.Prepare(t.Context(), isolation.NodeSpec{Slot: "kube1"})
	require.ErrorIs(t, err, ErrNotWired)
	require.ErrorIs(t, b.Start(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Stop(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Restart(t.Context(), Root("kube1")), ErrNotWired)
	require.ErrorIs(t, b.Destroy(t.Context(), Root("kube1")), ErrNotWired)
}
