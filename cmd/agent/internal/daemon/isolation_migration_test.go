// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// migHarness replaces every host-touching seam of a migration with a recorder.
type migHarness struct {
	t      *testing.T
	events []string

	source, target *isolation.FakeBackend
	applied        map[string]*provision.AgentConfig
	failAt         map[string]error
	active         *ActiveMachine
	dir            string
}

const testNode = "node-1"

func newMigHarness(t *testing.T, from isolation.Mode) *migHarness {
	t.Helper()

	h := &migHarness{
		t:       t,
		applied: map[string]*provision.AgentConfig{},
		failAt:  map[string]error{},
		dir:     t.TempDir(),
	}

	h.source = &isolation.FakeBackend{FakeMode: from}
	h.target = &isolation.FakeBackend{FakeMode: isolation.ModeContainer}

	cfg := baseConfig()
	cfg.Isolation = string(from)
	cfg.NodeName = testNode
	cfg.Kubelet.Labels = map[string]string{"role": "gpu"}
	h.active = &ActiveMachine{Name: "kube1", Config: cfg, Mode: from}
	h.applied["kube1"] = cfg

	h.replace(&inPlaceJournalPath, func() string { return filepath.Join(h.dir, "isolation-migration.json") })
	h.replace(&reprovisionDir, func() string { return filepath.Join(h.dir, "reprovision") })
	h.replace(&reprovisionUnitDir, func() string { return filepath.Join(h.dir, "units") })
	h.replace(&reprovisionJournalOf, func() string { return filepath.Join(h.dir, "reprovision", ReprovisionJournalName) })

	h.replace(&newMigrationBackend, func(_ *slog.Logger, e migrationEndpoint) (isolation.Backend, isolation.NodeRoot, error) {
		root := isolation.DirRoot{SlotName: e.Slot}
		if e.Source {
			return h.source, root, nil
		}

		h.target.FakeMode = e.Mode

		return h.target, root, nil
	})
	h.replace(&resolveMigrationDownloads, func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error) {
		return nil, nil, h.failAt["downloads"]
	})
	h.replace(&stageMigrationArchives, func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		h.events = append(h.events, "download")
		return h.failAt["download"]
	})
	h.replace(&cleanupMigrationNetwork, func(context.Context, *slog.Logger) error {
		h.events = append(h.events, "cleanup-network")
		return h.failAt["cleanup-network"]
	})
	h.replace(&persistMigrationConfig, func(_ context.Context, _ *slog.Logger, slot string, cfg *provision.AgentConfig) error {
		h.events = append(h.events, "persist "+slot)
		if err := h.failAt["persist "+slot]; err != nil {
			return err
		}

		h.applied[slot] = cfg

		return nil
	})
	h.replace(&removeMigrationConfig, func(_ context.Context, _ *slog.Logger, slot string) error {
		h.events = append(h.events, "remove-config "+slot)
		delete(h.applied, slot)

		return h.failAt["remove-config "+slot]
	})
	h.replace(&waitKubeletOnHost, func(_ context.Context, _ *slog.Logger, _ isolation.Mode, slot string) error {
		h.events = append(h.events, "kubelet-on-host "+slot)
		return h.failAt["kubelet-on-host"]
	})
	h.replace(&preflightMigrationTarget, func(_ context.Context, _ *slog.Logger, mode isolation.Mode, _ *provision.AgentConfig) error {
		h.events = append(h.events, "preflight "+string(mode))
		return h.failAt["preflight"]
	})
	h.replace(&readMigrationConfig, func(_ *slog.Logger, slot string) (*ActiveMachine, error) {
		if err := h.failAt["read "+slot]; err != nil {
			return nil, err
		}

		cfg, ok := h.applied[slot]
		if !ok {
			return nil, os.ErrNotExist
		}

		mode, err := cfg.IsolationMode()
		if err != nil {
			return nil, err
		}

		return &ActiveMachine{Name: slot, Config: cfg, Mode: mode}, nil
	})
	h.replace(&systemctlForMigration, func(_ context.Context, _ *slog.Logger, args ...string) error {
		h.events = append(h.events, "systemctl "+strings.Join(args, " "))
		return h.failAt["systemctl"]
	})
	h.replace(&runHandoffCommand, func(_ context.Context, bin string, args ...string) error {
		h.events = append(h.events, "handoff "+filepath.Base(bin)+" "+strings.Join(args[:2], " "))
		return h.failAt["handoff"]
	})
	h.replace(&agentExecutable, func() (string, error) {
		path := filepath.Join(h.dir, "agent-binary")
		return path, os.WriteFile(path, []byte("binary"), 0o755)
	})

	return h
}

