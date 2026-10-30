// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestRestartActiveNodeRunsTheRestartAndWrapsFailures(t *testing.T) {
	active := &ActiveMachine{Name: "kube2", Mode: isolation.ModeContainer}

	var ran int

	require.NoError(t, restartActiveNode(t.Context(), discardLogger(), active, func(context.Context) error { ran++; return nil }))
	assert.Equal(t, 1, ran)

	boom := errors.New("kubelet not active")
	err := restartActiveNode(t.Context(), discardLogger(), active, func(context.Context) error { return boom })
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "restart node")
}
