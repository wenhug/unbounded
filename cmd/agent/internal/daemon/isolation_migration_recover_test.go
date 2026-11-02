// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

func recoveryClient(t *testing.T, phase v1alpha3.OperationPhase) client.Client {
	t.Helper()

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec:       v1alpha3.MachineOperationSpec{MachineRef: "test-machine", OperationKind: v1alpha3.OperationIsolationMigrate},
		Status:     v1alpha3.MachineOperationStatus{Phase: phase},
	}

	return fakeStatusClient(machine, op)
}

func recoveryStore(t *testing.T) *installstate.Store {
	t.Helper()

	return installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock"))
}

func getOp(t *testing.T, c client.Client) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &op))

	return op
}

func TestRecoverIsolationMigration_NothingToDo(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

	assert.Empty(t, h.events)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, getOp(t, c).Status.Phase)
}

func TestRecoverIsolationMigration_ReportsAnInPlaceOutcome(t *testing.T) {
	tests := []struct {
		name      string
		step      migrate.Step
		persisted bool
		wantPhase v1alpha3.OperationPhase
		wantMsg   string
	}{
		{name: "target recorded and Ready", step: migrate.StepAwaitingReady, persisted: true, wantPhase: v1alpha3.OperationPhaseComplete, wantMsg: "IsolationMigrate to container completed"},
		{name: "crashed while starting the target", step: migrate.StepStartingTarget, wantPhase: v1alpha3.OperationPhaseFailed, wantMsg: "interrupted"},
		{name: "already rolled back", step: migrate.StepRolledBack, wantPhase: v1alpha3.OperationPhaseFailed, wantMsg: "readiness timed out"},
		{name: "already complete", step: migrate.StepComplete, wantPhase: v1alpha3.OperationPhaseComplete, wantMsg: "completed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newMigHarness(t, isolation.ModeNSpawn)
			writeJournal(t, tt.step, func(j *migrate.Journal) { j.Message = "readiness timed out" })

			if tt.persisted {
				target := h.active.Config.DeepCopy()
				target.Isolation = string(isolation.ModeContainer)
				h.applied["kube2"] = target
			}

			c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)

			recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

			op := getOp(t, c)
			assert.Equal(t, tt.wantPhase, op.Status.Phase)
			assert.Contains(t, op.Status.Message, tt.wantMsg)
			assert.Equal(t, int64(4), op.Status.ObservedMachineGeneration)

			_, err := h.journal()
			if tt.step == migrate.StepStartingTarget || tt.step == migrate.StepAwaitingReady || tt.step == migrate.StepRolledBack || tt.step == migrate.StepComplete {
				assert.ErrorIs(t, err, migrate.ErrNoJournal, "the journal goes once the outcome is reported")
			}
		})
	}
}

func TestRecoverIsolationMigration_DoesNotOverwriteAFinishedOperation(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepComplete)

	c := recoveryClient(t, v1alpha3.OperationPhaseFailed)

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

	assert.Equal(t, v1alpha3.OperationPhaseFailed, getOp(t, c).Status.Phase)

	_, err := h.journal()
	assert.ErrorIs(t, err, migrate.ErrNoJournal)
}

func TestRecoverIsolationMigration_ReprovisionReportedByTheNewDaemon(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Operation: "op-1", Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapping,
	}))

	// The bootstrap has not finished: nothing is reported yet.
	c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)
	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, getOp(t, c).Status.Phase)

	_, err := reprovisionJournalFor().Load()
	require.NoError(t, err)

	// Once the applied config is in the target mode it is.
	none := h.active.Config.DeepCopy()
	none.Isolation = string(isolation.ModeNone)
	h.applied = map[string]*provision.AgentConfig{"kube1": none}

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

	op := getOp(t, c)
	assert.Equal(t, v1alpha3.OperationPhaseComplete, op.Status.Phase)
	assert.Contains(t, op.Status.Message, "reprovision")

	_, err = reprovisionJournalFor().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal)
	assert.NoDirExists(t, reprovisionDir())
}