func (h *migHarness) replace(target any, value any) {
	h.t.Helper()

	switch p := target.(type) {
	case *func() string:
		old := *p
		*p = value.(func() string)

		h.t.Cleanup(func() { *p = old })
	case *func(*slog.Logger, migrationEndpoint) (isolation.Backend, isolation.NodeRoot, error):
		old := *p
		*p = value.(func(*slog.Logger, migrationEndpoint) (isolation.Backend, isolation.NodeRoot, error))

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error):
		old := *p
		*p = value.(func(context.Context, *provision.UnboundedAgentConfig) (*goalstates.DownloadOverrides, *goalstates.ContainerImageArchiveStaging, error))

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, string, *provision.AgentConfig) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, string, *provision.AgentConfig) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, string) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, string) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, isolation.Mode, string) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, isolation.Mode, string) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, isolation.Mode, *provision.AgentConfig) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, isolation.Mode, *provision.AgentConfig) error)

		h.t.Cleanup(func() { *p = old })
	case *func(*slog.Logger, string) (*ActiveMachine, error):
		old := *p
		*p = value.(func(*slog.Logger, string) (*ActiveMachine, error))

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, *slog.Logger, ...string) error:
		old := *p
		*p = value.(func(context.Context, *slog.Logger, ...string) error)

		h.t.Cleanup(func() { *p = old })
	case *func(context.Context, string, ...string) error:
		old := *p
		*p = value.(func(context.Context, string, ...string) error)

		h.t.Cleanup(func() { *p = old })
	case *func() (string, error):
		old := *p
		*p = value.(func() (string, error))

		h.t.Cleanup(func() { *p = old })
	default:
		h.t.Fatalf("unsupported seam type %T", target)
	}
}

func (h *migHarness) env() migrationEnv {
	return migrationEnv{Operation: "op-1"}
}

func (h *migHarness) run(target isolation.Mode, strategy migrate.Strategy) error {
	return migrateIsolation(context.Background(), discardLogger(), h.active, migrate.Request{Target: target, Strategy: strategy}, strategy, h.env())
}

func (h *migHarness) journal() (migrate.Journal, error) {
	return inPlaceJournalStore().Load()
}

// ---------------------------------------------------------------------------
// In-place: nspawn <-> container
// ---------------------------------------------------------------------------

func TestMigrateInPlace_BothDirections(t *testing.T) {
	for _, tt := range []struct{ from, to isolation.Mode }{
		{isolation.ModeNSpawn, isolation.ModeContainer},
		{isolation.ModeContainer, isolation.ModeNSpawn},
	} {
		t.Run(string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			h := newMigHarness(t, tt.from)

			require.NoError(t, h.run(tt.to, migrate.InPlace))

			assert.Equal(t, []string{
				"preflight " + string(tt.to),
				"download",
				"cleanup-network",
				"persist kube2",
				"kubelet-on-host kube2",
				"remove-config kube1",
			}, h.events)

			// Target: leftovers removed, built in the alternate slot, started.
			assert.Equal(t, []string{"destroy kube2", "prepare kube2", "start kube2"}, h.target.Calls())
			// Source: stopped, kept until Ready, then destroyed.
			assert.Equal(t, []string{"stop kube1", "destroy kube1"}, h.source.Calls())

			// The applied config moved to the new slot with the new mode, and
			// kept the node identity.
			require.NotContains(t, h.applied, "kube1")
			require.Contains(t, h.applied, "kube2")
			assert.Equal(t, string(tt.to), h.applied["kube2"].Isolation)
			assert.Equal(t, testNode, h.applied["kube2"].NodeName)
			assert.Equal(t, map[string]string{"role": "gpu"}, h.applied["kube2"].Kubelet.Labels)

			j, err := h.journal()
			require.NoError(t, err)
			assert.Equal(t, migrate.StepComplete, j.Step)
		})
	}
}

