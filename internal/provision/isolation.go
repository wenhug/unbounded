// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MinIsolationAgentVersion is the first unbounded-agent release that
// recognizes the Machine isolation field and the agent config "isolation"
// key (designs/agent-isolation-modes.md section 6.4).
//
// TODO(release): v0.19.0 is a PLACEHOLDER (the release after the latest tag
// at the time of writing). A maintainer must confirm the real first
// supporting release number before the isolation work ships.
const MinIsolationAgentVersion = "v0.19.0"

// Reasons for the IsolationSupported Machine condition.
const (
	// IsolationReasonAgentVersionSupported means the pinned (or latest)
	// agent version understands the requested isolation mode.
	IsolationReasonAgentVersionSupported = "AgentVersionSupported"
	// IsolationReasonAgentVersionTooOld means the pinned agent version
	// predates isolation support; the mode is not emitted.
	IsolationReasonAgentVersionTooOld = "AgentVersionTooOld"
	// IsolationReasonAgentVersionInvalid means the pinned agent version is
	// not a parseable semantic version; the mode is not emitted.
	IsolationReasonAgentVersionInvalid = "AgentVersionInvalid"
)

// IsolationSupport is the result of checking a Machine's requested isolation
// against its pinned agent version.
type IsolationSupport struct {
	// Required is true when the Machine requests a non-default isolation
	// mode, so a version check applies. When false, the other fields are
	// zero and no condition should be present.
	Required bool
	// Supported is true when the mode may be emitted into agent config.
	Supported bool
	// Reason is one of the IsolationReason* constants when Required.
	Reason string
	// Message is a human readable explanation when Required.
	Message string
}

// IsDefaultIsolation reports whether mode selects the default (nspawn)
// behavior. The empty mode means nspawn.
func IsDefaultIsolation(mode v1alpha3.IsolationMode) bool {
	return mode == "" || mode == v1alpha3.IsolationNSpawn
}

// CheckIsolationSupport checks the Machine's isolation against
// MinIsolationAgentVersion.
func CheckIsolationSupport(machine *v1alpha3.Machine) IsolationSupport {
	return checkIsolationSupport(machine, MinIsolationAgentVersion)
}

// checkIsolationSupport is CheckIsolationSupport with an injectable minimum.
//
// Version policy:
//   - Empty pin: the install script tracks the latest release, which is
//     assumed to support isolation.
//   - Unparseable pin: unsupported (AgentVersionInvalid); we cannot prove
//     the agent understands the field, and an old agent rejects unknown
//     config.
//   - Prerelease and dev tags use strict semver ordering, so
//     "v0.19.0-rc.1" is older than "v0.19.0" and is unsupported when the
//     minimum is "v0.19.0". This is the conservative choice; pin the final
//     release or a later one.
//   - URL/BaseURL overrides are not inspected; only spec.agent.version
//     is considered.
func checkIsolationSupport(machine *v1alpha3.Machine, minVersion string) IsolationSupport {
	if machine == nil || IsDefaultIsolation(machine.Spec.Isolation) {
		return IsolationSupport{}
	}

	mode := machine.Spec.Isolation

	pin := ""
	if machine.Spec.Agent != nil {
		pin = strings.TrimSpace(machine.Spec.Agent.Version)
	}

	if pin == "" {
		return IsolationSupport{
			Required:  true,
			Supported: true,
			Reason:    IsolationReasonAgentVersionSupported,
			Message:   fmt.Sprintf("isolation %q: agent version is unpinned and tracks the latest release", mode),
		}
	}

	have, err := semver.NewVersion(pin)
	if err != nil {
		return IsolationSupport{
			Required: true,
			Reason:   IsolationReasonAgentVersionInvalid,
			Message: fmt.Sprintf("isolation %q not applied: spec.agent.version %q is not a valid semantic version; use %s or newer",
				mode, pin, minVersion),
		}
	}

	need, err := semver.NewVersion(minVersion)
	if err != nil {
		// Programming error in the constant; fail closed.
		return IsolationSupport{
			Required: true,
			Reason:   IsolationReasonAgentVersionInvalid,
			Message:  fmt.Sprintf("isolation %q not applied: invalid minimum agent version %q", mode, minVersion),
		}
	}

	if have.LessThan(need) {
		return IsolationSupport{
			Required: true,
			Reason:   IsolationReasonAgentVersionTooOld,
			Message: fmt.Sprintf("isolation %q not applied: spec.agent.version %s is older than %s, the first release that supports isolation",
				mode, pin, minVersion),
		}
	}

	return IsolationSupport{
		Required:  true,
		Supported: true,
		Reason:    IsolationReasonAgentVersionSupported,
		Message:   fmt.Sprintf("isolation %q: agent version %s supports it", mode, pin),
	}
}
