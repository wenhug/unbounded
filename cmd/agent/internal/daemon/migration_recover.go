// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// recoverIsolationMigration runs at daemon start. A migration whose journal is
// still on disk was interrupted by a restart or handed off by a reprovision,
// and its MachineOperation was never finished: operations that are already
// InProgress are not reconciled again. It resolves the journal (resuming or
// rolling back, see resolveJournal), reports the outcome on the operation and
// removes the journal once the report is made. It never fails the daemon: what
// cannot be done now is tried again at the next start.
func recoverIsolationMigration(
	ctx context.Context,
	log *slog.Logger,
	c client.Client,
	store *installstate.Store,
	machineName string,
) {
	var pending []migrate.Journal

	for _, js := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		j, err := js.Load()

		switch {
		case errors.Is(err, migrate.ErrNoJournal):
		case err != nil:
			log.Error("an isolation migration journal is unreadable; the node may be mid-migration and needs attention", "error", err)
		default:
			pending = append(pending, j)
		}
	}

	if len(pending) == 0 {
		return
	}

	lock, err := installstate.AcquireWithin(ctx, installationLockWaitTimeout, store.AcquireMutationLock)
	if err != nil {
		log.Warn("could not take installation ownership to resolve an isolation migration; trying again at the next start", "error", err)

		return
	}

	defer releaseInstallationLock(log, lock)

	for _, j := range pending {
		env := migrationEnv{Operation: j.Operation}

		outcome := resolveJournal(ctx, log, env, j)
		if errors.Is(outcome, ErrMigrationHandedOff) {
			log.Info("isolation reprovision is still in progress", "operation", j.Operation)

			continue
		}

		if outcome == nil {
			if err := declareIsolation(ctx, c, machineName, j.To); err != nil {
				log.Warn("could not set spec.isolation to the migrated mode; set it to match so the drift clears", "mode", j.To, "error", err)
			}
		}

		if err := publishMigrationOutcome(ctx, c, machineName, j, outcome); err != nil {
			log.Warn("could not report the isolation migration outcome; trying again at the next start", "operation", j.Operation, "error", err)

			continue
		}

		log.Info("isolation migration resolved", "operation", j.Operation, "outcome", outcomeText(outcome))
		// Only this journal: another may be pending, and its report can still fail.
		clearJournals(log, j)
	}
}

// publishMigrationOutcome finishes the MachineOperation unless it is already
// finished or gone.
func publishMigrationOutcome(ctx context.Context, c client.Client, machineName string, j migrate.Journal, outcome error) error {
	var op v1alpha3.MachineOperation

	err := c.Get(ctx, client.ObjectKey{Name: j.Operation}, &op)
	if apierrors.IsNotFound(err) || err == nil && op.Status.IsTerminal() {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get MachineOperation %s: %w", j.Operation, err)
	}

	result := daemon.MachineOperationResult[int64]{
		Phase:   v1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: fmt.Sprintf("IsolationMigrate to %s completed (%s)", j.To, j.Strategy),
	}

	if machine, err := getLocalMachine(ctx, c, machineName); err == nil {
		result.ObservedMachineGeneration = machine.Generation
	}

	if outcome != nil {
		result.Phase = v1alpha3.OperationPhaseFailed
		result.Reason = "ExecutionFailed"
		result.Message = outcome.Error()
	}

	return daemon.FinishMachineOperation(ctx, c, daemon.MachineOperation{Name: j.Operation}, result)
}