func TestMigrateInPlace_FromSlotTwoGoesToSlotOne(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.active.Name = "kube2"
	h.applied = map[string]*provision.AgentConfig{"kube2": h.active.Config}

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))

	assert.Equal(t, []string{"destroy kube1", "prepare kube1", "start kube1"}, h.target.Calls())
	assert.Contains(t, h.applied, "kube1")
	assert.NotContains(t, h.applied, "kube2")
}

func TestMigrateInPlace_PreflightFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["preflight"] = errors.New("no runtime")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "no runtime")

	assert.Equal(t, []string{"preflight container"}, h.events, "no download, no config change")
	assert.Empty(t, h.source.Calls())
	assert.Empty(t, h.target.Calls())

	_, jerr := h.journal()
	assert.ErrorIs(t, jerr, migrate.ErrNoJournal, "nothing was changed, so nothing is journaled")
}

func TestMigrateInPlace_DownloadFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["download"] = errors.New("offline")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "offline")
	assert.Empty(t, h.source.Calls())
	assert.Empty(t, h.target.Calls())
}

func TestMigrateInPlace_ResolveDownloadsFailureChangesNothing(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["downloads"] = errors.New("registry unreachable")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "registry unreachable")
	assert.Empty(t, h.events)
}

func TestMigrateInPlace_RefusesWhenAlternateSlotIsRecordedInUse(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.applied["kube2"] = h.active.Config

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "still recorded as in use")
	assert.Empty(t, h.events)
}

func TestMigrateInPlace_AnUnreadableAlternateRecordIsOverwrittenAndRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["read kube2"] = errors.New("is a directory")
	h.failAt["persist kube2"] = errors.New("is a directory")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "rolled back to nspawn")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_PrepareFailureLeavesTheSourceRunning(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.PrepareErr = errors.New("unpack failed")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "unpack failed")
	assert.Empty(t, h.source.Calls())
	assert.NotContains(t, h.events, "cleanup-network", "the source is not touched before the target is built")
}

func TestMigrateInPlace_TargetStartFailureRollsBackToTheSource(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.StartErr = errors.New("container did not start")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "container did not start")
	require.ErrorContains(t, err, "rolled back to nspawn")

	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls(), "the source is restarted and never destroyed")
	assert.Equal(t, []string{"destroy kube2", "prepare kube2", "start kube2", "stop kube2", "destroy kube2"}, h.target.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.Contains(t, h.applied, "kube1")

	j, jerr := h.journal()
	require.NoError(t, jerr)
	assert.Equal(t, migrate.StepRolledBack, j.Step)
}

func TestMigrateInPlace_PersistFailureRestoresTheSourceConfig(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["persist kube2"] = errors.New("disk full")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "disk full")

	assert.Contains(t, h.events, "remove-config kube2")
	assert.Contains(t, h.events, "persist kube1", "the source's applied config is written back")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_ReadinessFailureRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["kubelet-on-host"] = errors.New("kubelet inactive")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "wait for the target node stack")
	require.ErrorContains(t, err, "rolled back to nspawn")

	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.NotContains(t, h.events, "remove-config kube1", "the source record survives until the target is up")
}

func TestMigrateInPlace_LocalKubeletFailureRollsBack(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.failAt["kubelet-on-host"] = errors.New("kubelet inactive")

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "kubelet inactive")
	assert.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestMigrateInPlace_RollbackFailureIsJournaled(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.target.StartErr = errors.New("container did not start")
	h.source.StartErr = errors.New("machine will not start")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorIs(t, err, migrate.ErrRollbackFailed)
	require.ErrorContains(t, err, "needs attention")

	j, jerr := h.journal()
	require.NoError(t, jerr)
	assert.Equal(t, migrate.StepFailed, j.Step)
}

