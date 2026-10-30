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

func TestCheckIsolationSupport_Injected(t *testing.T) {
	t.Parallel()

	const minV = "v1.2.0"

	tests := []struct {
		name      string
		mode      v1alpha3.IsolationMode
		pin       string
		agent     bool
		required  bool
		supported bool
		reason    string
	}{
		{name: "default empty needs no check", mode: "", pin: "v0.0.1", agent: true},
		{name: "nspawn needs no check", mode: v1alpha3.IsolationNSpawn, pin: "v0.0.1", agent: true},
		{name: "no agent spec tracks latest", mode: v1alpha3.IsolationNone, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "empty pin tracks latest", mode: v1alpha3.IsolationNone, agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "whitespace pin is empty", mode: v1alpha3.IsolationNone, pin: "  ", agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "older", mode: v1alpha3.IsolationContainer, pin: "v1.1.9", agent: true, required: true, reason: IsolationReasonAgentVersionTooOld},
		{name: "exact boundary", mode: v1alpha3.IsolationContainer, pin: "v1.2.0", agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "boundary without v prefix", mode: v1alpha3.IsolationContainer, pin: "1.2.0", agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "newer", mode: v1alpha3.IsolationContainer, pin: "v1.10.0", agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "prerelease of boundary is older", mode: v1alpha3.IsolationNone, pin: "v1.2.0-rc.1", agent: true, required: true, reason: IsolationReasonAgentVersionTooOld},
		{name: "prerelease of newer is supported", mode: v1alpha3.IsolationNone, pin: "v1.3.0-rc.1", agent: true, required: true, supported: true, reason: IsolationReasonAgentVersionSupported},
		{name: "dev tag older", mode: v1alpha3.IsolationNone, pin: "v1.1.0-dev", agent: true, required: true, reason: IsolationReasonAgentVersionTooOld},
		{name: "garbage", mode: v1alpha3.IsolationNone, pin: "latest", agent: true, required: true, reason: IsolationReasonAgentVersionInvalid},
		{name: "sha", mode: v1alpha3.IsolationNone, pin: "deadbeef/abc", agent: true, required: true, reason: IsolationReasonAgentVersionInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkIsolationSupport(isoMachine(tt.mode, tt.pin, tt.agent), minV)
			require.Equal(t, tt.required, got.Required)
			require.Equal(t, tt.supported, got.Supported)
			require.Equal(t, tt.reason, got.Reason)

			if tt.required {
				require.NotEmpty(t, got.Message)
			}
		})
	}

	require.False(t, checkIsolationSupport(nil, minV).Required)
}

func TestCheckIsolationSupport_Constant(t *testing.T) {
	t.Parallel()

	require.True(t, CheckIsolationSupport(isoMachine(v1alpha3.IsolationNone, MinIsolationAgentVersion, true)).Supported,
		"the constant must be parseable and exactly supported")
	require.False(t, CheckIsolationSupport(isoMachine(v1alpha3.IsolationNone, "v0.1.0", true)).Supported)
	require.Equal(t, IsolationReasonAgentVersionTooOld,
		CheckIsolationSupport(isoMachine(v1alpha3.IsolationNone, "v0.1.0", true)).Reason)
	require.True(t, CheckIsolationSupport(isoMachine(v1alpha3.IsolationContainer, "", true)).Supported)
}

func TestBuildAgentConfig_IsolationGating(t *testing.T) {
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

	// Supported (unpinned and at/after the minimum) is emitted.
	require.Equal(t, "none", build(isoMachine(v1alpha3.IsolationNone, "", true)).Isolation)
	require.Equal(t, "container", build(isoMachine(v1alpha3.IsolationContainer, MinIsolationAgentVersion, true)).Isolation)

	// Too old or unparseable: withheld.
	require.Empty(t, build(isoMachine(v1alpha3.IsolationNone, "v0.1.0", true)).Isolation)
	require.Empty(t, build(isoMachine(v1alpha3.IsolationContainer, "bogus", true)).Isolation)

	// Explicit nspawn is passed through regardless of version, as before.
	require.Equal(t, "nspawn", build(isoMachine(v1alpha3.IsolationNSpawn, "v0.1.0", true)).Isolation)
}
