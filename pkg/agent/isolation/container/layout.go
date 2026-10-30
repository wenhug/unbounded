// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"fmt"
	"path/filepath"
)

// Defaults for the host layout. PROVISIONAL: the runtime path assumes the
// agent downloads runc or crun into an agent-owned directory under
// /opt/unbounded; the download wiring does not exist yet.
const (
	// DefaultBase holds one directory per slot (the unpacked rootfs).
	DefaultBase = "/var/lib/unbounded/containers"
	// DefaultRuntimePath is the OCI runtime executable (runc or crun).
	DefaultRuntimePath = "/opt/unbounded/runtime/runc"
	// SystemdDir is where the agent writes the supervising unit.
	SystemdDir = "/etc/systemd/system"
	// initPath is PID 1 inside the container.
	initPath = "/usr/lib/systemd/systemd"

	slotKube1 = "kube1"
	slotKube2 = "kube2"
)

// ValidSlot reports whether slot is one of the blue/green slots.
func ValidSlot(slot string) error {
	if slot != slotKube1 && slot != slotKube2 {
		return fmt.Errorf("invalid container slot %q (want %s or %s)", slot, slotKube1, slotKube2)
	}

	return nil
}

// ContainerID is the OCI container id for a slot.
func ContainerID(slot string) string { return "unbounded-" + slot }

// UnitName is the agent-owned supervising unit for a slot.
func UnitName(slot string) string { return "unbounded-node-" + slot + ".service" }

// Layout is the on-disk arrangement for container slots.
type Layout struct {
	// Base is the directory holding the slots. Empty means DefaultBase.
	Base string
}

func (l Layout) base() string {
	if l.Base == "" {
		return DefaultBase
	}

	return l.Base
}

// RootfsDir is the unpacked rootfs of a slot, which is what the NodeRoot maps.
func (l Layout) RootfsDir(slot string) string { return filepath.Join(l.base(), slot) }

// BundleDir is the OCI bundle directory holding config.json. It is kept apart
// from the rootfs so the rootfs stays a pure image tree. "bundles" is not a
// valid slot so the two never collide.
func (l Layout) BundleDir(slot string) string { return filepath.Join(l.base(), "bundles", slot) }

// ConfigPath is the generated OCI runtime config.
func (l Layout) ConfigPath(slot string) string {
	return filepath.Join(l.BundleDir(slot), "config.json")
}

// SpecPath records the NodeSpec so the lifecycle pre-start hook can
// regenerate config.json without the original caller.
func (l Layout) SpecPath(slot string) string {
	return filepath.Join(l.BundleDir(slot), "nodespec.json")
}