func TestMigrateInPlace_CleanupFailureStillSucceedsAndKeepsTheJournalToRetry(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.source.DestroyErr = errors.New("busy")

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))

	j, err := h.journal()
	require.NoError(t, err)
	assert.Equal(t, migrate.StepReady, j.Step, "the next daemon start finishes the cleanup")
}

// ---------------------------------------------------------------------------
// Crash safety: resume from the journal
// ---------------------------------------------------------------------------

func writeJournal(t *testing.T, step migrate.Step, mutate ...func(*migrate.Journal)) migrate.Journal {
	t.Helper()

	j := migrate.Journal{
		Operation: "op-1", Strategy: migrate.InPlace,
		From: isolation.ModeNSpawn, To: isolation.ModeContainer,
		SourceSlot: "kube1", TargetSlot: "kube2", Step: step,
	}
	for _, m := range mutate {
		m(&j)
	}

	require.NoError(t, inPlaceJournalStore().Save(j))

	return j
}

func TestMigrateInPlace_ResumeAtEveryStep(t *testing.T) {
	tests := []struct {
		step       migrate.Step
		persisted  bool // the target's applied config is on disk
		wantErr    string
		source     []string
		target     []string
		wantEvents []string
		final      migrate.Step
		wantConfig []string // applied slots afterwards
	}{
		{
			step: migrate.StepPreparing, wantErr: "interrupted",
			target: []string{"destroy kube2"}, final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepStoppingSource, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2"},
			final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepStartingTarget, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2"},
			final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepPersisting, persisted: true, wantErr: "interrupted",
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2"},
			wantEvents: []string{"remove-config kube2", "persist kube1"}, final: migrate.StepRolledBack, wantConfig: []string{"kube1"},
		},
		{
			step: migrate.StepAwaitingReady, persisted: true,
			source: []string{"destroy kube1"}, wantEvents: []string{"kubelet-on-host kube2", "remove-config kube1"},
			final: migrate.StepComplete, wantConfig: []string{"kube2"},
		},
		{
			step: migrate.StepReady, persisted: true,
			source: []string{"destroy kube1"}, wantEvents: []string{"remove-config kube1"},
			final: migrate.StepComplete, wantConfig: []string{"kube2"},
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.step), func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			writeJournal(t, tt.step)

			if tt.persisted {
				target := h.active.Config.DeepCopy()
				target.Isolation = string(isolation.ModeContainer)
				h.applied["kube2"] = target
			}

			// The same operation arrives again after the restart.
			err := h.run(isolation.ModeContainer, migrate.InPlace)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}

			assert.Equal(t, tt.source, nilIfEmpty(h.source.Calls()))
			assert.Equal(t, tt.target, nilIfEmpty(h.target.Calls()))
			assert.Equal(t, tt.wantEvents, h.events)

			var slots []string
			for slot := range h.applied {
				slots = append(slots, slot)
			}

			assert.ElementsMatch(t, tt.wantConfig, slots)

			j, jerr := h.journal()
			require.NoError(t, jerr)
			assert.Equal(t, tt.final, j.Step)
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestMigrateInPlace_ResumeAwaitingReadyRollsBackWhenTheTargetIsNotUp(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepAwaitingReady)

	target := h.active.Config.DeepCopy()
	target.Isolation = string(isolation.ModeContainer)
	h.applied["kube2"] = target
	h.failAt["kubelet-on-host"] = errors.New("timed out")

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "timed out")
	assert.Equal(t, []string{"start kube1"}, h.source.Calls())
	assert.NotContains(t, h.applied, "kube2")
	assert.Contains(t, h.applied, "kube1")
}

func TestMigrateInPlace_TerminalJournalOfTheSameOperationIsNotRedone(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepComplete)

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))
	assert.Empty(t, h.events)
	assert.Empty(t, h.source.Calls())

	h = newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "readiness timed out" })

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "readiness timed out")
	assert.Empty(t, h.source.Calls())
}

func TestMigrateInPlace_AnotherOperationsInterruptedJournalIsResolvedFirst(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepStartingTarget, func(j *migrate.Journal) { j.Operation = "op-0" })

	err := h.run(isolation.ModeContainer, migrate.InPlace)
	require.ErrorContains(t, err, "earlier isolation migration (op-0) was interrupted")
	assert.Equal(t, []string{"start kube1"}, h.source.Calls(), "the earlier migration was rolled back")
	assert.NotContains(t, h.events, "preflight container", "the new request did not start")
}

