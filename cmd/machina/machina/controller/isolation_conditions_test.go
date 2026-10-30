// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

func isolationMachine(mode unboundedv1alpha3.IsolationMode, pin string) *unboundedv1alpha3.Machine {
	m := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "iso"},
		Spec:       unboundedv1alpha3.MachineSpec{Isolation: mode},
	}
	if pin != "" {
		m.Spec.Agent = &unboundedv1alpha3.AgentSpec{Version: pin}
	}

	return m
}

func cond(m *unboundedv1alpha3.Machine, t string) *metav1.Condition {
	return apimeta.FindStatusCondition(m.Status.Conditions, t)
}

func TestSyncIsolationConditions_Support(t *testing.T) {
	t.Parallel()

	m := isolationMachine(unboundedv1alpha3.IsolationNone, "v0.0.1")
	require.True(t, syncIsolationConditions(m))

	c := cond(m, unboundedv1alpha3.MachineConditionIsolationSupported)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, provision.IsolationReasonAgentVersionTooOld, c.Reason)

	// Idempotent.
	require.False(t, syncIsolationConditions(m))

	// Bad pin.
	m.Spec.Agent.Version = "latest"
	require.True(t, syncIsolationConditions(m))
	require.Equal(t, provision.IsolationReasonAgentVersionInvalid, cond(m, unboundedv1alpha3.MachineConditionIsolationSupported).Reason)

	// Pin upgraded to the minimum: condition flips to True.
	m.Spec.Agent.Version = provision.MinIsolationAgentVersion
	require.True(t, syncIsolationConditions(m))
	c = cond(m, unboundedv1alpha3.MachineConditionIsolationSupported)
	require.Equal(t, metav1.ConditionTrue, c.Status)

	// Empty pin is supported.
	m.Spec.Agent.Version = ""
	syncIsolationConditions(m)
	require.Equal(t, metav1.ConditionTrue, cond(m, unboundedv1alpha3.MachineConditionIsolationSupported).Status)

	// Field removed: condition cleared.
	m.Spec.Isolation = ""
	require.True(t, syncIsolationConditions(m))
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationSupported))
}

func TestSyncIsolationConditions_NoFieldNoConditions(t *testing.T) {
	t.Parallel()

	m := isolationMachine("", "v0.0.1")
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)

	m.Spec.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)
}

func TestSyncIsolationConditions_Drift(t *testing.T) {
	t.Parallel()

	m := isolationMachine(unboundedv1alpha3.IsolationContainer, "")

	// Applied not reported: no drift condition.
	syncIsolationConditions(m)
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))

	// Applied nspawn, desired container: drift naming both modes.
	m.Status.ObservedIsolation = unboundedv1alpha3.IsolationNSpawn
	require.True(t, syncIsolationConditions(m))

	c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Contains(t, c.Message, "container")
	require.Contains(t, c.Message, "nspawn")
	require.False(t, syncIsolationConditions(m))

	// Spec reverted to default while applied is nspawn: drift clears.
	m.Spec.Isolation = ""
	require.True(t, syncIsolationConditions(m))
	require.Equal(t, metav1.ConditionFalse, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Status)

	// Empty spec vs applied none: drift.
	m.Status.ObservedIsolation = unboundedv1alpha3.IsolationNone
	syncIsolationConditions(m)
	require.Equal(t, metav1.ConditionTrue, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Status)

	// Migrated: spec matches.
	m.Spec.Isolation = unboundedv1alpha3.IsolationNone
	syncIsolationConditions(m)
	require.Equal(t, metav1.ConditionFalse, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Status)

	// Applied report removed: condition removed.
	m.Status.ObservedIsolation = ""
	require.True(t, syncIsolationConditions(m))
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))
}

func TestMachineReconciler_IsolationConditionsPersisted(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// No SSH config: still gets conditions, and the spec is never changed.
	machine := isolationMachine(unboundedv1alpha3.IsolationNone, "v0.0.1")
	machine.Status.ObservedIsolation = unboundedv1alpha3.IsolationNSpawn

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).Build()
	reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)

	var got unboundedv1alpha3.Machine
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "iso"}, &got))
	require.Equal(t, unboundedv1alpha3.IsolationNone, got.Spec.Isolation)
	require.Equal(t, metav1.ConditionFalse, cond(&got, unboundedv1alpha3.MachineConditionIsolationSupported).Status)
	require.Equal(t, metav1.ConditionTrue, cond(&got, unboundedv1alpha3.MachineConditionIsolationDrift).Status)
}
