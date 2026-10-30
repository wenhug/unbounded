// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/renameio/v2"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// Reprovision resets the host and bootstraps it again in the target mode. The
// reset removes the daemon and the agent binary, so the daemon cannot run the
// bootstrap itself. Instead, before anything is reset, it stages a handoff in
// ReprovisionDir: a copy of the binary, the target configuration (the applied
// configuration with Isolation set) and a journal. It then installs and starts
// the unit ReprovisionUnit, whose command (the hidden isolation-reprovision
// subcommand) runs the existing reset, then the bootstrap stage plan for the
// target mode, and is retried by systemd (and run again at boot) until it
// succeeds. The daemon that bootstrap installs finds the journal at startup,
// sees the applied config in the target mode and reports the operation.
//
// The unit and ReprovisionDir are not touched by the reset, so a crash or a
// reboot at any point leaves the handoff to continue. What is not covered: a
// bootstrap that keeps failing leaves the host reset, the unit failed and the
// operation InProgress, because no daemon is left to report it; the Machine
// conditions set by the bootstrap reporter and the unit's journal say why.

// Seams for the host-mutating parts, replaced in tests.
var (
	// runHandoffCommand runs the staged binary, for its --check.
	runHandoffCommand = func(ctx context.Context, bin string, args ...string) error {
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput() //nolint:gosec // the binary is the agent's own staged copy
		if err != nil {
			return fmt.Errorf("%s %v: %w: %s", bin, args, err, out)
		}

		return nil
	}

	// systemctlForMigration runs systemctl.
	systemctlForMigration = func(ctx context.Context, log *slog.Logger, args ...string) error {
		return executil.RunCmd(ctx, log, executil.Systemctl(), args...)
	}

	// agentExecutable is the running agent binary, copied into the handoff.
	agentExecutable = os.Executable
)

func reprovisionUnitPath() string { return filepath.Join(reprovisionUnitDir(), ReprovisionUnit) }

func renderReprovisionUnit(dir string) string {
	return fmt.Sprintf(`# Written by unbounded-agent for an IsolationMigrate reprovision. It resets this
# host and bootstraps it again in the new isolation mode, and is removed by the
# daemon that reports the outcome.
[Unit]
Description=unbounded-agent isolation reprovision
After=network-online.target
Wants=network-online.target
ConditionPathExists=%[1]s/%[2]s
StartLimitIntervalSec=3600
StartLimitBurst=10

[Service]
Type=simple
ExecStart=%[1]s/%[3]s isolation-reprovision --handoff-dir %[1]s
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
`, dir, ReprovisionJournalName, ReprovisionBinaryName)
}

// migrateReprovision stages the handoff and starts the unit.
// Nothing is reset here. It returns ErrMigrationHandedOff on success.
func migrateReprovision(ctx context.Context, log *slog.Logger, active *ActiveMachine, req migrate.Request, env migrationEnv) error {
	targetCfg := active.Config.DeepCopy()
	targetCfg.Isolation = string(req.Target)

	if err := targetCfg.Validate(); err != nil {
		return fmt.Errorf("configuration for isolation mode %s is invalid: %w", req.Target, err)
	}

	// Before any mutation.
	if err := preflightMigrationTarget(ctx, log, req.Target, targetCfg); err != nil {
		return err
	}

	dir := reprovisionDir()
	store := reprovisionJournalFor()

	j := migrate.Journal{Operation: env.Operation, Strategy: migrate.Reprovision, From: active.Mode, To: req.Target}

	cleanup := func(cause error) error {
		// Nothing was reset, so the handoff is thrown away.
		removeReprovisionFiles(log)

		return cause
	}

	if err := stageHandoff(log, dir, targetCfg); err != nil {
		return cleanup(err)
	}

	// The staged binary validates the configuration and that a bootstrap plan
	// exists for the mode, so an unsupported target fails before anything is reset.
	if err := runHandoffCommand(ctx, filepath.Join(dir, ReprovisionBinaryName), "isolation-reprovision", "--check", "--handoff-dir", dir); err != nil {
		return cleanup(fmt.Errorf("the target configuration cannot be bootstrapped: %w", err))
	}

	// From here the node is going away. The journal says so before the unit
	// can reset anything.
	j.Step = migrate.StepReprovisioning

	if err := store.Save(j); err != nil {
		return cleanup(err)
	}

	if err := installReprovisionUnit(ctx, log, dir); err != nil {
		return cleanup(err)
	}

	log.Info("reprovision handed off; the host resets and bootstraps in the new mode", "unit", ReprovisionUnit, "to", req.Target)

	return ErrMigrationHandedOff
}

