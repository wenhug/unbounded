// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package appliedconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func write(t *testing.T, dir, slot, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, slot+"-applied-config.json"), []byte(body), 0o600))
}

func TestReadDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		wantSlot string
		wantMode isolation.Mode
		wantErr  string
	}{
		{name: "missing mode is nspawn", files: map[string]string{"kube1": `{"MachineName":"x"}`}, wantSlot: "kube1", wantMode: isolation.ModeNSpawn},
		{name: "none", files: map[string]string{"kube2": `{"Isolation":"none"}`}, wantSlot: "kube2", wantMode: isolation.ModeNone},
		{name: "container", files: map[string]string{"kube1": `{"Isolation":"container"}`}, wantSlot: "kube1", wantMode: isolation.ModeContainer},
		{name: "kube1 wins", files: map[string]string{"kube1": `{}`, "kube2": `{"Isolation":"none"}`}, wantSlot: "kube1", wantMode: isolation.ModeNSpawn},
		{name: "unknown mode", files: map[string]string{"kube1": `{"Isolation":"microvm"}`}, wantErr: "unsupported isolation mode"},
		{name: "bad json", files: map[string]string{"kube1": `{`}, wantErr: "decode applied config"},
		{name: "none present", files: nil, wantErr: ErrNotFound.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for slot, body := range tt.files {
				write(t, dir, slot, body)
			}

			got, err := ReadDir(dir)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, &Applied{Slot: tt.wantSlot, Mode: tt.wantMode}, got)
		})
	}
}

func TestReadDirChecksum(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	body := `{"Isolation":"none"}`
	write(t, dir, "kube1", body)

	// A matching sidecar is accepted.
	sidecar := filepath.Join(dir, "kube1-applied-config.json.sha256")
	require.NoError(t, os.WriteFile(sidecar, []byte(goalstates.ComputeChecksum([]byte(body))+"\n"), 0o600))

	got, err := ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, isolation.ModeNone, got.Mode)

	// A mismatching sidecar is rejected.
	require.NoError(t, os.WriteFile(sidecar, []byte("deadbeef\n"), 0o600))

	_, err = ReadDir(dir)
	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
}
