// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"fmt"
	"sync"
)

// Factory builds a Backend. deps is backend specific and may be nil.
type Factory func(deps any) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[Mode]Factory{}
)

// Register makes a backend available to New. It is meant to be called from the
// backend package's init or from the composition root. Registering a mode twice
// panics, since that is a programming error.
func Register(mode Mode, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[mode]; dup {
		panic(fmt.Sprintf("isolation: backend %q registered twice", mode))
	}

	registry[mode] = f
}

// New returns the backend for mode. The empty mode is the default mode. An
// unknown or unregistered mode is an error.
func New(mode Mode, deps any) (Backend, error) {
	m, err := ParseMode(string(mode))
	if err != nil {
		return nil, err
	}

	registryMu.RLock()

	f, ok := registry[m]

	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("isolation mode %q has no registered backend", m)
	}

	return f(deps)
}

// Unregister removes the backend for mode and returns its factory, so a test
// can exercise the "known mode with no registered backend" path whichever modes
// this binary ships, and then put the factory back with Register. Production
// code never calls it.
func Unregister(mode Mode) (Factory, bool) {
	registryMu.Lock()
	defer registryMu.Unlock()

	f, ok := registry[mode]
	delete(registry, mode)

	return f, ok
}
