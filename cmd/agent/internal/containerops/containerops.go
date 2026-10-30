// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package containerops is the composition root for the container isolation
// backend. The backend package cannot import the phase packages that unpack the
// OS image, provision the rootfs and configure the node stack, so this package
// binds them to a goal state and wires them into container.Backend.
//
// PROVISIONAL: the container mode is experimental. It is implemented against
// the design (section 8.3) and stubbed hosts; nothing here has run against a
// real runc, systemd or Azure Container Linux node. Comments marked
// PROVISIONAL name assumptions that are not verified.
//
// Layout of a container node. A slot (kube1 or kube2, blue/green as for nspawn)
// is an unpacked OS image under /var/lib/unbounded/containers/<slot> that runs
// systemd as PID 1 in an OCI container supervised by an agent-owned unit. The
// in-rootfs configuration phases are the nspawn ones, pointed at the container
// root through the goal state's RootOverride and, for commands, ExecOverride
// (which runs "<runtime> exec"). Paths under the same-path bind mounts
// (/var/lib/kubelet, /opt/cni, /etc/containerd/certs.d, /run/containerd,
// /sys/fs/bpf) resolve to the host directory so the agent writes where the
// container reads; see container.Root.
//
// Shared state. Because those directories are shared by both slots, a repave
// rewrites the kubelet configuration, kubeconfig and CNI binaries the previous
// slot also sees. Upgrade saves the shared configuration files first and puts
// them back before the previous slot starts again on a rollback. The previous
// slot's rootfs-private state (containerd data, unit drop-ins, binaries) is not
// touched until the new slot is healthy.
package containerops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

// kubeletClientPKIDir holds kubelet's rotated client certificates. It is not
// goalstates.KubeletPKIDir, which is the cluster CA directory.
const kubeletClientPKIDir = "/var/lib/kubelet/pki"

// Slot is the slot a first install builds.
const Slot = goalstates.NSpawnMachineKube1

// Params bind a Node to one agent configuration and one slot.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides
	// Slot is kube1 or kube2. Empty means kube1.
	Slot string

	// Layout, SystemdDir, RuntimePath and HelperPath are as in container.Backend.
	// An empty HelperPath is the agent's rollback-stable lifecycle helper.
	Layout      container.Layout
	SystemdDir  string
	RuntimePath string
	HelperPath  string
	// Run executes host commands. Nil uses the real host.
	Run container.Runner
	// HostRoot prefixes the same-path host locations. Empty is the real host;
	// tests use a temporary tree.
	HostRoot string
	// ConfigSnapshotDir is where Upgrade saves the shared configuration so a
	// rollback can restore it. Empty means a directory in the agent configuration
	// directory below HostRoot.
	ConfigSnapshotDir string
	// MountInfoPath is the mount table. Empty means /proc/self/mountinfo.
	MountInfoPath string
	// Host locates the SELinux and cgroup interfaces. Nil is the real host.
	Host *container.HostFS

	// GoalState, when set, is used instead of resolving one. Bootstrap passes
	// the goal state it has already resolved so GPU and device discovery happens
	// once.
	GoalState *goalstates.MachineGoalState
	// Resolve overrides goal state resolution. Tests use it.
	Resolve func(slot string) (*goalstates.MachineGoalState, error)

	// Unpack, InstallRuntime, InstallHelper and ProvisionRootfs replace the
	// real artifact work. Nil does it for real.
	Unpack          func(ctx context.Context, dir string) error
	InstallRuntime  func(ctx context.Context) error
	InstallHelper   func(ctx context.Context) error
	ProvisionRootfs func(ctx context.Context) error
	// SetupNetwork and CleanupNetwork replace the shared LocalDNS network
	// phases. Nil runs the real ones.
	SetupNetwork   func(ctx context.Context) error
	CleanupNetwork func(ctx context.Context) error
	// WaitLocalDNS replaces the LocalDNS readiness wait.
	WaitLocalDNS func(ctx context.Context) error

	// PollInterval is the wait between readiness polls. Zero means one second.
	PollInterval time.Duration
	// ReadyTimeout bounds the wait for systemd in the container. Zero means two
	// minutes.
	ReadyTimeout time.Duration
}

