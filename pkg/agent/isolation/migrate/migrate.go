// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package migrate moves a live node from one isolation mode to another. The
// orchestration is written against isolation.Backend so it is independent of
// any particular mode, and every host or cluster side effect it needs is a
// hook supplied by the caller.
package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Strategy is how a migration is carried out.
type Strategy string

const (
	// InPlace prepares the target beside the source on the same host and
	// switches over, rolling back to the source on failure.
	InPlace Strategy = "inPlace"
	// Reprovision drains, resets and bootstraps again in the new mode.
	Reprovision Strategy = "reprovision"
)

// ParseStrategy validates s. Empty is returned as empty so the caller can ask
// Plan for the default.
func ParseStrategy(s string) (Strategy, error) {
	switch st := Strategy(s); st {
	case "", InPlace, Reprovision:
		return st, nil
	default:
		return "", fmt.Errorf("unsupported migration strategy %q (supported: %s, %s)", s, InPlace, Reprovision)
	}
}

// Plan returns the strategy to use to move from one mode to another, following
// the supported-pairs table in the design. A requested strategy that the pair
// does not support, and a move to the mode the node is already in, are errors.
// An empty request selects in-place when it is supported and reprovision
// otherwise.
func Plan(from, to isolation.Mode, requested Strategy) (Strategy, error) {
	for _, m := range []isolation.Mode{from, to} {
		if _, err := isolation.ParseMode(string(m)); err != nil {
			return "", err
		}
	}

	if from == to {
		return "", fmt.Errorf("node is already in isolation mode %q", to)
	}

	// In-place needs two backends that can run side by side in different
	// slots. ModeNone has no slot, so any move involving it is a reprovision.
	inPlaceOK := from != isolation.ModeNone && to != isolation.ModeNone

	switch requested {
	case "":
		if inPlaceOK {
			return InPlace, nil
		}

		return Reprovision, nil
	case InPlace:
		if !inPlaceOK {
			return "", fmt.Errorf("in-place migration from %q to %q is not supported; use %s", from, to, Reprovision)
		}

		return InPlace, nil
	case Reprovision:
		return Reprovision, nil
	default:
		return "", fmt.Errorf("unsupported migration strategy %q", requested)
	}
}

// InPlaceSpec describes an in-place migration. Hooks may be nil only where
// noted; a nil required hook is a programming error reported by Run.
type InPlaceSpec struct {
	Source     isolation.Backend
	SourceRoot isolation.NodeRoot
	Target     isolation.Backend
	// TargetSpec names the alternate slot and the node to prepare.
	TargetSpec isolation.NodeSpec

	// Preflight checks the target backend's requirements before anything is
	// mutated.
	Preflight func(ctx context.Context, target isolation.Backend) error
	// Download fetches the artifacts the target needs. Optional.
	Download func(ctx context.Context) error
	// CleanupNetwork removes host networking the source left behind, as a
	// repave does.
	CleanupNetwork func(ctx context.Context) error
	// PersistTarget records the new mode, slot and owning backend.
	PersistTarget func(ctx context.Context, target isolation.NodeRoot) error
	// RestoreSource puts the source's applied config back after PersistTarget
	// when the migration rolls back. Required whenever PersistTarget is set.
	RestoreSource func(ctx context.Context) error
	// WaitReady blocks until the target node stack is up on the host or a
	// bounded timeout passes. Cordoning, draining and uncordoning the Node
	// belong to the operator, not to the migration.
	WaitReady func(ctx context.Context) error
	// RemoveSourceConfig removes the source's applied config after success.
	RemoveSourceConfig func(ctx context.Context) error

	// Record, when set, is called before each step that changes the host, with
	// the step about to run (and with StepReady and StepComplete after the
	// matching work). It is a write-ahead log: when Record returns an error the
	// step does not run and the migration is rolled back like any failure.
	// Recording is what lets ResumeInPlace decide what to do after a crash.
	Record func(ctx context.Context, step Step) error
	// TargetRoot is the node root of the target slot. Required by ResumeInPlace,
	// which has no Prepare result to take it from; ignored by RunInPlace.
	TargetRoot isolation.NodeRoot
}

// Step is how far an in-place migration got. Each value is written before the
// step it names starts, so after a crash it says what may have happened, not
// what finished.
type Step string

const (
	// StepPreparing precedes Prepare: a target slot may be partially built.
	StepPreparing Step = "preparing"
	// StepStoppingSource precedes the source stop: the source may be stopped.
	StepStoppingSource Step = "stopping-source"
	// StepStartingTarget precedes the target start: the target may be running.
	StepStartingTarget Step = "starting-target"
	// StepPersisting precedes PersistTarget: the target's applied config may
	// exist.
	StepPersisting Step = "persisting"
	// StepAwaitingReady follows PersistTarget: the target runs and is recorded.
	StepAwaitingReady Step = "awaiting-ready"
	// StepReady follows a successful WaitReady: only source cleanup is left.
	StepReady Step = "ready"
	// StepComplete follows source cleanup.
	StepComplete Step = "complete"
)

