// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

func observedIsolationClient(t *testing.T) (client.Client, *api.Machine) {
	t.Helper()

	m := &api.Machine{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}

	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(m).WithStatusSubresource(m).Build(), m
}

func TestReportObservedIsolation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		isolation string
		want      api.IsolationMode
	}{
		{"legacy config without the setting is nspawn", "", api.IsolationNSpawn},
		{"explicit nspawn", "nspawn", api.IsolationNSpawn},
		{"none", "none", api.IsolationNone},
		{"container", "container", api.IsolationContainer},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, m := observedIsolationClient(t)
			cfg := &provision.AgentConfig{MachineName: m.Name, Isolation: tt.isolation}

			require.NoError(t, reportObservedIsolation(t.Context(), c, cfg))
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
			require.Equal(t, tt.want, m.Status.ObservedIsolation)

			// Reporting again is a no-op.
			require.NoError(t, reportObservedIsolation(t.Context(), c, cfg))
		})
	}
}

func TestReportObservedIsolationFollowsModeChange(t *testing.T) {
	c, m := observedIsolationClient(t)

	require.NoError(t, reportObservedIsolation(t.Context(), c, &provision.AgentConfig{MachineName: m.Name}))
	require.NoError(t, reportObservedIsolation(t.Context(), c, &provision.AgentConfig{MachineName: m.Name, Isolation: "container"}))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Equal(t, api.IsolationContainer, m.Status.ObservedIsolation)
}

func TestReportObservedIsolationRejectsUnknownModeAndMissingMachine(t *testing.T) {
	c, m := observedIsolationClient(t)

	require.ErrorContains(t, reportObservedIsolation(t.Context(), c, &provision.AgentConfig{MachineName: m.Name, Isolation: "microvm"}), "unsupported isolation mode")
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Empty(t, m.Status.ObservedIsolation)

	require.Error(t, reportObservedIsolation(t.Context(), c, &provision.AgentConfig{MachineName: "absent"}))
}

func TestReportMachineObservationsReportsBoth(t *testing.T) {
	c, m := observedIsolationClient(t)

	require.NoError(t, reportMachineObservations(t.Context(), c, &provision.AgentConfig{MachineName: m.Name, ProvisioningFormat: "ignition", Isolation: "none"}))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(m), m))
	require.Equal(t, api.ProvisioningFormatIgnition, m.Status.ObservedProvisioningFormat)
	require.Equal(t, api.IsolationNone, m.Status.ObservedIsolation)
}
