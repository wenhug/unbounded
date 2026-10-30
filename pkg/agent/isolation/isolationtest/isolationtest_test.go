// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolationtest

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// memBackend is a minimal correct backend: it proves the suite can pass and
// that a runner without optional hooks only skips the cases needing them.
type memBackend struct {
	isolation.FakeBackend

	mu      sync.Mutex
	running bool
	fail    map[Op]error
}

func (m *memBackend) take(op Op) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.fail[op]
	delete(m.fail, op)

	return err
}

func (m *memBackend) set(on bool) {
	m.mu.Lock()
	m.running = on
	m.mu.Unlock()
}

func (m *memBackend) Prepare(_ context.Context, n isolation.NodeSpec) (isolation.NodeRoot, error) {
	if err := m.take(OpPrepare); err != nil {
		return nil, err
	}

	return isolation.DirRoot{SlotName: n.Slot, Dir: "/root/" + n.Slot}, nil
}

func (m *memBackend) Start(context.Context, isolation.NodeRoot) error {
	if err := m.take(OpStart); err != nil {
		return err
	}

	m.set(true)

	return nil
}

func (m *memBackend) Stop(context.Context, isolation.NodeRoot) error {
	if err := m.take(OpStop); err != nil {
		return err
	}

	m.set(false)

	return nil
}

func (m *memBackend) Restart(context.Context, isolation.NodeRoot) error {
	if err := m.take(OpRestart); err != nil {
		return err
	}

	m.set(true)

	return nil
}

func (m *memBackend) Destroy(context.Context, isolation.NodeRoot) error {
	m.set(false)
	return nil
}

func (m *memBackend) Status(context.Context, isolation.NodeRoot) (isolation.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return isolation.Status{Running: m.running}, nil
}

func (m *memBackend) Exec(_ context.Context, _ isolation.NodeRoot, c isolation.ExecSpec) (isolation.ExecResult, error) {
	if len(c.Args) > 0 && c.Args[0] == "false" {
		return isolation.ExecResult{}, io.ErrUnexpectedEOF
	}

	return isolation.ExecResult{Stdout: strings.Join(c.Args[1:], " ") + "\n"}, nil
}

func TestSuiteRunsAgainstCorrectBackend(t *testing.T) {
	Run(t, func(*testing.T) (isolation.Backend, Options) {
		m := &memBackend{fail: map[Op]error{}}

		return m, Options{
			Spec:         isolation.NodeSpec{Slot: "kube1"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			Fail: func(op Op, err error) {
				m.mu.Lock()
				m.fail[op] = err
				m.mu.Unlock()
			},
		}
	})
}

func TestSuiteSkipsCasesWithoutOptionalHooks(t *testing.T) {
	Run(t, func(*testing.T) (isolation.Backend, Options) {
		return &memBackend{fail: map[Op]error{}}, Options{Spec: isolation.NodeSpec{Slot: "kube1"}, SkipLogs: true}
	})
}