func TestRecoverIsolationMigration_StaysQuietWhileInstallationHoldsTheHost(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	writeJournal(t, migrate.StepStartingTarget)

	store := recoveryStore(t)
	rec, err := installstate.NewRecord("test-machine", "x")
	require.NoError(t, err)
	require.NoError(t, store.Save(rec)) // Installing

	c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)

	ctx, cancel := context.WithTimeout(context.Background(), 200_000_000)
	defer cancel()

	recoverIsolationMigration(ctx, discardLogger(), c, store, "test-machine")

	assert.Empty(t, h.source.Calls(), "no host work without installation ownership")
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, getOp(t, c).Status.Phase)
}

// ---------------------------------------------------------------------------
// The handler against the design's supported-pairs table
// ---------------------------------------------------------------------------

func TestReconcileIsolationMigrate_DesignMatrix(t *testing.T) {
	n, c, no := isolation.ModeNSpawn, isolation.ModeContainer, isolation.ModeNone

	tests := []struct {
		from, to isolation.Mode
		strategy string
		want     v1alpha3.OperationPhase
		used     migrate.Strategy
	}{
		{n, c, "", v1alpha3.OperationPhaseComplete, migrate.InPlace},
		{n, c, "inPlace", v1alpha3.OperationPhaseComplete, migrate.InPlace},
		{n, c, "reprovision", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{c, n, "", v1alpha3.OperationPhaseComplete, migrate.InPlace},
		{c, n, "inPlace", v1alpha3.OperationPhaseComplete, migrate.InPlace},
		{c, n, "reprovision", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{n, no, "", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{n, no, "reprovision", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{n, no, "inPlace", v1alpha3.OperationPhaseFailed, ""},
		{c, no, "", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{c, no, "inPlace", v1alpha3.OperationPhaseFailed, ""},
		{no, n, "", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{no, n, "inPlace", v1alpha3.OperationPhaseFailed, ""},
		{no, c, "", v1alpha3.OperationPhaseComplete, migrate.Reprovision},
		{no, c, "inPlace", v1alpha3.OperationPhaseFailed, ""},
		{n, n, "", v1alpha3.OperationPhaseFailed, ""},
		{c, c, "reprovision", v1alpha3.OperationPhaseFailed, ""},
		{no, no, "", v1alpha3.OperationPhaseFailed, ""},
	}

	for _, tt := range tests {
		t.Run(string(tt.from)+"->"+string(tt.to)+"/"+tt.strategy, func(t *testing.T) {
			op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: tt.from}}

			params := map[string]string{"targetMode": string(tt.to)}
			if tt.strategy != "" {
				params["strategy"] = tt.strategy
			}

			updated := runIsolationMigrate(t, params, op)

			assert.Equal(t, tt.want, updated.Status.Phase)

			if tt.want == v1alpha3.OperationPhaseComplete {
				assert.Equal(t, tt.used, op.migrateStrategy)
				assert.Equal(t, "op-1", op.migrateEnv.Operation, "the operation name reaches the migration for the journal")
			} else {
				assert.Zero(t, op.migrateCalls, "an unsupported pair is rejected before any host work")
			}
		})
	}
}

func TestReconcileIsolationMigrate_HandedOffStaysInProgress(t *testing.T) {
	op := &fakeNodeOperator{
		active:     &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn},
		migrateErr: ErrMigrationHandedOff,
	}

	updated := runIsolationMigrate(t, map[string]string{"targetMode": "none"}, op)

	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase, "the new daemon reports the end")
	assert.Contains(t, updated.Status.Message, "reprovisioning to none")
}

func TestReconcileIsolationMigrate_ReportsObservedIsolationAfterSuccess(t *testing.T) {
	cfg := baseConfig()
	cfg.Isolation = string(isolation.ModeContainer)
	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: cfg, Mode: isolation.ModeContainer}}

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 3}}
	opObj := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef: "test-machine", OperationKind: v1alpha3.OperationIsolationMigrate,
			Parameters: map[string]string{"targetMode": "nspawn"},
		},
	}
	c := fakeStatusClient(machine, opObj)

	_, err := newTestMachinaMachineOperationReconciler(t, c, op).ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var got v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
	assert.Equal(t, v1alpha3.IsolationMode("container"), got.Status.ObservedIsolation,
		"observedIsolation is read from the applied config the operator left")
}

