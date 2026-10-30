// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func init() {
	registerStagePlan(isolation.ModeContainer, func() stagePlan { return &containerPlan{} })
}

// containerPlan is the bootstrap plan for the container mode: the node stack
// runs in a privileged OCI container (systemd as PID 1) supervised by an
// agent-owned unit. The host-global phases are the nspawn plan's, minus the
// systemd-container package and the machined hardening. What differs is that
// the OS image is unpacked into a container slot, an OCI bundle and unit are
// generated, and the stack inside is started through "<runtime> exec".
//
// PROVISIONAL: the container mode is experimental and unverified on a real
// host; see the containerops package comment.
type containerPlan struct {
	node *containerops.Node

	// sync, archives and persist are the host-touching steps; tests replace them.
	sync     func(paths ...string) error
	archives func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task
	persist  func(log *slog.Logger, slot string, cfg *config.AgentConfig) phases.Task
}

func (p *containerPlan) syncFS(paths ...string) error {
	if p.sync != nil {
		return p.sync(paths...)
	}

	return fsutil.SyncFilesystems(paths...)
}

func (p *containerPlan) downloadArchives(s *agentStages) phases.Task {
	if p.archives != nil {
		return p.archives(s.log, s.archives)
	}

	return rootfs.DownloadContainerImageArchives(s.log, s.archives)
}

func (p *containerPlan) persistApplied(log *slog.Logger, cfg *config.AgentConfig) phases.Task {
	if p.persist != nil {
		return p.persist(log, containerops.Slot, cfg)
	}

	return daemon.PersistAppliedConfig(log, containerops.Slot, cfg)
}

func (containerPlan) EnsureHostClean(ctx context.Context, s *agentStages) error {
	return host.EnsureNoExistingDeploymentFor(ctx, s.log, isolation.ModeContainer)
}

func (p *containerPlan) ResolveInputs(ctx context.Context, s *agentStages) error {
	downloads, archives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, s.cfg)
	if err != nil {
		return err
	}

	s.archives = archives

	s.gs, err = goalstates.ResolveMachine(s.log, &s.cfg.AgentConfig, containerops.Slot, downloads)
	if err != nil {
		return err
	}

	p.node = containerops.New(containerops.Params{Log: s.log, Config: &s.cfg.AgentConfig, Downloads: downloads, Slot: containerops.Slot, GoalState: s.gs})

	// Resolving now applies the container overrides to s.gs, which the mode
	// independent stages (attestation) share.
	_, err = p.node.GoalState()

	return err
}

// PrepareHost is the nspawn plan's sequence without the steps that exist only
// for the machine: no systemd-container and no apt hardening against machined
// restarts. The distro containerd and kubelet units are still disabled because
// the container shares /run/containerd with the host (design invariant 3), so a
// host containerd would own the socket the container's containerd needs. The
// kernel modules the node needs are loaded on the host, because the sysctls
// below need br_netfilter and a container does not load modules at boot.
func (p *containerPlan) PrepareHost(ctx context.Context, s *agentStages) error {
	if err := hostroot.Prepare(ctx, s.log, "bin", "libexec"); err != nil {
		return err
	}

	if err := daemon.InstallBootstrapBinary(); err != nil {
		return err
	}

	if err := phases.Serial(s.log,
		host.InstallPackagesFor(s.log, isolation.ModeContainer),
		noneops.LoadKernelModules(s.log, nil),
		phases.Parallel(s.log,
			host.ConfigureOS(s.log), host.ConfigureNFTables(s.log),
			phases.Serial(s.log, host.DisableDocker(s.log), host.ConfigureDocker(s.log)),
			host.DisableContainerd(s.log), host.DisableKubelet(s.log),
			host.DisableSwap(s.log), host.HardenAPTFor(s.log, isolation.ModeContainer)),
	).Do(ctx); err != nil {
		return err
	}

	return fsutil.SyncFilesystems("/etc", hostroot.Resolve(), installstate.DefaultDirectory)
}

// PrepareNodeRoot installs the runtime, unpacks the image and builds the
// slot. A running container owns its rootfs, so as for nspawn a node that is
// already up is left alone; the unpack would otherwise rebuild it in place.
func (p *containerPlan) PrepareNodeRoot(ctx context.Context, s *agentStages) error {
	running, err := p.node.Running(ctx)
	if err != nil {
		return err
	}

	if running {
		s.log.Info("node container is already running; leaving its rootfs in place", "slot", containerops.Slot)

		return nil
	}

	if err := s.prepareCredentials(ctx); err != nil {
		return err
	}

	if err := phases.Serial(s.log,
		p.downloadArchives(s),
		funcTask{"prepare-container-node", func(ctx context.Context) error {
			_, err := p.node.Prepare(ctx)
			return err
		}},
	).Do(ctx); err != nil {
		return err
	}

	return p.syncFS(container.DefaultBase, hostroot.Resolve(), goalstates.SystemdSystemDir)
}

func (p *containerPlan) EnsureNodeStarted(ctx context.Context, s *agentStages) error {
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
		funcTask{"configure-container-node", func(ctx context.Context) (err error) {
			containerdChanged, kubeletChanged, err = p.node.Configure(ctx)
			return err
		}},
		funcTask{"start-container-node", func(ctx context.Context) error {
			return p.node.Start(ctx, wasRunning, containerdChanged, kubeletChanged)
		}},
	}

	if !wasRunning {
		tasks = append(tasks, p.persistApplied(s.log, &s.cfg.AgentConfig))
	}

	if err := phases.Serial(s.log, tasks...).Do(ctx); err != nil {
		return err
	}

	return p.syncFS(container.DefaultBase, goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