// Node is a container node bound to one configuration and slot.
type Node struct {
	p       Params
	slot    string
	backend *container.Backend

	// snapshot is the directory of the shared configuration saved by a running
	// Upgrade, until it is restored or discarded.
	snapshot string

	once sync.Once
	gs   *goalstates.MachineGoalState
	err  error
}

// New returns a Node. Goal state resolution is deferred to first use so that
// operations that never need it (Stop, Status, Destroy) work without a
// configuration.
func New(p Params) *Node {
	if p.Log == nil {
		p.Log = slog.Default()
	}

	slot := p.Slot
	if slot == "" {
		slot = Slot
	}

	helper := p.HelperPath
	if helper == "" {
		helper = goalstates.ResolveHostPaths().NSpawnLifecycleBinary
	}

	return &Node{p: p, slot: slot, backend: &container.Backend{
		Log:           p.Log,
		Layout:        p.Layout,
		RuntimePath:   p.RuntimePath,
		HelperPath:    helper,
		SystemdDir:    p.SystemdDir,
		Host:          p.Host,
		Run:           p.Run,
		SourceRoot:    p.HostRoot,
		MountInfoPath: p.MountInfoPath,
	}}
}

// SlotName is the slot this node manages.

// Backend is the container backend wired to this node. Its Unpack is bound on
// first use of the goal state.
func (n *Node) Backend() *container.Backend { return n.backend }

// GoalState is the resolved goal state with the container overrides applied.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		if err := container.ValidSlot(n.slot); err != nil {
			n.err = err
			return
		}

		n.gs, n.err = n.resolveGoalState()
		if n.err != nil {
			return
		}

		n.applyOverrides()
	})

	return n.err
}

func (n *Node) resolveGoalState() (*goalstates.MachineGoalState, error) {
	if n.p.GoalState != nil {
		return n.p.GoalState, nil
	}

	resolve := n.p.Resolve
	if resolve == nil {
		if n.p.Config == nil {
			return nil, errors.New("no agent configuration to resolve the node")
		}

		resolve = func(slot string) (*goalstates.MachineGoalState, error) {
			return goalstates.ResolveMachine(n.p.Log, n.p.Config, slot, n.p.Downloads)
		}
	}

	gs, err := resolve(n.slot)
	if err != nil {
		return nil, fmt.Errorf("resolve machine goal state: %w", err)
	}

	return gs, nil
}

// applyOverrides points the goal state at the container: the root is the
// slot's container root, commands run through the runtime's exec, and the
// backend unpacks the image and requires the LocalDNS network unit.
func (n *Node) applyOverrides() {
	root, _ := n.backend.NodeRoot(n.slot) //nolint:errcheck // the slot was validated above

	n.gs.RootFS.RootOverride = root
	n.gs.NodeStart.RootOverride = root
	n.gs.NodeStart.InContainer = true
	n.gs.NodeStart.ExecOverride = func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
		res, err := n.backend.Exec(ctx, root, isolation.ExecSpec{Args: args})

		return res.Stdout, err
	}

	n.backend.Unpack = func(ctx context.Context, _ isolation.NodeSpec, dir string) error { return n.unpack(ctx, dir) }

	if n.gs.NodeStart.LocalDNS.Enabled {
		n.backend.UnitRequires = []string{goalstates.LocalDNSNetworkUnit}
	}
}

func (n *Node) root() isolation.NodeRoot {
	r, _ := n.backend.NodeRoot(n.slot) //nolint:errcheck // the slot is validated by resolve and by every backend call

	return r
}

func (n *Node) runtimePath() string {
	if n.p.RuntimePath != "" {
		return n.p.RuntimePath
	}

	return container.DefaultRuntimePath
}

// unpack materializes the OS image into dir. It is the owned variant: a slot
// whose previous unpack was interrupted (no completion marker) is cleaned and
// rebuilt, and a completed one is kept. Callers must never point it at a slot
// that runs a node.
func (n *Node) unpack(ctx context.Context, dir string) error {
	if n.p.Unpack != nil {
		return n.p.Unpack(ctx, dir)
	}

	rf := n.gs.RootFS

	return oci.DownloadOwnedRootFS(n.p.Log, dir, rf.HostArch, rf.OCIImage).Do(ctx)
}

