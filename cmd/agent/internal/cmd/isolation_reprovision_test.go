// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

type reprovisionHarness struct {
	events []string
	store  migrate.JournalStore
	runner *reprovisionRunner

	resetErrs []error
	startErr  error
}

func newReprovisionHarness(t *testing.T, step migrate.Step) *reprovisionHarness {
	t.Helper()

	h := &reprovisionHarness{store: migrate.JournalStore{Path: filepath.Join(t.TempDir(), "journal.json")}}

	require.NoError(t, h.store.Save(migrate.Journal{
		Operation: "op-1", Strategy: migrate.Reprovision,
		From: isolation.ModeNSpawn, To: isolation.ModeNone, Step: step,
	}))

	h.runner = &reprovisionRunner{
		log: slog.New(slog.DiscardHandler),
		reset: func(context.Context) error {
			h.events = append(h.events, "reset")

			if len(h.resetErrs) == 0 {
				return nil
			}

			err := h.resetErrs[0]
			h.resetErrs = h.resetErrs[1:]

			return err
		},
		start: func(context.Context) error {
			h.events = append(h.events, "start")
			return h.startErr
		},
		sleep: func(time.Duration) { h.events = append(h.events, "sleep") },
	}

	return h
}

func (h *reprovisionHarness) step(t *testing.T) migrate.Step {
	t.Helper()

	j, err := h.store.Load()
	require.NoError(t, err)

	return j.Step
}

func TestReprovisionRunsResetThenBootstrap(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)

	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"reset", "start"}, h.events)
	assert.Equal(t, migrate.StepBootstrapped, h.step(t))
}

func TestReprovisionWaitsForTheDaemonToReleaseOwnership(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.resetErrs = []error{installstate.ErrLockHeld, fmt.Errorf("wrapped: %w", installstate.ErrLockHeld)}

	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"reset", "sleep", "reset", "sleep", "reset", "start"}, h.events)
}

func TestReprovisionResetFailureIsRecordedAndRetriedByTheUnit(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.resetErrs = []error{errors.New("cannot unmount")}

	err := h.runner.run(t.Context(), h.store)
	require.ErrorContains(t, err, "cannot unmount")

	assert.Equal(t, []string{"reset"}, h.events, "bootstrap does not run on a host that did not reset")
	assert.Equal(t, migrate.StepReprovisioning, h.step(t), "the next attempt resets again")

	j, _ := h.store.Load() //nolint:errcheck // saved above
	assert.Contains(t, j.Message, "cannot unmount")

	// systemd runs the unit again.
	h.events = nil
	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Equal(t, []string{"reset", "start"}, h.events)
}

func TestReprovisionBootstrapFailureResumesWithoutResettingAgain(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.startErr = errors.New("kubelet did not join")

	err := h.runner.run(t.Context(), h.store)
	require.ErrorContains(t, err, "kubelet did not join")
	require.ErrorContains(t, err, "mode none")
	assert.Equal(t, migrate.StepBootstrapping, h.step(t))

	h.events, h.startErr = nil, nil
	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Equal(t, []string{"start"}, h.events, "the reset is not repeated; bootstrap decides from the host")
	assert.Equal(t, migrate.StepBootstrapped, h.step(t))
}

func TestReprovisionAlreadyFinishedDoesNothing(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepBootstrapped)

	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Empty(t, h.events)
}

func TestReprovisionRefusesAJournalItDoesNotRunFrom(t *testing.T) {
	for _, step := range []migrate.Step{migrate.StepLegacyDraining, migrate.StepReady, migrate.StepRolledBack} {
		h := newReprovisionHarness(t, step)

		require.ErrorContains(t, h.runner.run(t.Context(), h.store), "does not run from")
		assert.Empty(t, h.events, "nothing may be reset for a journal that is not a handoff")
	}
}

func TestReprovisionNeedsAJournal(t *testing.T) {
	r := &reprovisionRunner{log: slog.New(slog.DiscardHandler)}

	require.ErrorContains(t, r.run(t.Context(), migrate.JournalStore{Path: filepath.Join(t.TempDir(), "missing.json")}), "journal")
}

func TestCheckStagedConfig(t *testing.T) {
	good := `{"MachineName":"m1","NodeName":"n1","Isolation":"%s",
"Cluster":{"CaCertBase64":"dGVzdA==","ClusterDNS":"10.0.0.10","Version":"1.33.1"},
"Kubelet":{"ApiServer":"https://api.example.com:6443","Auth":{"BootstrapToken":"abc123.xyz789"}}}`

	for _, mode := range []string{"nspawn", "none", "container"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, good, mode), 0o600))
			t.Setenv(configFileEnv, path)

			require.NoError(t, checkStagedConfig(slog.New(slog.DiscardHandler)))
		})
	}

	t.Run("unknown mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, good, "microvm"), 0o600))
		t.Setenv(configFileEnv, path)

		require.ErrorContains(t, checkStagedConfig(slog.New(slog.DiscardHandler)), "Isolation")
	})

	t.Run("missing file", func(t *testing.T) {
		t.Setenv(configFileEnv, filepath.Join(t.TempDir(), "absent.json"))

		require.Error(t, checkStagedConfig(slog.New(slog.DiscardHandler)))
	})
}

func TestIsolationReprovisionIsHiddenAndRegistered(t *testing.T) {
	c := newCmdIsolationReprovision(&CommandContext{})

	assert.True(t, c.Hidden)
	assert.NotNil(t, c.Flags().Lookup("handoff-dir"))
	assert.NotNil(t, c.Flags().Lookup("check"))
}