// The operator asked for the new mode, so the Machine declares it afterwards and
// the IsolationDrift condition clears instead of staying True.
func TestReconcileIsolationMigrate_DeclaresTheMigratedModeInTheSpec(t *testing.T) {
	cfg := baseConfig()
	cfg.Isolation = string(isolation.ModeContainer)
	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: cfg, Mode: isolation.ModeNSpawn}}

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 3, Labels: map[string]string{"keep": "me"}}}
	opObj := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef: "test-machine", OperationKind: v1alpha3.OperationIsolationMigrate,
			Parameters: map[string]string{"targetMode": "container"},
		},
	}
	c := fakeStatusClient(machine, opObj)

	_, err := newTestMachinaMachineOperationReconciler(t, c, op).ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var got v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
	assert.Equal(t, v1alpha3.IsolationContainer, got.Spec.Isolation)
	assert.Equal(t, map[string]string{"keep": "me"}, got.Labels, "only spec.isolation is written")
}

func TestReconcileIsolationMigrate_ASpecPatchFailureDoesNotFailTheMigration(t *testing.T) {
	cfg := baseConfig()
	cfg.Isolation = string(isolation.ModeContainer)
	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: cfg, Mode: isolation.ModeNSpawn}}

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 3}}
	opObj := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef: "test-machine", OperationKind: v1alpha3.OperationIsolationMigrate,
			Parameters: map[string]string{"targetMode": "container"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}, &v1alpha3.MachineOperation{}).
		WithObjects(machine, opObj).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*v1alpha3.Machine); ok {
				return errors.New("machines is forbidden: cannot patch")
			}

			return cl.Patch(ctx, obj, patch, opts...)
		}}).Build()

	_, err := newTestMachinaMachineOperationReconciler(t, c, op).ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var done v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &done))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, done.Status.Phase, "the node did move, so the operation succeeded")
}

func TestRecoverIsolationMigration_DeclaresTheModeOnlyOnSuccess(t *testing.T) {
	for _, tt := range []struct {
		name string
		step migrate.Step
		want v1alpha3.IsolationMode
	}{
		{"completed", migrate.StepComplete, v1alpha3.IsolationContainer},
		{"rolled back", migrate.StepRolledBack, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			newMigHarness(t, isolation.ModeNSpawn)
			writeJournal(t, tt.step, func(j *migrate.Journal) { j.Message = "readiness timed out" })

			c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)

			recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

			var got v1alpha3.Machine
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &got))
			assert.Equal(t, tt.want, got.Spec.Isolation)
		})
	}
}

func TestNewMigrationBackendKnowsInPlaceModesOnly(t *testing.T) {
	cfg := baseConfig()

	b, root, err := newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNSpawn, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeNSpawn, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	b, root, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "kube2", Config: cfg})
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeContainer, b.Mode())
	assert.Equal(t, "kube2", root.Slot())

	_, _, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeNone, Slot: "kube1", Config: cfg})
	require.ErrorContains(t, err, "not supported")

	_, _, err = newMigrationBackend(discardLogger(), migrationEndpoint{Mode: isolation.ModeContainer, Slot: "bogus", Config: cfg})
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// A reprovision whose bootstrap never succeeded
// ---------------------------------------------------------------------------

func stageGivenUpHandoff(t *testing.T, h *migHarness) {
	t.Helper()

	dir := reprovisionDir()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ReprovisionBinaryName), []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ReprovisionConfigName), []byte(`{"token":"secret"}`), 0o600))
	require.NoError(t, os.MkdirAll(reprovisionUnitDir(), 0o755))
	require.NoError(t, os.WriteFile(reprovisionUnitPath(), []byte("[Unit]\n"), 0o644))
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Operation: "op-1", Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepFailed, Attempts: ReprovisionMaxAttempts,
		Message: "reprovision to isolation mode none gave up after 5 attempts; the host was reset and is not bootstrapped: kubelet did not join",
	}))
}

func TestAbandonReprovisionKeepsOnlyTheJournal(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageGivenUpHandoff(t, h)

	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))

	assert.NoFileExists(t, reprovisionUnitPath())
	assert.NoFileExists(t, filepath.Join(reprovisionDir(), ReprovisionBinaryName))
	assert.NoFileExists(t, filepath.Join(reprovisionDir(), ReprovisionConfigName), "the staged configuration holds the bootstrap token")
	assert.FileExists(t, reprovisionJournalOf(), "the journal is what the next daemon reports")
	assert.Equal(t, []string{"systemctl disable " + ReprovisionUnit, "systemctl daemon-reload"}, h.events)

	// Running it again, as an interrupted handoff does, is harmless.
	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))
}

