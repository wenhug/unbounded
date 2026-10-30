// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// logsBackend wraps FakeBackend to return fixed log content and record options.
type logsBackend struct {
	*isolation.FakeBackend
	content string
	got     isolation.LogOptions
	err     error
}

func (l *logsBackend) Logs(_ context.Context, _ isolation.NodeRoot, o isolation.LogOptions) (io.ReadCloser, error) {
	l.got = o
	if l.err != nil {
		return nil, l.err
	}

	return io.NopCloser(strings.NewReader(l.content)), nil
}

func resolverFor(mode isolation.Mode, b isolation.Backend) accessResolver {
	return func(*slog.Logger) (*nodeAccess, error) {
		if b.Mode() != mode {
			return nil, errors.New("mode mismatch")
		}

		return &nodeAccess{Backend: b, Root: isolation.DirRoot{SlotName: "kube1"}}, nil
	}
}

func TestExecPerBackend(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			fb := &isolation.FakeBackend{FakeMode: mode, ExecFunc: func(isolation.ExecSpec) (isolation.ExecResult, error) {
				return isolation.ExecResult{Stdout: "ok"}, nil
			}}

			var out bytes.Buffer

			err := runExec(t.Context(), slog.Default(), resolverFor(mode, fb), &out, []string{"crictl", "ps"})
			require.NoError(t, err)
			require.Equal(t, "ok\n", out.String())
			require.Equal(t, []isolation.ExecSpec{{Args: []string{"crictl", "ps"}}}, fb.Execs())
			require.Equal(t, []string{"exec kube1"}, fb.Calls())
		})
	}
}

func TestExecErrors(t *testing.T) {
	t.Parallel()

	t.Run("no command", func(t *testing.T) {
		t.Parallel()

		err := runExec(t.Context(), slog.Default(), resolverFor(isolation.ModeNSpawn, &isolation.FakeBackend{}), io.Discard, nil)
		require.ErrorContains(t, err, "requires a command")
	})

	t.Run("resolver error", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("no applied config")
		err := runExec(t.Context(), slog.Default(), func(*slog.Logger) (*nodeAccess, error) { return nil, boom }, io.Discard, []string{"x"})
		require.ErrorIs(t, err, boom)
	})

	t.Run("backend error keeps output", func(t *testing.T) {
		t.Parallel()

		boom := errors.New("exit 1")
		fb := &isolation.FakeBackend{ExecFunc: func(isolation.ExecSpec) (isolation.ExecResult, error) {
			return isolation.ExecResult{Stdout: "partial"}, boom
		}}

		var out bytes.Buffer

		err := runExec(t.Context(), slog.Default(), resolverFor(isolation.ModeNSpawn, fb), &out, []string{"x"})
		require.ErrorIs(t, err, boom)
		require.Equal(t, "partial\n", out.String())
	})
}

func TestLogsPerBackend(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			lb := &logsBackend{FakeBackend: &isolation.FakeBackend{FakeMode: mode}, content: "line1\nline2\n"}
			opts := isolation.LogOptions{Unit: "kubelet", Follow: true, Lines: 5}

			var out bytes.Buffer

			require.NoError(t, runLogs(t.Context(), slog.Default(), resolverFor(mode, lb), &out, opts))
			require.Equal(t, "line1\nline2\n", out.String())
			require.Equal(t, opts, lb.got)
		})
	}
}

func TestLogsErrors(t *testing.T) {
	t.Parallel()

	lb := &logsBackend{FakeBackend: &isolation.FakeBackend{}, err: errors.New("journal unavailable")}
	require.ErrorContains(t, runLogs(t.Context(), slog.Default(), resolverFor(isolation.ModeNSpawn, lb), io.Discard, isolation.LogOptions{}), "journal unavailable")

	require.ErrorContains(t, runLogs(t.Context(), slog.Default(), resolverFor(isolation.ModeNSpawn, lb), io.Discard, isolation.LogOptions{Lines: -1}), "negative")

	boom := errors.New("no applied config")
	err := runLogs(t.Context(), slog.Default(), func(*slog.Logger) (*nodeAccess, error) { return nil, boom }, io.Discard, isolation.LogOptions{})
	require.ErrorIs(t, err, boom)
}

func TestBackendForMode(t *testing.T) {
	t.Parallel()

	b, err := backendForMode(isolation.ModeNSpawn, slog.Default())
	require.NoError(t, err)
	require.Equal(t, isolation.ModeNSpawn, b.Mode())

	b, err = backendForMode(isolation.ModeNone, slog.Default())
	require.NoError(t, err)
	require.Equal(t, isolation.ModeNone, b.Mode())

	_, err = backendForMode(isolation.Mode("microvm"), slog.Default())
	require.ErrorContains(t, err, "not supported by exec and logs")

	// container has no backend registered in this binary yet.
	if _, err := backendForMode(isolation.ModeContainer, slog.Default()); err != nil {
		require.ErrorContains(t, err, "not supported by exec and logs")
	}
}

func TestExecLogsRegistered(t *testing.T) {
	t.Parallel()

	cmdCtx := &CommandContext{LogFormat: "text"}
	require.Equal(t, "exec", newCmdExec(cmdCtx).Name())
	require.Equal(t, "logs", newCmdLogs(cmdCtx).Name())
	require.NotNil(t, newCmdLogs(cmdCtx).Flags().Lookup("follow"))
	require.NotNil(t, newCmdLogs(cmdCtx).Flags().Lookup("lines"))
	require.NotNil(t, newCmdLogs(cmdCtx).Flags().Lookup("unit"))
}