// Prepare builds the slot: installs the host runtime and lifecycle helper,
// unpacks the image, renders the bundle and unit, and provisions the rootfs
// (Kubernetes, CRI and CNI binaries and OS configuration). It does not start
// anything. It is safe to repeat on a slot that is not running.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	if err := n.installRuntime(ctx); err != nil {
		return nil, err
	}

	if err := n.installHelper(ctx); err != nil {
		return nil, err
	}

	r, err := n.backend.Prepare(ctx, isolation.NodeSpec{
		Slot:    n.slot,
		Version: n.gs.RootFS.KubernetesVersion,
		Grants:  n.gs.RootFS.Grants(),
	})
	if err != nil {
		return nil, err
	}

	if err := n.provisionRootfs(ctx); err != nil {
		return nil, err
	}

	return r, nil
}

func (n *Node) installRuntime(ctx context.Context) error {
	if n.p.InstallRuntime != nil {
		return n.p.InstallRuntime(ctx)
	}

	return rootfs.InstallHostRuntime(n.p.Log, n.gs.RootFS, n.runtimePath()).Do(ctx)
}

func (n *Node) installHelper(ctx context.Context) error {
	if n.p.InstallHelper != nil {
		return n.p.InstallHelper(ctx)
	}

	return rootfs.EnsureNSpawnLifecycleHelper().Do(ctx)
}

func (n *Node) provisionRootfs(ctx context.Context) error {
	if n.p.ProvisionRootfs != nil {
		return n.p.ProvisionRootfs(ctx)
	}

	rf := n.gs.RootFS

	return phases.Parallel(n.p.Log,
		rootfs.DownloadKubeBinaries(n.p.Log, rf),
		rootfs.DownloadCRIBinaries(n.p.Log, rf),
		rootfs.DownloadCNIBinaries(n.p.Log, rf),
		rootfs.ConfigureOS(rf),
		rootfs.DisableResolved(rf),
		rootfs.ConfigureLocalDNS(n.p.Log, rf),
	).Do(ctx)
}

// Configure writes the node configuration into the container root (and the
// same-path host directories) and prepares the shared LocalDNS network. It
// reports whether containerd or kubelet configuration differed from what was
// there, so a reapply against a running node restarts only what changed.
func (n *Node) Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	gs := n.gs.NodeStart
	containerd := nodestart.ConfigureContainerdTracked(gs)
	kubelet := nodestart.ConfigureKubeletTracked(gs)

	if err := phases.Parallel(n.p.Log, containerd, kubelet, nodestart.ConfigureLocalDNS(gs)).Do(ctx); err != nil {
		return false, false, err
	}

	if err := n.setupNetwork(ctx); err != nil {
		return false, false, err
	}

	return containerd.Changed(), kubelet.Changed(), nil
}

// Running reports whether the container is up.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := container.ValidSlot(n.slot); err != nil {
		return false, err
	}

	st, err := n.backend.Status(ctx, n.root())
	if err != nil {
		return false, err
	}

	return st.Running, nil
}

type funcTask struct {
	name string
	do   func(context.Context) error
}

func (t funcTask) Name() string                 { return t.name }
func (t funcTask) Do(ctx context.Context) error { return t.do(ctx) }

// Start brings the node up: starts the container, waits for systemd inside it,
// then starts containerd and kubelet and waits for kubelet to bootstrap. When
// the container was already running and its configuration changed, the changed
// service is restarted, containerd first.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	gs := n.gs.NodeStart

	return phases.Serial(n.p.Log,
		funcTask{"start-container", func(ctx context.Context) error { return n.backend.Start(ctx, n.root()) }},
		funcTask{"wait-container-ready", n.waitReady},
		funcTask{"wait-localdns", n.waitLocalDNS},
		nodestart.StartContainerd(n.p.Log, gs),
		nodestart.ImportContainerImages(n.p.Log, gs),
		nodestart.StartKubelet(n.p.Log, gs),
		funcTask{"restart-reconfigured-services", func(ctx context.Context) error {
			return n.restartChanged(ctx, wasRunning, containerdChanged, kubeletChanged)
		}},
		nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, gs),
	).Do(ctx)
}