func TestRecoverIsolationMigration_ReportsAReprovisionThatGaveUp(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)
	stageGivenUpHandoff(t, h)
	require.NoError(t, AbandonReprovision(context.Background(), discardLogger(), reprovisionDir()))

	c := recoveryClient(t, v1alpha3.OperationPhaseInProgress)

	// The first daemon to start on the reset host, after a new bootstrap.
	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

	op := getOp(t, c)
	assert.Equal(t, v1alpha3.OperationPhaseFailed, op.Status.Phase)
	assert.Contains(t, op.Status.Message, "gave up after 5 attempts")
	assert.Contains(t, op.Status.Message, "kubelet did not join")

	_, err := reprovisionJournalFor().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal, "the journal goes once the failure is reported")
	assert.NoDirExists(t, reprovisionDir())
}

func TestRemoveReprovisionHandoffLeavesNothingBehind(t *testing.T) {
	h := newMigHarness(t, isolation.ModeNSpawn)

	// Nothing staged: a reset on an ordinary host does nothing.
	require.NoError(t, RemoveReprovisionHandoff(discardLogger()).Do(context.Background()))
	assert.Empty(t, h.events)

	stageGivenUpHandoff(t, h)

	require.NoError(t, RemoveReprovisionHandoff(discardLogger()).Do(context.Background()))

	assert.NoFileExists(t, reprovisionUnitPath())
	assert.NoDirExists(t, reprovisionDir())
	assert.Equal(t, []string{
		"systemctl stop --no-block " + ReprovisionUnit,
		"systemctl disable " + ReprovisionUnit,
		"systemctl daemon-reload",
	}, h.events)
}

func TestResetIncludesTheHandoffCleanupExceptForTheHandoffsOwnReset(t *testing.T) {
	log := discardLogger()

	assert.Contains(t, resetResources(log).Name(), "remove-reprovision-handoff")
	assert.NotContains(t, resetNodeResources(log).Name(), "remove-reprovision-handoff")
}

// Two outcomes are pending: reporting the first must not clear the second's
// journal. If the second report then fails, the journal is all that is left of
// its outcome.
func TestRecoverIsolationMigration_ClearsOnlyTheJournalItReported(t *testing.T) {
	newMigHarness(t, isolation.ModeNSpawn)

	writeJournal(t, migrate.StepRolledBack, func(j *migrate.Journal) { j.Message = "readiness timed out" })
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Operation: "op-2", Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapped,
	}))

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := func(name string) *v1alpha3.MachineOperation {
		return &v1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       v1alpha3.MachineOperationSpec{MachineRef: "test-machine", OperationKind: v1alpha3.OperationIsolationMigrate},
			Status:     v1alpha3.MachineOperationStatus{Phase: v1alpha3.OperationPhaseInProgress},
		}
	}

	c := fake.NewClientBuilder().WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}, &v1alpha3.MachineOperation{}).
		WithObjects(machine, op("op-1"), op("op-2")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*v1alpha3.MachineOperation); ok && key.Name == "op-2" {
					return errors.New("API server unavailable")
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()

	recoverIsolationMigration(context.Background(), discardLogger(), c, recoveryStore(t), "test-machine")

	assert.Equal(t, v1alpha3.OperationPhaseFailed, getOp(t, c).Status.Phase, "the first outcome was reported")

	_, err := inPlaceJournalStore().Load()
	assert.ErrorIs(t, err, migrate.ErrNoJournal, "and its journal is gone")

	_, err = reprovisionJournalFor().Load()
	assert.NoError(t, err, "the second report failed, so its journal stays for the next start")
}

// A reprovision that is still running is not "resolved", and the message must
// not say it is.
func TestResumePending_ARunningReprovisionIsReportedAsStillRunning(t *testing.T) {
	newMigHarness(t, isolation.ModeNSpawn)

	require.NoError(t, os.MkdirAll(reprovisionDir(), 0o700))
	require.NoError(t, reprovisionJournalFor().Save(migrate.Journal{
		Operation: "op-1", Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone,
		Step: migrate.StepBootstrapping,
	}))

	handled, err := resumePending(context.Background(), discardLogger(), migrationEnv{Operation: "op-2"})
	require.Error(t, err)
	assert.False(t, handled)
	assert.Contains(t, err.Error(), "still in progress")
	assert.NotContains(t, err.Error(), "resolved")
}
