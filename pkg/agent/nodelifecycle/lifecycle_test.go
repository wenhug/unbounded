// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodelifecycle

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeController struct {
	calls      []string
	regenErr   error
	restartErr error
	// runningAfter is the number of Running calls that report not running.
	runningAfter int
	runningCalls int
	runningErr   error
}

func (f *fakeController) Regenerate(_ context.Context, slot string) error {
	f.calls = append(f.calls, "regenerate "+slot)
	return f.regenErr
}

func (f *fakeController) Running(_ context.Context, slot string) (bool, string, error) {
	f.calls = append(f.calls, "running "+slot)
	f.runningCalls++

	if f.runningErr != nil {
		return false, "", f.runningErr
	}

	if f.runningCalls > f.runningAfter {
		return true, "", nil
	}

	return false, "starting", nil
}

func (f *fakeController) Restart(_ context.Context, slot string) error {
	f.calls = append(f.calls, "restart "+slot)
	return f.restartErr
}

func newLifecycle(c Controller) *Lifecycle {
	return &Lifecycle{Log: slog.Default(), Controller: c, PollInterval: time.Millisecond, ReadyTimeout: 200 * time.Millisecond}
}

func TestPreStart(t *testing.T) {
	c := &fakeController{}
	reloaded := 0
	l := newLifecycle(c)
	l.ReloadSystemd = func(context.Context) error { reloaded++; return nil }

	require.NoError(t, l.PreStart(context.Background(), "kube1"))
	require.Equal(t, []string{"regenerate kube1"}, c.calls)
	require.Equal(t, 1, reloaded)
}

func TestPreStartFailures(t *testing.T) {
	c := &fakeController{regenErr: errors.New("bad spec")}
	l := newLifecycle(c)
	l.ReloadSystemd = func(context.Context) error { t.Fatal("must not reload after a failed regenerate"); return nil }
	require.ErrorContains(t, l.PreStart(context.Background(), "kube1"), "bad spec")

	l = newLifecycle(&fakeController{})
	l.ReloadSystemd = func(context.Context) error { return errors.New("dbus") }
	require.ErrorContains(t, l.PreStart(context.Background(), "kube1"), "dbus")

	l = newLifecycle(&fakeController{})
	require.NoError(t, l.PreStart(context.Background(), "kube1"), "reload is optional")
}

func TestPostStartWaitsUntilRunning(t *testing.T) {
	c := &fakeController{runningAfter: 3}
	require.NoError(t, newLifecycle(c).PostStart(context.Background(), "kube2"))
	require.Equal(t, 4, c.runningCalls)
}

func TestPostStartTimesOut(t *testing.T) {
	c := &fakeController{runningAfter: 1 << 30}
	l := newLifecycle(c)
	l.ReadyTimeout = 20 * time.Millisecond
	require.ErrorContains(t, l.PostStart(context.Background(), "kube1"), "starting")

	c = &fakeController{runningErr: errors.New("no runtime")}
	l = newLifecycle(c)
	l.ReadyTimeout = 20 * time.Millisecond
	require.ErrorContains(t, l.PostStart(context.Background(), "kube1"), "no runtime")
}

func TestPostStartHonorsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	l := newLifecycle(&fakeController{runningAfter: 1 << 30})
	require.Error(t, l.PostStart(ctx, "kube1"))
}

func TestReconcile(t *testing.T) {
	c := &fakeController{}
	require.NoError(t, newLifecycle(c).Reconcile(context.Background(), "kube1"))
	require.Equal(t, []string{"restart kube1"}, c.calls)

	c = &fakeController{restartErr: errors.New("nope")}
	require.ErrorContains(t, newLifecycle(c).Reconcile(context.Background(), "kube1"), "nope")
}

func TestValidate(t *testing.T) {
	require.Error(t, (&Lifecycle{Controller: &fakeController{}}).PreStart(context.Background(), "kube1"))
	require.Error(t, (&Lifecycle{Log: slog.Default()}).Reconcile(context.Background(), "kube1"))
}
