// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// checkGrants decides whether the host-native node stack can honor grants.
//
// The stack runs on the host, so a grant is satisfied natively when the thing it
// names is where the stack looks for it: a device node at its own path, a mount
// whose source and target are the same path. Such a grant is honored by
// checking that the source exists, so a missing device or directory fails
// Prepare instead of surfacing as a confusing runtime error. A grant that would
// need the host to look different to the stack (a bind mount at another path, a
// library path to add) cannot be honored and is refused. Nothing is ignored.
//
// NVIDIA library directories and the helper binary directory are the one place
// a different target is asked for: nspawn and container bind them at
// /run/host-nvidia/N so tooling can find them inside a private root. On the
// host the libraries are already on the loader path and the helper binaries are
// on PATH, so those grants are satisfied by the source existing.
func (b *Backend) checkGrants(grants []isolation.Grant) error {
	var errs []error

	hasNvidiaDevice := false

	for _, g := range grants {
		if err := b.checkGrant(g); err != nil {
			errs = append(errs, err)
		}

		if g.Purpose == isolation.PurposeNvidiaDevice {
			hasNvidiaDevice = true
		}
	}

	if hasNvidiaDevice {
		for _, p := range b.GPUHostPaths {
			if _, err := os.Stat(b.hostPath(p)); err != nil {
				errs = append(errs, fmt.Errorf("NVIDIA devices are present but %s is not available on the host (%w); isolation mode none needs the NVIDIA Container Toolkit installed on GPU hosts", p, err))
			}
		}
	}

	return errors.Join(errs...)
}

func (b *Backend) checkGrant(g isolation.Grant) error {
	switch g.Kind {
	case isolation.GrantDevice:
		if g.Source == "" {
			// A device class selector. The stack's units run without a device
			// cgroup policy, so every class is already reachable.
			if g.DeviceClass == "" {
				return errors.New("device grant names neither a device node nor a device class")
			}

			return nil
		}

		if err := b.requireSource(g); err != nil {
			return fmt.Errorf("device grant: %w", err)
		}

		return nil
	case isolation.GrantMount:
		if g.Source == "" {
			return errors.New("mount grant has no source")
		}

		if g.Target != "" && g.Target != g.Source && !nativeRemap(g.Purpose) {
			return fmt.Errorf("mount grant %s at %s cannot be honored in isolation mode %s: the node stack sees the host, so a path cannot be remapped; mount it at its own path or use isolation mode nspawn or container",
				g.Source, g.Target, isolation.ModeNone)
		}

		if err := b.requireSource(g); err != nil {
			return fmt.Errorf("mount grant: %w", err)
		}

		return nil
	case isolation.GrantLibraryPath:
		return fmt.Errorf("library-path grant %s cannot be honored in isolation mode %s: the node stack uses the host's library configuration", g.Source, isolation.ModeNone)
	default:
		return fmt.Errorf("unsupported grant kind %q", g.Kind)
	}
}

// nativeRemap reports whether a mount of this purpose is satisfied by the host
// view even though another mode binds it at a different path.
func nativeRemap(p isolation.GrantPurpose) bool {
	switch p {
	case isolation.PurposeNvidiaLibDir, isolation.PurposeNvidiaI386LibDir, isolation.PurposeNvidiaHelperBin:
		return true
	default:
		return false
	}
}

func (b *Backend) requireSource(g isolation.Grant) error {
	if !filepath.IsAbs(g.Source) {
		return fmt.Errorf("source %q must be an absolute path", g.Source)
	}

	if _, err := os.Stat(b.hostPath(g.Source)); err != nil {
		return fmt.Errorf("source %s: %w", g.Source, err)
	}

	return nil
}

func (b *Backend) hostPath(p string) string {
	if b.HostRoot == "" {
		return p
	}

	return filepath.Join(b.HostRoot, p)
}