// ErrInterrupted is returned, joined with the outcome of the rollback, by
// ResumeInPlace when it undid a migration that a restart interrupted.
var ErrInterrupted = errors.New("isolation migration was interrupted")

func (s InPlaceSpec) record(ctx context.Context, step Step) error {
	if s.Record == nil {
		return nil
	}

	if err := s.Record(ctx, step); err != nil {
		return fmt.Errorf("record migration step %s: %w", step, err)
	}

	return nil
}

// Result reports how a migration ended.
type Result struct {
	// Target is the node root the node now runs on. Nil unless the migration
	// succeeded.
	Target isolation.NodeRoot
	// RolledBack is true when a failure was followed by a restart of the source.
	RolledBack bool
}

// ErrRollbackFailed is returned, joined with the original failure, when a
// failed migration could not restore the source. The node needs attention.
var ErrRollbackFailed = errors.New("rollback to the source failed")

// RunInPlace performs the in-place steps from the design. Nothing is mutated
// when preflight or download fails. Once the target exists, any failure
// destroys it and, if the source was stopped, starts the source again. The
// source root is retained until WaitReady succeeds.
func RunInPlace(ctx context.Context, s InPlaceSpec) (Result, error) {
	if s.Source == nil || s.Target == nil || s.SourceRoot == nil || s.Preflight == nil ||
		s.CleanupNetwork == nil || s.PersistTarget == nil ||
		s.RestoreSource == nil || s.WaitReady == nil || s.RemoveSourceConfig == nil {
		return Result{}, errors.New("migrate: incomplete in-place spec")
	}

	// 1 and 2: nothing has been mutated yet, so these fail cleanly.
	if err := s.Preflight(ctx, s.Target); err != nil {
		return Result{}, fmt.Errorf("preflight %s: %w", s.Target.Mode(), err)
	}

	if s.Download != nil {
		if err := s.Download(ctx); err != nil {
			return Result{}, fmt.Errorf("download %s artifacts: %w", s.Target.Mode(), err)
		}
	}

	// 3: prepare beside the source.
	if err := s.record(ctx, StepPreparing); err != nil {
		return Result{}, err
	}

	targetRoot, err := s.Target.Prepare(ctx, s.TargetSpec)
	if err != nil {
		// Prepare may have left a partial slot. The source was not touched.
		st := &state{spec: s, targetRoot: s.TargetRoot}
		if s.TargetRoot != nil {
			_, rbErr := st.rollback(ctx)
			err = errors.Join(err, rbErr)
		}

		return Result{}, fmt.Errorf("prepare %s: %w", s.Target.Mode(), err)
	}

	st := &state{spec: s, targetRoot: targetRoot}

	if err := st.run(ctx); err != nil {
		rolledBack, rbErr := st.rollback(ctx)
		if rbErr != nil {
			return Result{RolledBack: rolledBack}, errors.Join(err, fmt.Errorf("%w: %w", ErrRollbackFailed, rbErr))
		}

		return Result{RolledBack: rolledBack}, err
	}

	return st.finish(ctx)
}

// ResumeInPlace decides what to do with an in-place migration a restart
// interrupted, from the last step Record saw. Before the source was stopped it
// only removes the partial target. Once the source may be stopped it rolls back
// to the source and returns ErrInterrupted, since repeating a step that crashed
// the process could loop. When the target was already running and recorded it
// waits for the target as the original run would, then finishes or rolls back.
func ResumeInPlace(ctx context.Context, s InPlaceSpec, at Step) (Result, error) {
	if s.Source == nil || s.Target == nil || s.SourceRoot == nil || s.TargetRoot == nil ||
		s.RestoreSource == nil || s.WaitReady == nil || s.RemoveSourceConfig == nil {
		return Result{}, errors.New("migrate: incomplete resume spec")
	}

	st := &state{spec: s, targetRoot: s.TargetRoot}

	switch at {
	case StepComplete:
		return Result{Target: s.TargetRoot}, nil
	case StepReady:
		return st.finish(ctx)
	case StepAwaitingReady:
		st.sourceStopped, st.targetStarted, st.persisted = true, true, true

		if err := s.WaitReady(ctx); err != nil {
			return st.rollbackInterrupted(ctx, fmt.Errorf("wait for the target node stack: %w", err))
		}

		if err := s.record(ctx, StepReady); err != nil {
			return st.rollbackInterrupted(ctx, err)
		}

		return st.finish(ctx)
	case StepPreparing, StepLegacyDraining:
		return st.rollbackInterrupted(ctx, nil)
	case StepStoppingSource, StepStartingTarget:
		st.sourceStopped, st.targetStarted = true, at == StepStartingTarget

		return st.rollbackInterrupted(ctx, nil)
	case StepPersisting:
		st.sourceStopped, st.targetStarted, st.persisted = true, true, true

		return st.rollbackInterrupted(ctx, nil)
	default:
		return Result{}, fmt.Errorf("migrate: unknown step %q", at)
	}
}

