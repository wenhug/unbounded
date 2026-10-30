// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// migrateIsolation is MigrateIsolation for every source mode. The source and
// target backends are chosen from the journal and the request, not from the
// operator that dispatched here.
//
// A journal on disk means an earlier attempt was interrupted. It is resumed or
// rolled back first, whatever was just requested, and the request is then
// reported as failed if it was a different one: the caller asks again once the
// node is in a known state.
func migrateIsolation(ctx context.Context, log *slog.Logger, active *ActiveMachine, req migrate.Request, strategy migrate.Strategy, env migrationEnv) error {
	// The target backend must exist in this agent. Failing here leaves the node
	// untouched.
	if _, err := isolation.New(req.Target, nil); err != nil {
		return fmt.Errorf("cannot migrate %s to %s: %w", active.Mode, req.Target, err)
	}

	if env.Nodes == nil {
		return fmt.Errorf("cannot migrate %s to %s: the daemon has no Kubernetes client to drain the node", active.Mode, req.Target)
	}

	if handled, err := resumePending(ctx, log, env); handled || err != nil {
		return err
	}

	log.Info("isolation migration requested", "from", active.Mode, "to", req.Target, "strategy", strategy)

	switch strategy {
	case migrate.InPlace:
		return migrateInPlace(ctx, log, active, req, env)
	case migrate.Reprovision:
		return migrateReprovision(ctx, log, active, req, env)
	default:
		return fmt.Errorf("unsupported migration strategy %q", strategy)
	}
}

// resumePending resolves a migration an earlier daemon left unfinished. It
// reports handled when the journal belongs to this operation, in which case the
// error is the operation's outcome (nil for success) and nothing more is to be
// done. With no journal it returns false and nil. A terminal journal of another
// operation was already reported and is dropped.
func resumePending(ctx context.Context, log *slog.Logger, env migrationEnv) (bool, error) {
	for _, store := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		j, err := store.Load()
		if errors.Is(err, migrate.ErrNoJournal) {
			continue
		}

		if err != nil {
			return false, fmt.Errorf("an isolation migration may be under way and its journal is unreadable: %w", err)
		}

		outcome := resolveJournal(ctx, log, env, j)
		if j.Operation == env.Operation {
			return true, outcome
		}

		if !j.Terminal() {
			return false, fmt.Errorf("an earlier isolation migration (%s) was interrupted and has been resolved (%v); request this one again",
				j.Operation, outcomeText(outcome))
		}

		clearJournals(log, j)
	}

	return false, nil
}

func outcomeText(err error) string {
	if err == nil {
		return "completed"
	}

	return err.Error()
}

// resolveJournal brings the host to a known state for j and returns the
// operation's outcome: nil for success.
func resolveJournal(ctx context.Context, log *slog.Logger, env migrationEnv, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepComplete, migrate.StepBootstrapped:
		return nil
	case migrate.StepRolledBack, migrate.StepFailed:
		return errors.New(j.Message)
	}

	if j.Strategy == migrate.Reprovision {
		return resolveReprovision(ctx, log, env, j)
	}

	return resumeInPlace(ctx, log, env, j)
}

// preflightMigrationTarget checks the target mode's host requirements without
// changing anything, before the first mutation.
var preflightMigrationTarget = func(ctx context.Context, log *slog.Logger, mode isolation.Mode, _ *config.AgentConfig) error {
	var checks []preflight.Checker

	switch mode {
	case isolation.ModeNSpawn:
		checks = append(checks, host.CheckNSpawnRuntime(log))
	case isolation.ModeContainer:
		checks = append(checks, host.CheckContainerBackend(log))
	case isolation.ModeNone:
		// The none backend's full preflight refuses a host that runs a kubelet
		// or containerd it does not own, and the node being migrated is one.
		// Bootstrap runs it after the reset, on the clean host. Before it, only
		// what can be checked without that: writable paths.
		if err := none.CheckWritable("/etc", none.DefaultBase, "/var"); err != nil {
			return fmt.Errorf("host cannot run isolation mode none: %w", err)
		}

		return nil
	default:
		return fmt.Errorf("unsupported isolation mode %q", mode)
	}

	report := preflight.Run(ctx, checks, preflight.Options{})
	if !report.HasErrors() {
		return nil
	}

	var msgs []string

	for _, r := range report.Checks {
		if r.Severity == preflight.SeverityError {
			msgs = append(msgs, r.Name+": "+r.Message)
		}
	}

	return fmt.Errorf("host cannot run isolation mode %s: %s", mode, strings.Join(msgs, "; "))
}

// clearJournals removes the terminal journal j came from, and for a reprovision
// the unit and directory that carried it. It is called after the outcome was
// reported to the MachineOperation, so a crash before then reports it again.
func clearJournals(log *slog.Logger, j migrate.Journal) {
	if j.Strategy == migrate.Reprovision {
		removeReprovisionFiles(log)

		return
	}

	if err := inPlaceJournalStore().Remove(); err != nil {
		log.Warn("could not remove the migration journal", "error", err)
	}
}

// clearReportedJournals drops any terminal journal after the handler reported.
func clearReportedJournals(log *slog.Logger) {
	for _, store := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		j, err := store.Load()
		if err == nil && j.Terminal() {
			clearJournals(log, j)
		}
	}
}
