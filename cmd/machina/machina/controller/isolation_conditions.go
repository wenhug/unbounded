// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Reasons for the IsolationDrift condition.
const (
	isolationReasonDrift   = "AppliedModeDiffers"
	isolationReasonInSync  = "AppliedModeMatches"
	isolationMessageNoMove = "nothing moves until an IsolationMigrate MachineOperation is created"
)

// effectiveIsolation resolves the empty mode to the nspawn default.
func effectiveIsolation(mode unboundedv1alpha3.IsolationMode) unboundedv1alpha3.IsolationMode {
	if mode == "" {
		return unboundedv1alpha3.IsolationNSpawn
	}

	return mode
}

// syncIsolationConditions maintains the IsolationDrift condition (design 6.5)
// on the Machine. It only mutates the in-memory status and reports whether
// anything changed.
//
// IsolationDrift is present only once the agent has reported
// status.observedIsolation; it is never acted upon.
func syncIsolationConditions(machine *unboundedv1alpha3.Machine) bool {
	changed := false

	observed := machine.Status.ObservedIsolation
	if observed == "" {
		if apimeta.RemoveStatusCondition(&machine.Status.Conditions, unboundedv1alpha3.MachineConditionIsolationDrift) {
			changed = true
		}

		return changed
	}

	desired := effectiveIsolation(machine.Spec.Isolation)
	applied := effectiveIsolation(observed)

	cond := metav1.Condition{
		Type:    unboundedv1alpha3.MachineConditionIsolationDrift,
		Status:  metav1.ConditionFalse,
		Reason:  isolationReasonInSync,
		Message: fmt.Sprintf("spec.isolation %q matches applied isolation %q", desired, applied),
	}
	if desired != applied {
		cond.Status = metav1.ConditionTrue
		cond.Reason = isolationReasonDrift
		cond.Message = fmt.Sprintf("spec.isolation is %q but the applied isolation is %q; %s", desired, applied, isolationMessageNoMove)
	}

	return setConditionIfChanged(machine, cond) || changed
}

// setConditionIfChanged sets cond and reports whether the stored condition
// differs (status, reason or message) from what was there before.
func setConditionIfChanged(machine *unboundedv1alpha3.Machine, cond metav1.Condition) bool {
	cond.ObservedGeneration = machine.Generation

	prev := apimeta.FindStatusCondition(machine.Status.Conditions, cond.Type)
	if prev != nil && prev.Status == cond.Status && prev.Reason == cond.Reason &&
		prev.Message == cond.Message && prev.ObservedGeneration == cond.ObservedGeneration {
		return false
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, cond)

	return true
}
