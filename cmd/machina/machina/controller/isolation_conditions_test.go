// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func isolationMachine(mode unboundedv1alpha3.IsolationMode) *unboundedv1alpha3.Machine {
	m := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "iso"},
		Spec:       unboundedv1alpha3.MachineSpec{Isolation: mode},
	}

	return m
}

func cond(m *unboundedv1alpha3.Machine, t string) *metav1.Condition {
	return apimeta.FindStatusCondition(m.Status.Conditions, t)
}

func TestSyncIsolationConditions_NoFieldNoConditions(t *testing.T) {
	t.Parallel()

	m := isolationMachine("")
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)

	m.Spec.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)
}

func TestSyncIsolationConditions_Drift(t *testing.T) {
	t.Parallel()

	m := isolationMachine(unboundedv1alpha3.IsolationContainer)

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
	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.ObservedIsolation = unboundedv1alpha3.IsolationNSpawn

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).Build()
	reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)

	var got unboundedv1alpha3.Machine
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "iso"}, &got))
	require.Equal(t, unboundedv1alpha3.IsolationNone, got.Spec.Isolation)
	require.Equal(t, metav1.ConditionTrue, cond(&got, unboundedv1alpha3.MachineConditionIsolationDrift).Status)
}

// The agent patches status.observedIsolation at the same time. Losing that race
// is expected, not an error: the reconcile runs again on the fresh object.
func TestMachineReconciler_AStatusConflictRequeuesInsteadOfFailing(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.ObservedIsolation = unboundedv1alpha3.IsolationNSpawn

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "unbounded-cloud.io", Resource: "machines"}, "iso", nil)
			},
		}).Build()
	reconciler := &MachineReconciler{Client: c, Scheme: s}

	res, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)
	require.Positive(t, res.RequeueAfter, "the reconcile is retried")
}

func TestMachineReconciler_AnotherStatusErrorIsStillReturned(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.ObservedIsolation = unboundedv1alpha3.IsolationNSpawn

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewInternalError(context.DeadlineExceeded)
			},
		}).Build()
	reconciler := &MachineReconciler{Client: c, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.Error(t, err)
}