func (n *Node) restartChanged(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if !wasRunning {
		// This start booted the container, so the services have already read
		// the configuration written before it.
		return nil
	}

	for _, u := range []struct {
		unit    string
		changed bool
	}{{goalstates.SystemdUnitContainerd, containerdChanged}, {goalstates.SystemdUnitKubelet, kubeletChanged}} {
		if !u.changed {
			continue
		}

		n.p.Log.Info("configuration changed on a running node, restarting service", "unit", u.unit, "slot", n.slot)

		if _, err := n.backend.Exec(ctx, n.root(), isolation.ExecSpec{Args: []string{"systemctl", "restart", u.unit}}); err != nil {
			return fmt.Errorf("systemctl restart %s in %s: %w", u.unit, n.slot, err)
		}
	}

	return nil
}

func (n *Node) waitLocalDNS(ctx context.Context) error {
	if n.p.WaitLocalDNS != nil {
		return n.p.WaitLocalDNS(ctx)
	}

	return nodestart.WaitForLocalDNS(n.p.Log, n.gs.NodeStart).Do(ctx)
}

// waitReady waits until systemd inside the container accepts commands and has
// finished starting. PROVISIONAL: "running" and "degraded" are systemd's
// SystemState values once startup is done; a degraded system is accepted
// because an unrelated failed unit must not block the node.
func (n *Node) waitReady(ctx context.Context) error {
	return waitSystemd(ctx, n.backend, n.root(), n.p.PollInterval, n.p.ReadyTimeout)
}

// waitSystemd polls systemd in the container until it has finished booting.
func waitSystemd(ctx context.Context, b *container.Backend, root isolation.NodeRoot, interval, timeout time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}

	if timeout <= 0 {
		timeout = 2 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last string

	for {
		res, err := b.Exec(ctx, root, isolation.ExecSpec{Args: []string{"systemctl", "show", "--property=SystemState", "--value"}})

		switch state := strings.TrimSpace(res.Stdout); {
		case err != nil:
			last = err.Error()
		case state == "running" || state == "degraded":
			return nil
		default:
			last = "system state " + state
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s not ready after %s: %s", root.Slot(), timeout, last)
		case <-time.After(interval):
		}
	}
}

// Stop gracefully stops the node: kubelet and containerd inside first so the
// container stop does not have to kill them, then the container. Stopping a
// stopped node succeeds.
func (n *Node) Stop(ctx context.Context) error {
	if err := container.ValidSlot(n.slot); err != nil {
		return err
	}

	running, err := n.Running(ctx)
	if err != nil {
		return err
	}

	if running {
		for _, unit := range []string{goalstates.SystemdUnitKubelet, goalstates.SystemdUnitContainerd} {
			if _, err := n.backend.Exec(ctx, n.root(), isolation.ExecSpec{Args: []string{"systemctl", "stop", unit}}); err != nil {
				n.p.Log.Warn("failed to pre-stop service (proceeding anyway)", "slot", n.slot, "unit", unit, "error", err)
			}
		}
	}

	return n.backend.Stop(ctx, n.root())
}

// Restart restarts the container only (soft reboot): the unit's hooks
// regenerate the bundle, systemd inside starts the enabled containerd and
// kubelet, and this waits for LocalDNS and an active kubelet. The host is not
// rebooted.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return phases.Serial(n.p.Log,
		funcTask{"restart-container", func(ctx context.Context) error { return n.backend.Restart(ctx, n.root()) }},
		funcTask{"wait-container-ready", n.waitReady},
		funcTask{"wait-localdns", n.waitLocalDNS},
		nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet),
	).Do(ctx)
}

// Destroy stops the node and removes its unit, bundle and rootfs.
func (n *Node) Destroy(ctx context.Context) error {
	if err := container.ValidSlot(n.slot); err != nil {
		return err
	}

	return n.backend.Destroy(ctx, n.root())
}

