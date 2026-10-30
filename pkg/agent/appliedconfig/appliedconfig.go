// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package appliedconfig reads the small part of an applied agent config that
// read-only tooling needs: which slot is active and which isolation mode it
// runs in. It deliberately avoids the full config decode and validation so it
// keeps working for hosts written by older agents.
package appliedconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// ErrNotFound means no slot has an applied config.
var ErrNotFound = errors.New("no applied config found")

// Applied is the active slot and its recorded isolation mode.
type Applied struct {
	// Slot is the blue/green slot, for example kube1.
	Slot string
	// Mode is the recorded isolation mode. A config with no recorded mode is
	// nspawn.
	Mode isolation.Mode
}

// Read returns the first slot (kube1, then kube2) that has an applied config in
// goalstates.AgentConfigDir.
func Read() (*Applied, error) {
	return ReadDir(goalstates.AgentConfigDir)
}

// ReadDir is Read for an explicit config directory. A present sha256 sidecar
// is verified; an absent one is accepted. An unrecognized mode is an error so
// an agent never treats a newer mode as nspawn.
func ReadDir(dir string) (*Applied, error) {
	for _, slot := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		path := filepath.Join(dir, slot+"-applied-config.json")

		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read applied config %s: %w", path, err)
		}

		if err := goalstates.VerifyChecksum(data, path+".sha256"); err != nil {
			return nil, fmt.Errorf("verify applied config %s: %w", path, err)
		}

		var raw struct {
			Isolation string `json:"Isolation"`
		}

		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("decode applied config %s: %w", path, err)
		}

		mode, err := isolation.ParseMode(raw.Isolation)
		if err != nil {
			return nil, fmt.Errorf("applied config %s: %w", path, err)
		}

		return &Applied{Slot: slot, Mode: mode}, nil
	}

	return nil, fmt.Errorf("%w in %s", ErrNotFound, dir)
}