func TestMigrateInPlace_AnotherOperationsFinishedJournalIsDropped(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepComplete, func(j *migrate.Journal) { j.Operation = "op-0" })

	require.NoError(t, h.run(isolation.ModeContainer, migrate.InPlace))

	j, err := h.journal()
	require.NoError(t, err)
	assert.Equal(t, "op-1", j.Operation)
}

func TestMigrateInPlace_UnreadableJournalRefusesToProceed(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	require.NoError(t, os.WriteFile(inPlaceJournalPath(), []byte("{broken"), 0o600))

	require.ErrorContains(t, h.run(isolation.ModeContainer, migrate.InPlace), "journal is unreadable")
	assert.Empty(t, h.events)
}

func TestActiveSlotOrderFollowsTheJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	_ = h

	assert.Equal(t, []string{"kube1", "kube2"}, activeSlotOrder(), "no journal")

	for step, want := range map[migrate.Step][]string{
		migrate.StepPreparing:     {"kube1", "kube2"},
		migrate.StepPersisting:    {"kube1", "kube2"},
		migrate.StepAwaitingReady: {"kube2", "kube1"},
		migrate.StepReady:         {"kube2", "kube1"},
		migrate.StepComplete:      {"kube2", "kube1"},
		migrate.StepRolledBack:    {"kube1", "kube2"},
	} {
		writeJournal(t, step)
		assert.Equal(t, want, activeSlotOrder(), string(step))
	}

	// Slot two as the source.
	writeJournal(t, migrate.StepReady, func(j *migrate.Journal) { j.SourceSlot, j.TargetSlot = "kube2", "kube1" })
	assert.Equal(t, []string{"kube1", "kube2"}, activeSlotOrder())
	writeJournal(t, migrate.StepStoppingSource, func(j *migrate.Journal) { j.SourceSlot, j.TargetSlot = "kube2", "kube1" })
	assert.Equal(t, []string{"kube2", "kube1"}, activeSlotOrder())

	// A reprovision journal does not reorder anything.
	writeJournal(t, migrate.StepReady, func(j *migrate.Journal) { j.Strategy = migrate.Reprovision })
	assert.Equal(t, []string{"kube1", "kube2"}, activeSlotOrder())
}

// ---------------------------------------------------------------------------
// Reprovision
// ---------------------------------------------------------------------------

func TestMigrateReprovision_StagesTheHandoffAndStartsTheUnit(t *testing.T) {
	// Every pair that involves none, and the in-place pairs asked to reprovision.
	for _, tt := range []struct{ from, to isolation.Mode }{
		{isolation.ModeNSpawn, isolation.ModeNone},
		{isolation.ModeContainer, isolation.ModeNone},
		{isolation.ModeNone, isolation.ModeNSpawn},
		{isolation.ModeNone, isolation.ModeContainer},
		{isolation.ModeNSpawn, isolation.ModeContainer},
		{isolation.ModeContainer, isolation.ModeNSpawn},
	} {
		t.Run(string(tt.from)+" to "+string(tt.to), func(t *testing.T) {
			h := newMigHarness(t, tt.from)

			err := h.run(tt.to, migrate.Reprovision)
			require.ErrorIs(t, err, ErrMigrationHandedOff)

			// Preflight and the staged binary's check come before the unit
			// starts, and nothing drains or cordons the node.
			assert.Equal(t, []string{
				"preflight " + string(tt.to),
				"handoff unbounded-agent isolation-reprovision --check",
				"systemctl daemon-reload",
				"systemctl enable " + ReprovisionUnit,
				"systemctl start --no-block " + ReprovisionUnit,
			}, h.events)

			dir := reprovisionDir()
			assert.FileExists(t, filepath.Join(dir, ReprovisionBinaryName))

			cfgPath := filepath.Join(dir, ReprovisionConfigName)
			assert.FileExists(t, cfgPath)

			info, serr := os.Stat(cfgPath)
			require.NoError(t, serr)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the configuration holds a token")

			data, rerr := os.ReadFile(cfgPath)
			require.NoError(t, rerr)
			assert.Contains(t, string(data), fmt.Sprintf("%q", string(tt.to)))

			j, jerr := reprovisionJournalFor().Load()
			require.NoError(t, jerr)
			assert.Equal(t, migrate.StepReprovisioning, j.Step)
			assert.Equal(t, migrate.Reprovision, j.Strategy)
			assert.Equal(t, tt.to, j.To)

			unit, uerr := os.ReadFile(reprovisionUnitPath())
			require.NoError(t, uerr)
			assert.Contains(t, string(unit), "isolation-reprovision --handoff-dir "+dir)
			assert.Contains(t, string(unit), "Restart=on-failure")
			assert.Contains(t, string(unit), "ConditionPathExists="+filepath.Join(dir, ReprovisionJournalName))

			// Nothing is reset by the daemon: the source node is untouched.
			assert.Empty(t, h.source.Calls())
			assert.Empty(t, h.target.Calls())
		})
	}
}