// Resume starts a slot that was stopped, for rollback: it restores the shared
// LocalDNS network, starts the container and waits for kubelet to be active.
// The slot's rootfs and unit are expected to be intact.
func (n *Node) Resume(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	return phases.Serial(n.p.Log,
		funcTask{"setup-localdns-network", n.setupNetwork},
		funcTask{"start-container", func(ctx context.Context) error { return n.backend.Start(ctx, n.root()) }},
		funcTask{"wait-container-ready", n.waitReady},
		funcTask{"wait-localdns", n.waitLocalDNS},
		nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet),
	).Do(ctx)
}

// RewriteUnit re-renders the supervising unit for the current helper path and
// reloads systemd without restarting the container.
func (n *Node) RewriteUnit(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.installHelper(ctx); err != nil {
		return err
	}

	return n.backend.RewriteUnit(ctx, n.slot)
}

func (n *Node) unmountKubelet(ctx context.Context) error {
	td := &noneops.Teardown{Log: n.p.Log, MountInfo: n.p.MountInfoPath}
	if n.p.Run != nil {
		td.Run = none.Runner(n.p.Run)
	}

	return td.UnmountBelow(ctx, "/var/lib/kubelet")
}

func (n *Node) setupNetwork(ctx context.Context) error {
	if n.p.SetupNetwork != nil {
		return n.p.SetupNetwork(ctx)
	}

	return nodestart.SetupLocalDNSNetwork(n.p.Log, n.gs.NodeStart).Do(ctx)
}

func (n *Node) cleanupNetwork(ctx context.Context) error {
	if n.p.CleanupNetwork != nil {
		return n.p.CleanupNetwork(ctx)
	}

	return reset.CleanupNetwork(n.p.Log).Do(ctx)
}

// resetSharedKubeletState prepares the shared kubelet directory for a new slot.
// Pod volume mounts propagate from the stopped container to the host under
// /var/lib/kubelet and outlive it, so they are unmounted, and the previous
// kubelet identity is removed when the node authenticates with a bootstrap
// token, so the new slot bootstraps fresh credentials as a new nspawn machine
// does. Direct kubeconfig and exec credential modes are rewritten by Configure.
// PROVISIONAL: propagation of pod mounts to the host (rshared through the host
// root mount) is the design's assumption.
func (n *Node) resetSharedKubeletState(ctx context.Context) error {
	if err := n.unmountKubelet(ctx); err != nil {
		return err
	}

	k := n.gs.NodeStart.Kubelet
	if k.BootstrapToken == "" || len(k.KubeconfigData) > 0 || k.ExecCredential != nil {
		return nil
	}

	for _, p := range []string{goalstates.KubeletKubeconfigPath, kubeletClientPKIDir} {
		if err := os.RemoveAll(n.gs.NodeStart.Root().Path(p)); err != nil {
			return fmt.Errorf("remove previous kubelet credentials %s: %w", p, err)
		}
	}

	return nil
}

// Upgrade moves the node from old to this node's slot (blue/green): the new
// slot is built beside the running old one, then the old slot is stopped, host
// networking is cleaned as repave does, the shared kubelet state is reset, and
// the new slot is configured, started and waited on. The caller has already
// drained the node and removed it from the cluster, as for an nspawn repave,
// persists the applied config after a nil return, and then retires old.
//
// If anything fails the new slot is removed and the old one is started again,
// and the returned error says whether that worked. The files both slots share
// at the same host paths, such as the kubelet identity, are saved first (see
// confsnap) and restored before the old slot starts again.
func (n *Node) Upgrade(ctx context.Context, old *Node) (err error) {
	if n.slot == old.slot {
		return fmt.Errorf("upgrade needs a different slot than the running %s", old.slot)
	}

	// A slot left by an earlier failed or retired upgrade must not be reused:
	// its rootfs may be a different image. The old slot is untouched here.
	if n.backend.SlotExists(n.slot) {
		if err := n.Destroy(ctx); err != nil {
			return fmt.Errorf("remove stale slot %s: %w", n.slot, err)
		}
	}

	dir := n.configSnapshotDir()
	if err := confsnap.Take(dir, n.p.HostRoot, confsnap.SharedPaths()); err != nil {
		return fmt.Errorf("save shared configuration: %w", err)
	}

	n.snapshot = dir

	defer func() {
		// A snapshot that is still set was not restored, and is kept for that.
		if err == nil && n.snapshot != "" {
			if derr := confsnap.Discard(n.snapshot); derr != nil {
				n.p.Log.Warn("could not remove the saved shared configuration", "error", derr)
			}

			n.snapshot = ""
		}
	}()

	if _, err := n.Prepare(ctx); err != nil {
		// The old slot has not been touched.
		return n.abandon(ctx, fmt.Errorf("prepare slot %s: %w", n.slot, err))
	}

	if err := old.Stop(ctx); err != nil {
		return n.rollback(ctx, old, fmt.Errorf("stop slot %s: %w", old.slot, err))
	}

	if err := n.cleanupNetwork(ctx); err != nil {
		return n.rollback(ctx, old, fmt.Errorf("clean up network: %w", err))
	}

	if err := n.Activate(ctx); err != nil {
		return n.rollback(ctx, old, err)
	}

	return nil
}

