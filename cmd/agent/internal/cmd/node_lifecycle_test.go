// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeLifecycleCommand(t *testing.T) {
	cmd := newCmdNodeLifecycle(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.Hidden)
	require.Len(t, cmd.Commands(), 3)

	names := map[string]bool{}

	for _, c := range cmd.Commands() {
		require.True(t, c.Hidden)

		names[c.Name()] = true
	}

	require.Equal(t, map[string]bool{"pre-start": true, "post-start": true, "reconcile": true}, names)

	cmd.SetArgs([]string{"pre-start", "other"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "invalid container slot")
}

func TestNodeLifecycleRegistered(t *testing.T) {
	require.NotNil(t, newNodeLifecycle(testLogger()))
}