// stageHandoff writes the binary copy and target configuration.
func stageHandoff(log *slog.Logger, dir string, cfg *provision.AgentConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	self, err := agentExecutable()
	if err != nil {
		return fmt.Errorf("find the running agent binary: %w", err)
	}

	if err := fsutil.InstallFile(self, filepath.Join(dir, ReprovisionBinaryName), 0o755); err != nil {
		return fmt.Errorf("copy the agent binary into %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(provision.UnboundedAgentConfig{AgentConfig: *cfg}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal target configuration: %w", err)
	}

	path := filepath.Join(dir, ReprovisionConfigName)
	if err := renameio.WriteFile(path, data, 0o600, renameio.WithTempDir(dir)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	log.Info("reprovision handoff staged", "dir", dir)

	return nil
}

// installReprovisionUnit writes the unit, reloads systemd and starts the unit
// without waiting: the unit resets the host, which includes stopping this
// daemon.
func installReprovisionUnit(ctx context.Context, log *slog.Logger, dir string) error {
	path := reprovisionUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if err := renameio.WriteFile(path, []byte(renderReprovisionUnit(dir)), 0o644, renameio.WithTempDir(filepath.Dir(path))); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ReprovisionUnit},
		{"start", "--no-block", ReprovisionUnit},
	} {
		if err := systemctlForMigration(ctx, log, args...); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}

	return nil
}

// removeReprovisionFiles removes the unit and the handoff directory.
func removeReprovisionFiles(log *slog.Logger) {
	ctx := context.Background()

	if _, err := os.Lstat(reprovisionUnitPath()); err == nil {
		if err := systemctlForMigration(ctx, log, "disable", ReprovisionUnit); err != nil {
			log.Warn("could not disable the reprovision unit", "error", err)
		}

		if err := os.Remove(reprovisionUnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("could not remove the reprovision unit", "error", err)
		}

		if err := systemctlForMigration(ctx, log, "daemon-reload"); err != nil {
			log.Warn("could not reload systemd after removing the reprovision unit", "error", err)
		}
	}

	if err := os.RemoveAll(reprovisionDir()); err != nil {
		log.Warn("could not remove the reprovision directory", "dir", reprovisionDir(), "error", err)
	}
}

// resolveReprovision is resolveJournal for a reprovision. A journal still at the
// legacy draining step means the host was not reset, so the handoff is undone. After it, the handoff is in
// charge: the outcome is success once the applied config records the target
// mode, and still in progress until then.
func resolveReprovision(_ context.Context, log *slog.Logger, _ migrationEnv, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepLegacyDraining:
		removeReprovisionFiles(log)

		return fmt.Errorf("%w before the host was reset; the %s node was not changed", migrate.ErrInterrupted, j.From)
	case migrate.StepReprovisioning, migrate.StepBootstrapping:
		if !appliedInMode(log, j.To) {
			return ErrMigrationHandedOff
		}

		j.Step = migrate.StepBootstrapped
		if err := reprovisionJournalFor().Save(j); err != nil {
			log.Warn("could not record that the reprovision finished", "error", err)
		}

		return nil
	default:
		return fmt.Errorf("unexpected reprovision step %q", j.Step)
	}
}

// appliedInMode reports whether the host has an applied config recorded in
// mode, which is how a finished bootstrap shows.
func appliedInMode(log *slog.Logger, mode isolation.Mode) bool {
	for _, slot := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		active, err := readMigrationConfig(log, slot)
		if err == nil && active.Mode == mode {
			return true
		}
	}

	return false
}