// Activate brings a prepared slot up as the node's only running slot: it resets
// the shared kubelet state so the new kubelet bootstraps fresh credentials,
// configures the node, starts it and waits for kubelet to be active. The caller
// has stopped whatever ran before and cleaned host networking. It is the tail
// of Upgrade, shared with an isolation migration into this mode, and removes
// nothing on failure: the caller decides how to roll back.
func (n *Node) Activate(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.resetSharedKubeletState(ctx); err != nil {
		return err
	}

	containerdChanged, kubeletChanged, err := n.Configure(ctx)
	if err != nil {
		return fmt.Errorf("configure slot %s: %w", n.slot, err)
	}

	if err := n.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
		return fmt.Errorf("start slot %s: %w", n.slot, err)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, goalstates.SystemdUnitKubelet).Do(ctx)
}

// abandon removes the half built new slot after a failure before the old slot
// was touched.
func (n *Node) abandon(ctx context.Context, cause error) error {
	var errs []error

	if err := n.Destroy(context.WithoutCancel(ctx)); err != nil {
		errs = append(errs, fmt.Errorf("remove slot %s: %w", n.slot, err))
	}

	if err := n.restoreShared(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(append([]error{cause}, errs...)...)
}

func (n *Node) configSnapshotDir() string {
	if n.p.ConfigSnapshotDir != "" {
		return n.p.ConfigSnapshotDir
	}

	return filepath.Join(n.p.HostRoot, goalstates.AgentConfigDir, "container-upgrade-config")
}

// restoreShared puts the saved shared configuration back and drops the
// snapshot. A failure keeps it.
func (n *Node) restoreShared() error {
	if n.snapshot == "" {
		return nil
	}

	if err := confsnap.Restore(n.snapshot, n.p.HostRoot); err != nil {
		return fmt.Errorf("restore shared configuration: %w", err)
	}

	if err := confsnap.Discard(n.snapshot); err != nil {
		n.p.Log.Warn("could not remove the saved shared configuration", "error", err)
	}

	n.snapshot = ""

	return nil
}

// rollback removes the new slot and starts the old one again.
func (n *Node) rollback(ctx context.Context, old *Node, cause error) error {
	ctx = context.WithoutCancel(ctx)

	n.p.Log.Error("upgrade failed, restoring the previous slot", "failed_slot", n.slot, "restore_slot", old.slot, "error", cause)

	var errs []error

	if err := n.Destroy(ctx); err != nil {
		errs = append(errs, fmt.Errorf("remove slot %s: %w", n.slot, err))
	}

	if err := n.cleanupNetwork(ctx); err != nil {
		errs = append(errs, fmt.Errorf("clean up network: %w", err))
	}

	// The new slot is gone, so nothing writes the shared files any more, and the
	// old slot starts on the files it was running with.
	if err := n.restoreShared(); err != nil {
		errs = append(errs, err)
	}

	if err := old.Resume(ctx); err != nil {
		errs = append(errs, fmt.Errorf("restart previous slot %s: %w", old.slot, err))
	}

	if len(errs) > 0 {
		return errors.Join(append([]error{cause}, errs...)...)
	}

	return fmt.Errorf("%w (previous slot %s restored)", cause, old.slot)
}
