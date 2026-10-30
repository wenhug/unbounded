// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// stubResetHost replaces what detectResetMode reads and returns how many times
// the applied config was read.
func stubResetHost(t *testing.T, applied isolation.Mode, readErr error, noneTraces, containerTraces bool) *int {
	t.Helper()

	oldRead, oldNone, oldContainer := readAppliedMode, hostIsNone, hostIsContainer

	t.Cleanup(func() { readAppliedMode, hostIsNone, hostIsContainer = oldRead, oldNone, oldContainer })

	reads := 0
	readAppliedMode = func() (isolation.Mode, error) {
		reads++

		return applied, readErr
	}
	hostIsNone = func() bool { return noneTraces }
	hostIsContainer = func() bool { return containerTraces }

	return &reads
}

func TestDetectResetMode(t *testing.T) {
	unreadable := errors.New("checksum mismatch")

	tests := []struct {
		name            string
		applied         isolation.Mode
		readErr         error
		noneTraces      bool
		containerTraces bool
		want            isolation.Mode
	}{
		{name: "readable nspawn ignores stray none traces", applied: isolation.ModeNSpawn, noneTraces: true, want: isolation.ModeNSpawn},
		{name: "readable nspawn ignores stray container traces", applied: isolation.ModeNSpawn, containerTraces: true, want: isolation.ModeNSpawn},
		{name: "readable nspawn ignores both", applied: isolation.ModeNSpawn, noneTraces: true, containerTraces: true, want: isolation.ModeNSpawn},
		{name: "readable none", applied: isolation.ModeNone, want: isolation.ModeNone},
		{name: "readable none ignores container traces", applied: isolation.ModeNone, containerTraces: true, want: isolation.ModeNone},
		{name: "readable container", applied: isolation.ModeContainer, want: isolation.ModeContainer},
		{name: "readable container ignores none traces", applied: isolation.ModeContainer, noneTraces: true, want: isolation.ModeContainer},
		{name: "gone with none traces", readErr: appliedconfig.ErrNotFound, noneTraces: true, want: isolation.ModeNone},
		{name: "gone with container traces", readErr: appliedconfig.ErrNotFound, containerTraces: true, want: isolation.ModeContainer},
		{name: "unreadable with none traces", readErr: unreadable, noneTraces: true, want: isolation.ModeNone},
		{name: "unreadable with container traces", readErr: unreadable, containerTraces: true, want: isolation.ModeContainer},
		{name: "none traces win over container traces when unreadable", readErr: unreadable, noneTraces: true, containerTraces: true, want: isolation.ModeNone},
		{name: "gone and nothing else is nspawn", readErr: appliedconfig.ErrNotFound, want: isolation.ModeNSpawn},
		{name: "unreadable and nothing else is nspawn", readErr: unreadable, want: isolation.ModeNSpawn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reads := stubResetHost(t, tt.applied, tt.readErr, tt.noneTraces, tt.containerTraces)

			assert.Equal(t, tt.want, detectResetMode(discardLogger()))
			assert.Equal(t, 1, *reads, "the applied config is read once")
		})
	}
}

func TestResetNodeResourcesFollowTheAppliedModeNotTheTraces(t *testing.T) {
	stubResetHost(t, isolation.ModeNSpawn, nil, true, true)

	name := resetNodeResources(discardLogger()).Name()
	assert.Contains(t, name, "stop-machine", "an nspawn host with leftovers is torn down as nspawn")
	assert.NotContains(t, name, "stop-none-node")

	stubResetHost(t, isolation.ModeNone, nil, false, false)
	assert.Contains(t, resetNodeResources(discardLogger()).Name(), "stop-none-node")

	stubResetHost(t, isolation.ModeContainer, nil, false, false)
	assert.NotContains(t, resetNodeResources(discardLogger()).Name(), "stop-machine")
	assert.NotContains(t, resetNodeResources(discardLogger()).Name(), "stop-none-node")
}

func TestResetBuildsItsTasksFromOneRead(t *testing.T) {
	reads := stubResetHost(t, isolation.ModeNone, nil, false, false)

	_ = ResetAgent(discardLogger())

	assert.Equal(t, 1, *reads, "ResetAgent decides the mode once for the teardown and the sync paths")

	*reads = 0
	_ = ResetAgentForReprovision(discardLogger())

	assert.Equal(t, 1, *reads)
}
