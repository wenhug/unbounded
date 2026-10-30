// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func isoMachine(mode v1alpha3.IsolationMode, pin string, hasAgent bool) *v1alpha3.Machine {
	m := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Spec:       v1alpha3.MachineSpec{Isolation: mode},
	}
	if hasAgent {
		m.Spec.Agent = &v1alpha3.AgentSpec{Version: pin}
	}

	return m
}

func TestBuildAgentConfig_IsolationPassThrough(t *testing.T) {
	t.Parallel()

	build := func(m *v1alpha3.Machine) UnboundedAgentConfig {
		return BuildAgentConfig(BuildAgentConfigParams{Machine: m})
	}

	// Unset field: output unchanged and omits the key.
	base := build(isoMachine("", "", false))
	require.Empty(t, base.Isolation)

	raw, err := json.Marshal(base)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "isolation")

	// The mode is passed through, whatever the pinned agent version is.
	for _, mode := range []v1alpha3.IsolationMode{v1alpha3.IsolationNSpawn, v1alpha3.IsolationNone, v1alpha3.IsolationContainer} {
		require.Equal(t, string(mode), build(isoMachine(mode, "", false)).Isolation)
		require.Equal(t, string(mode), build(isoMachine(mode, "v0.1.0", true)).Isolation)
	}
}