func TestMigrateReprovision_FailureBeforeTheUnitUndoesEverything(t *testing.T) {
	tests := []struct {
		name    string
		fail    func(h *migHarness)
		wantErr string
	}{
		{name: "preflight", fail: func(h *migHarness) { h.failAt["preflight"] = errors.New("read-only /opt") }, wantErr: "read-only /opt"},
		{name: "staged check", fail: func(h *migHarness) { h.failAt["handoff"] = errors.New("no plan") }, wantErr: "cannot be bootstrapped"},
		{name: "unit start", fail: func(h *migHarness) { h.failAt["systemctl"] = errors.New("boom") }, wantErr: "boom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			tt.fail(h)

			require.ErrorContains(t, h.run(isolation.ModeNone, migrate.Reprovision), tt.wantErr)

			assert.NoDirExists(t, reprovisionDir(), "no handoff is left behind")
			assert.Empty(t, h.source.Calls(), "the node stack was never stopped")
		})
	}
}

func TestMigrateReprovision_InvalidTargetConfigIsRejectedBeforeAnything(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	h.active.Config.Cluster.ClusterDNS = ""

	require.ErrorContains(t, h.run(isolation.ModeNone, migrate.Reprovision), "is invalid")
	assert.Empty(t, h.events)
	assert.NoDirExists(t, reprovisionDir())
}

func TestResolveReprovision(t *testing.T) {
	stage := func(h *migHarness, step migrate.Step) {
		require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
			Operation: "op-1", Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
			Step: step,
		}))
		require.NoError(t, os.MkdirAll(reprovisionDir(), 0o700))
	}

	t.Run("still in progress until the target mode is applied", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		stage(h, migrate.StepBootstrapping)

		j, _ := reprovisionJournalFor().Load() //nolint:errcheck // staged above
		require.ErrorIs(t, resolveJournal(context.Background(), discardLogger(), h.env(), j), ErrMigrationHandedOff)
		assert.DirExists(t, reprovisionDir())
	})

	t.Run("reported once the target mode is applied", func(t *testing.T) {
		h := newMigHarness(t, isolation.ModeNSpawn)
		stage(h, migrate.StepReprovisioning)

		none := h.active.Config.DeepCopy()
		none.Isolation = string(isolation.ModeNone)
		h.applied = map[string]*provision.AgentConfig{"kube1": none}

		j, _ := reprovisionJournalFor().Load() //nolint:errcheck // staged above
		require.NoError(t, resolveJournal(context.Background(), discardLogger(), h.env(), j))

		got, err := reprovisionJournalFor().Load()
		require.NoError(t, err)
		assert.Equal(t, migrate.StepBootstrapped, got.Step)
	})
}

func TestRenderReprovisionUnit(t *testing.T) {
	unit := renderReprovisionUnit("/var/lib/x")
	assert.Contains(t, unit, "ExecStart=/var/lib/x/unbounded-agent isolation-reprovision --handoff-dir /var/lib/x")
	assert.NotContains(t, unit, "—", "no em-dashes")
}
