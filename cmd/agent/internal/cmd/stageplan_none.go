// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func init() {
	registerStagePlan(isolation.ModeNone, func() stagePlan { return &nonePlan{} })
}

// nonePlan is the bootstrap plan for the none mode: the node stack runs as
// agent-owned units directly on the host. The host-global phases (nftables,
// sysctls, swap, docker, WireGuard keys later in reset) are the same ones the
// nspawn plan runs. What differs is that nothing is unpacked and no machine is
// started: artifacts are installed into a versioned directory, the node
// configuration is written at the shared host paths, and the units are started.
type nonePlan struct {
	node *noneops.Node

	// sync and persist are the host-touching steps; tests replace them.
	sync     func(paths ...string) error
	archives func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task
	persist  func(log *slog.Logger, slot string, cfg *config.AgentConfig) phases.Task
}

func (p *nonePlan) syncFS(paths ...string) error {
	if p.sync != nil {
		return p.sync(paths...)
	}

	return fsutil.SyncFilesystems(paths...)
}

func (p *nonePlan) downloadArchives(s *agentStages) phases.Task {
	if p.archives != nil {
		return p.archives(s.log, s.archives)
	}

	return rootfs.DownloadContainerImageArchives(s.log, s.archives)
}

func (p *nonePlan) persistApplied(log *slog.Logger, cfg *config.AgentConfig) phases.Task {
	if p.persist != nil {
		return p.persist(log, noneops.Slot, cfg)
	}

	return daemon.PersistAppliedConfig(log, noneops.Slot, cfg)
}

type funcTask struct {
	name string
	do   func(context.Context) error
}

func (t funcTask) Name() string                 { return t.name }
func (t funcTask) Do(ctx context.Context) error { return t.do(ctx) }

func (nonePlan) EnsureHostClean(ctx context.Context, s *agentStages) error {
	return host.EnsureNoExistingDeploymentFor(ctx, s.log, isolation.ModeNone)
}

func (p *nonePlan) ResolveInputs(ctx context.Context, s *agentStages) error {
	downloads, archives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, s.cfg)
	if err != nil {
		return err
	}

	s.archives = archives

	s.gs, err = goalstates.ResolveMachine(s.log, &s.cfg.AgentConfig, noneops.Slot, downloads)
	if err != nil {
		return err
	}

	p.node = noneops.New(noneops.Params{Log: s.log, Config: &s.cfg.AgentConfig, Downloads: downloads, GoalState: s.gs})

	// Resolving now applies the host-native overrides to s.gs, which the
	// mode independent stages (attestation) share.
	_, err = p.node.GoalState()

	return err
}

// PrepareHost is the nspawn plan's sequence without the steps that exist only
// for the machine: no systemd-container, no apt hardening against machined
// restarts, and no DisableContainerd/DisableKubelet, since those stop and
// disable the distro units and preflight has already refused a host that has
// any. The kernel modules the machine would have loaded at boot are loaded on
// the host first, because the sysctls below need br_netfilter.
func (p *nonePlan) PrepareHost(ctx context.Context, s *agentStages) error {
	if err := hostroot.Prepare(ctx, s.log, "bin", "libexec"); err != nil {
		return err
	}

	if err := daemon.InstallBootstrapBinary(); err != nil {
		return err
	}

	if err := phases.Serial(s.log,
		host.InstallPackagesFor(s.log, isolation.ModeNone),
		noneops.LoadKernelModules(s.log, nil),
		phases.Parallel(s.log,
			host.ConfigureOS(s.log), host.ConfigureNFTables(s.log),
			phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
			host.DisableSwap(s.log), host.HardenAPTFor(s.log, isolation.ModeNone)),
	).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/etc", hostroot.Resolve(), installstate.DefaultDirectory)
}

// PrepareNodeRoot installs the version and writes the units. A running node
// owns its binaries, so as for nspawn a node that is already up is left alone.
func (p *nonePlan) PrepareNodeRoot(ctx context.Context, s *agentStages) error {
	running, err := p.node.Running(ctx)
	if err != nil {
		return err
	}

	if running {
		s.log.Info("node units are already running; leaving the installed version in place")

		return nil
	}

	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	if err := phases.Serial(s.log,
		p.downloadArchives(s),
		funcTask{"install-none-node", p.node.Provision},
	).Do(ctx); err != nil {
		return err
	}

	return p.syncFS(none.DefaultBase, hostroot.Resolve(), goalstates.SystemdSystemDir)
}

func (p *nonePlan) EnsureNodeStarted(ctx context.Context, s *agentStages) error {
	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	// Asked before the stage runs, as for nspawn, because afterwards the answer
	// is always yes. A node found running was not built by this attempt, so the
	// applied config is left alone; see nodeStartTask.
	wasRunning, err := p.node.Running(ctx)
	if err != nil {
		return err
	}

	var containerdChanged, kubeletChanged bool

	tasks := []phases.Task{
		funcTask{"configure-none-node", func(ctx context.Context) (err error) {
			containerdChanged, kubeletChanged, err = p.node.Configure(ctx)
			return err
		}},
		funcTask{"start-none-node", func(ctx context.Context) error {
			return p.node.Start(ctx, wasRunning, containerdChanged, kubeletChanged)
		}},
	}

	if !wasRunning {
		tasks = append(tasks, p.persistApplied(s.log, &s.cfg.AgentConfig))
	}

	if err := phases.Serial(s.log, tasks...).Do(ctx); err != nil {
		return err
	}

	return p.syncFS(none.DefaultBase, goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