func (st *state) rollbackInterrupted(ctx context.Context, cause error) (Result, error) {
	// A step that crashed or timed out may have started the target without
	// recording it, so stop it whenever the source may have been stopped.
	st.targetStarted = st.targetStarted || st.sourceStopped

	rolledBack, rbErr := st.rollback(ctx)

	err := ErrInterrupted
	if cause != nil {
		err = fmt.Errorf("%w: %w", ErrInterrupted, cause)
	}

	if rbErr != nil {
		return Result{RolledBack: rolledBack}, errors.Join(err, fmt.Errorf("%w: %w", ErrRollbackFailed, rbErr))
	}

	return Result{RolledBack: rolledBack}, err
}

// finish is the last step: the target is up. Failures from here do not undo the
// migration.
func (st *state) finish(ctx context.Context) (Result, error) {
	s := st.spec
	targetRoot := st.targetRoot

	var cleanup error
	if err := s.Source.Destroy(ctx, s.SourceRoot); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("destroy source: %w", err))
	}

	if err := s.RemoveSourceConfig(ctx); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("remove source applied config: %w", err))
	}

	if cleanup == nil {
		if err := s.record(ctx, StepComplete); err != nil {
			cleanup = err
		}
	}

	return Result{Target: targetRoot}, cleanup
}

type state struct {
	spec          InPlaceSpec
	targetRoot    isolation.NodeRoot
	sourceStopped bool
	targetStarted bool
	persisted     bool
}

func (st *state) run(ctx context.Context) error {
	s := st.spec

	// 4. Record, then mark, then stop: a failed stop may still have stopped
	// part of the source, but a failed record has stopped nothing.
	if err := s.record(ctx, StepStoppingSource); err != nil {
		return err
	}

	st.sourceStopped = true

	if err := s.Source.Stop(ctx, s.SourceRoot); err != nil {
		return fmt.Errorf("stop %s: %w", s.Source.Mode(), err)
	}

	// 5.
	if err := s.CleanupNetwork(ctx); err != nil {
		return fmt.Errorf("clean up host networking: %w", err)
	}

	// 6. Fresh kubelet credentials come from the normal bootstrap flow inside
	// Start; none are copied from the source.
	if err := s.record(ctx, StepStartingTarget); err != nil {
		return err
	}

	st.targetStarted = true

	if err := s.Target.Start(ctx, st.targetRoot); err != nil {
		return fmt.Errorf("start %s: %w", s.Target.Mode(), err)
	}

	// 7.
	if err := s.record(ctx, StepPersisting); err != nil {
		return err
	}

	st.persisted = true

	if err := s.PersistTarget(ctx, st.targetRoot); err != nil {
		return fmt.Errorf("persist applied config: %w", err)
	}

	if err := s.record(ctx, StepAwaitingReady); err != nil {
		return err
	}

	// 8.
	if err := s.WaitReady(ctx); err != nil {
		return fmt.Errorf("wait for the target node stack: %w", err)
	}

	return s.record(ctx, StepReady)
}

// rollback undoes whatever run did. It uses a context that outlives
// cancellation of the migration's own, so a timed-out migration still restores
// the source.
func (st *state) rollback(ctx context.Context) (bool, error) {
	s := st.spec
	ctx = context.WithoutCancel(ctx)

	var errs []error

	if st.targetStarted {
		if err := s.Target.Stop(ctx, st.targetRoot); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", s.Target.Mode(), err))
		}
	}

	if st.persisted {
		if err := s.RestoreSource(ctx); err != nil {
			errs = append(errs, fmt.Errorf("restore source applied config: %w", err))
		}
	}

	if err := s.Target.Destroy(ctx, st.targetRoot); err != nil {
		errs = append(errs, fmt.Errorf("destroy %s: %w", s.Target.Mode(), err))
	}

	restarted := false

	if st.sourceStopped {
		if err := s.Source.Start(ctx, s.SourceRoot); err != nil {
			errs = append(errs, fmt.Errorf("start %s: %w", s.Source.Mode(), err))
		} else {
			restarted = true
		}
	}

	return restarted, errors.Join(errs...)
}

// Parameter keys of an IsolationMigrate MachineOperation.
const (
	ParamTargetMode = "targetMode"
	ParamStrategy   = "strategy"
)

// Request is a validated IsolationMigrate request.
type Request struct {
	Target   isolation.Mode
	Strategy Strategy
}

// ParseParams validates the Parameters of an IsolationMigrate operation.
// targetMode is required; strategy is optional.
func ParseParams(params map[string]string) (Request, error) {
	raw := params[ParamTargetMode]
	if raw == "" {
		return Request{}, fmt.Errorf("IsolationMigrate requires parameter %s", ParamTargetMode)
	}

	target, err := isolation.ParseMode(raw)
	if err != nil {
		return Request{}, fmt.Errorf("parameter %s: %w", ParamTargetMode, err)
	}

	strategy, err := ParseStrategy(params[ParamStrategy])
	if err != nil {
		return Request{}, fmt.Errorf("parameter %s: %w", ParamStrategy, err)
	}

	return Request{Target: target, Strategy: strategy}, nil
}
