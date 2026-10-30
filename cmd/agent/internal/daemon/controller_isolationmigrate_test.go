// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

func runIsolationMigrate(t *testing.T, params map[string]string, op *fakeNodeOperator) v1alpha3.MachineOperation {
	t.Helper()

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 3}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationIsolationMigrate,
			Parameters:    params,
		},
	}

	c := fakeStatusClient(machine, machineOp)
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))

	return updated
}

func TestReconcileIsolationMigrate_Complete(t *testing.T) {
	active := &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn}
	op := &fakeNodeOperator{active: active}

	updated := runIsolationMigrate(t, map[string]string{"targetMode": "container"}, op)

	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, int64(3), updated.Status.ObservedMachineGeneration)
	assert.Equal(t, 1, op.migrateCalls)
	assert.Same(t, active, op.migrateActive)
	assert.Equal(t, isolation.ModeContainer, op.migrateReq.Target)
	assert.Equal(t, migrate.InPlace, op.migrateStrategy, "nspawn to container defaults to in-place")
}

func TestReconcileIsolationMigrate_ReprovisionForNone(t *testing.T) {
	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn}}

	updated := runIsolationMigrate(t, map[string]string{"targetMode": "none"}, op)

	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, migrate.Reprovision, op.migrateStrategy)
}

func TestReconcileIsolationMigrate_InvalidParametersTouchNothing(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"missing target": {},
		"unknown target": {"targetMode": "microvm"},
		"bad strategy":   {"targetMode": "none", "strategy": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn}}

			updated := runIsolationMigrate(t, params, op)

			assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
			assert.Equal(t, 0, op.migrateCalls)

			cond := updated.Status.Conditions
			require.NotEmpty(t, cond)
		})
	}
}

func TestReconcileIsolationMigrate_UnsupportedPairRejectedBeforeMutation(t *testing.T) {
	// Same mode, and in-place involving none, are both rejected up front.
	for name, params := range map[string]map[string]string{
		"same mode":          {"targetMode": "nspawn"},
		"in-place with none": {"targetMode": "none", "strategy": "inPlace"},
	} {
		t.Run(name, func(t *testing.T) {
			op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn}}

			updated := runIsolationMigrate(t, params, op)

			assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
			assert.Equal(t, 0, op.migrateCalls)
		})
	}
}

func TestReconcileIsolationMigrate_ExecutionFailure(t *testing.T) {
	op := &fakeNodeOperator{
		active:     &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn},
		migrateErr: errors.New("target not ready"),
	}

	updated := runIsolationMigrate(t, map[string]string{"targetMode": "container"}, op)

	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "target not ready", updated.Status.Message)
}

func TestReconcileIsolationMigrate_FindActiveMachineFailed(t *testing.T) {
	op := &fakeNodeOperator{findErr: errors.New("no active machine")}

	updated := runIsolationMigrate(t, map[string]string{"targetMode": "container"}, op)

	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, 0, op.migrateCalls)
}

func TestNspawnNodeOperator_MigrateIsolationFailsWithoutTargetBackend(t *testing.T) {
	active := &ActiveMachine{Name: "kube1", Config: baseConfig(), Mode: isolation.ModeNSpawn}

	err := nspawnNodeOperator{}.MigrateIsolation(context.Background(), discardLogger(), active,
		migrate.Request{Target: isolation.ModeContainer}, migrate.InPlace)
	require.ErrorContains(t, err, "no registered backend")
}
