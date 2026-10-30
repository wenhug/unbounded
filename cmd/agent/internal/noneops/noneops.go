// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package noneops is the composition root for the none isolation backend. The
// backend package cannot import the phase packages that install and configure
// the node stack, so this package resolves the goal state for a host-native
// node, binds the phases to it and injects them into none.Backend as hooks.
//
// Layout of a none node. The agent owns /opt/unbounded/node: each installed
// version is a directory holding bin and libexec, and a current symlink selects
// the active one. Everything else is at the same host path the nspawn machine
// sees (/etc/containerd, /var/lib/kubelet, /opt/cni, /run/containerd), so
// unbounded-net-node, gantry and their charts do not change per mode.
//
// Slot. A none node has no blue/green slot. The slot recorded in applied-config
// (the file name <slot>-applied-config.json) is always kube1, so discovery,
// the uninstall script and the appliedconfig reader work unchanged. The NodeRoot
// slot is the version directory name, which is what Upgrade flips.
package noneops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Slot is the slot name a none node records its applied config under.
const Slot = goalstates.NSpawnMachineKube1

// Params bind a Node to one agent configuration.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides

	// Layout is the agent-owned directory. The zero value is the default.
	Layout none.Layout
	// SystemdDir is where units are written. Empty means /etc/systemd/system.
	SystemdDir string
	// Run executes host commands. Nil uses the real host.
	Run none.Runner
	// Proc is the procfs Destroy scans for container shims. Empty means /proc.
	Proc string

	// GoalState, when set, is used instead of resolving one. Bootstrap passes
	// the goal state it has already resolved so GPU and device discovery happens
	// once.
	GoalState *goalstates.MachineGoalState
	// Resolve overrides goal state resolution. Tests use it; the default is
	// goalstates.ResolveMachine.
	Resolve func(slot string) (*goalstates.MachineGoalState, error)

	// HostRoot prefixes the same-path host locations (/etc, /var/lib, /opt/cni)
	// the node writes. Empty is the real host; tests use a temporary tree.
	HostRoot string

	// Download replaces the artifact installers. Nil downloads for real.
	Download func(ctx context.Context) error
	// SetupNetwork and CleanupNetwork replace the shared LocalDNS network
	// phases. Nil runs the real ones.
	SetupNetwork   func(ctx context.Context) error
	CleanupNetwork func(ctx context.Context) error

	// ConfigSnapshotDir is where Upgrade saves the shared configuration so a
	// rollback can restore it. Empty means a directory in the agent configuration
	// directory below HostRoot.
	ConfigSnapshotDir string

	// SetupGPU replaces the host GPU setup that runs before the stack starts.
	SetupGPU func(ctx context.Context) error
	// WaitLocalDNS and WaitKubelet override the readiness waits. Nil uses the
	// phases the nspawn mode uses.
	WaitLocalDNS func(ctx context.Context) error
	WaitKubelet  func(ctx context.Context) error
}

// Node is a none node bound to one configuration.
type Node struct {
	p Params

	once    sync.Once
	gs      *goalstates.MachineGoalState
	err     error
	backend *none.Backend
	version string
}

// New returns a Node. Goal state resolution is deferred to first use so that
// operations that never need it (Stop, Status) work without a configuration.
func New(p Params) *Node {
	if p.Log == nil {
		p.Log = slog.Default()
	}

	return &Node{p: p}
}

// Version is the version directory name of this configuration.
func (n *Node) Version() (string, error) {
	if err := n.resolve(); err != nil {
		return "", err
	}

	return n.version, nil
}

// GoalState is the resolved goal state with the host-native overrides applied.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

// Backend is the none backend wired to this node's phases.
func (n *Node) Backend() (*none.Backend, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend, nil
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		n.gs, n.err = n.resolveGoalState()
		if n.err != nil {
			return
		}

		n.version = VersionFor(n.gs.RootFS)
		n.backend = n.newBackend()
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

	gs, err := resolve(Slot)
	if err != nil {
		return nil, fmt.Errorf("resolve machine goal state: %w", err)
	}

	return gs, nil
}

// applyOverrides points the goal state at the host: the root maps installer
// directories into the version, commands run on the host, and the binaries the
// generated configuration names are reached through the current link.
func (n *Node) applyOverrides() {
	root := n.root()

	n.gs.RootFS.RootOverride = root
	n.gs.NodeStart.RootOverride = root
	n.gs.NodeStart.ExecOverride = func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
		res, err := n.backend.Exec(ctx, root, isolation.ExecSpec{Args: args})

		return res.Stdout, err
	}

	bin := n.p.Layout.BinDir()
	n.gs.NodeStart.Containerd.ContainerdBinPath = filepath.Join(bin, "containerd")
	n.gs.NodeStart.Containerd.RuncBinaryPath = filepath.Join(bin, "runc")
	n.gs.NodeStart.Kubelet.KubeletBinPath = filepath.Join(bin, "kubelet")
}

func (n *Node) root() none.Root {
	return none.Root{Layout: n.p.Layout, Version: n.version, HostRoot: n.p.HostRoot}
}

// VersionFor names the version directory for a rootfs goal state: the
// Kubernetes version plus a digest of everything else that decides which
// binaries are installed, so changing only the containerd or CNI version, the
// architecture or a download override yields a new directory while an unchanged
// configuration reuses the installed one.
func VersionFor(r *goalstates.RootFS) string {
	inputs := struct {
		Containerd, RunC, CNI, CoreDNS, Arch string
		CoreDNSEnabled                       bool
		Downloads                            *goalstates.DownloadOverrides
	}{r.ContainerdVersion, r.RunCVersion, r.CNIPluginVersion, r.LocalDNS.CoreDNSVersion, r.HostArch, r.LocalDNS.Enabled, r.Downloads}

	data, _ := json.Marshal(inputs) //nolint:errcheck // plain data always marshals
	sum := sha256.Sum256(data)

	return strings.TrimPrefix(r.KubernetesVersion, "v") + "-" + hex.EncodeToString(sum[:4])
}

// setupGPU prepares the host for GPU workloads. It does nothing on a node
// without NVIDIA GPUs.
func (n *Node) setupGPU(ctx context.Context) error {
	if n.p.SetupGPU != nil {
		return n.p.SetupGPU(ctx)
	}

	return nodestart.SetupNVIDIAOnHost(n.p.Log, n.gs.NodeStart).Do(ctx)
}

func (n *Node) waitLocalDNS(ctx context.Context) error {
	if n.p.WaitLocalDNS != nil {
		return n.p.WaitLocalDNS(ctx)
	}

	return nodestart.WaitForLocalDNS(n.p.Log, n.gs.NodeStart).Do(ctx)
}

func (n *Node) waitKubelet(ctx context.Context) error {
	if n.p.WaitKubelet != nil {
		return n.p.WaitKubelet(ctx)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, none.UnitKubelet).Do(ctx)
}

func (n *Node) newBackend() *none.Backend {
	proc := n.p.Proc
	if proc == "" {
		proc = "/proc"
	}

	return &none.Backend{
		Log:        n.p.Log,
		Layout:     n.p.Layout,
		SystemdDir: n.p.SystemdDir,
		Run:        n.p.Run,
		Proc:       proc,
		Units: none.UnitSpec{
			KubeletConfigPath: goalstates.KubeletConfigurationPath,
			LocalDNS:          n.gs.NodeStart.LocalDNS.Enabled,
		},
		Install: n.install,
		// The stack is the host: GPU devices and libraries are already visible,
		// so there is nothing to bind. What GPU workloads need is the toolkit's
		// CDI spec, rebuilt at every start from the devices present then.
		BeforeStart:  n.setupGPU,
		HostRoot:     n.p.HostRoot,
		GPUHostPaths: nodestart.NVIDIAToolkitPaths(),
		AfterStart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
			none.UnitContainerd: func(ctx context.Context) error {
				return nodestart.ImportContainerImages(n.p.Log, n.gs.NodeStart).Do(ctx)
			},
		},
		AfterRestart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
		},
	}
}

// install places the version's artifacts: the same Kubernetes, CRI, CNI and
// CoreDNS installers the nspawn rootfs uses, pointed at the host root, plus the
// kernel modules the node needs and the LocalDNS supervisor script.
func (n *Node) install(ctx context.Context, _ isolation.NodeRoot) error {
	rf := n.gs.RootFS

	if n.p.Download != nil {
		if err := n.p.Download(ctx); err != nil {
			return err
		}

		return n.writeSupervisor()
	}

	tasks := []phases.Task{
		rootfs.DownloadKubeBinaries(n.p.Log, rf),
		rootfs.DownloadCRIBinaries(n.p.Log, rf),
		rootfs.DownloadCNIBinaries(n.p.Log, rf),
		rootfs.InstallCoreDNS(n.p.Log, rf),
		n.kernelModulesTask(),
	}

	if err := phases.Parallel(n.p.Log, tasks...).Do(ctx); err != nil {
		return err
	}

	return n.writeSupervisor()
}

func (n *Node) writeSupervisor() error {
	if !n.gs.RootFS.LocalDNS.Enabled {
		return nil
	}

	path := n.gs.RootFS.Root().Path(goalstates.LocalDNSSupervisorPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create LocalDNS supervisor directory: %w", err)
	}

	script := rootfs.LocalDNSSupervisorScript(filepath.Join(n.p.Layout.BinDir(), "coredns"))
	if err := os.WriteFile(path, script, 0o755); err != nil { //nolint:gosec // the supervisor must be executable
		return fmt.Errorf("write LocalDNS supervisor: %w", err)
	}

	return os.Chmod(path, 0o755) //nolint:gosec // executable by the service user
}

// Prepare installs the version and writes the units. It does not activate.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend.Prepare(ctx, isolation.NodeSpec{
		Slot:    n.version,
		Version: n.gs.RootFS.KubernetesVersion,
		Grants:  n.gs.RootFS.Grants(),
	})
}

// Provision installs the version, writes the units and points the current link
// at it. Bootstrap uses it for the first install, where there is no previous
// version to keep running.
func (n *Node) Provision(ctx context.Context) error {
	r, err := n.Prepare(ctx)
	if err != nil {
		return err
	}

	if _, err := n.backend.Activate(r); err != nil {
		return fmt.Errorf("activate %s: %w", r.Slot(), err)
	}

	return nil
}

// Configure writes the node configuration at its host paths and prepares the
// shared LocalDNS network. It reports whether containerd or kubelet
// configuration differed from what was on disk, so a reapply against a running
// node restarts only what changed.
func (n *Node) Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	gs := n.gs.NodeStart
	containerd := nodestart.ConfigureContainerdOnHost(gs)
	kubelet := nodestart.ConfigureKubeletOnHost(gs, none.UnitKubelet)

	if err := phases.Parallel(n.p.Log, containerd, kubelet, nodestart.ConfigureLocalDNSOnHost(gs)).Do(ctx); err != nil {
		return false, false, err
	}

	if err := n.setupNetwork(ctx); err != nil {
		return false, false, err
	}

	return containerd.Changed(), kubelet.Changed(), nil
}

// Running reports whether the whole stack is active.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := n.resolve(); err != nil {
		return false, err
	}

	st, err := n.backend.Status(ctx, n.root())
	if err != nil {
		return false, err
	}

	return st.Running, nil
}

// Start brings the stack up and waits for kubelet to bootstrap. When the stack
// was already running and its configuration changed, the changed service is
// restarted, containerd first.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	r := n.root()

	if err := n.backend.Start(ctx, r); err != nil {
		return err
	}

	if wasRunning {
		for _, u := range []struct {
			unit    string
			changed bool
		}{{none.UnitContainerd, containerdChanged}, {none.UnitKubelet, kubeletChanged}} {
			if !u.changed {
				continue
			}

			n.p.Log.Info("configuration changed on a running node, restarting service", "unit", u.unit)

			if _, err := n.backend.Exec(ctx, r, isolation.ExecSpec{Args: []string{"systemctl", "restart", u.unit}}); err != nil {
				return fmt.Errorf("systemctl restart %s: %w", u.unit, err)
			}
		}
	}

	return n.waitBootstrap(ctx)
}

// waitBootstrap waits for kubelet's client credentials to appear. It runs
// through a serial group so a failure carries the task name the Machine
// condition reason is classified from.
func (n *Node) waitBootstrap(ctx context.Context) error {
	return phases.Serial(n.p.Log, nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, n.gs.NodeStart)).Do(ctx)
}

// Restart restarts the node stack in place (soft reboot): LocalDNS, containerd,
// then kubelet, waiting for LocalDNS after it restarts and for kubelet to be
// active at the end. The host is not rebooted.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.backend.Restart(ctx, n.root()); err != nil {
		return err
	}

	return n.waitKubelet(ctx)
}

// Upgrade installs this configuration's version beside the active one, writes
// the new configuration, and switches over: stop the stack, clean host
// networking as repave does, flip the current link, start, and wait for kubelet.
// If the new version does not come up the link is flipped back and the previous
// version is started again. The caller has already drained the node and removed
// it from the cluster, as for an nspawn repave, and persists the applied config
// after a nil return.
//
// Configuration lives at the same host paths for every version, so the files
// the stacks share are saved before anything is written (see confsnap) and put
// back on every failure: before the flip, and before the previous version
// starts on a rollback.
func (n *Node) Upgrade(ctx context.Context) (err error) {
	if err := n.resolve(); err != nil {
		return err
	}

	snapshot := n.configSnapshotDir()

	if err := confsnap.Take(snapshot, n.p.HostRoot, confsnap.SharedPaths()); err != nil {
		return fmt.Errorf("save shared configuration: %w", err)
	}

	restored := false
	restore := func() error {
		if err := confsnap.Restore(snapshot, n.p.HostRoot); err != nil {
			return fmt.Errorf("restore shared configuration: %w", err)
		}

		restored = true

		return nil
	}

	defer func() {
		if err != nil && !restored {
			// A failure the backend did not roll back from, such as an install or
			// configuration error, or a stop that failed. The snapshot stays when
			// the restore fails, so it can be tried again.
			if rerr := restore(); rerr != nil {
				err = errors.Join(err, rerr)

				return
			}
		}

		if derr := confsnap.Discard(snapshot); derr != nil {
			n.p.Log.Warn("could not remove the saved shared configuration", "error", derr)
		}
	}()

	r, err := n.Prepare(ctx)
	if err != nil {
		return err
	}

	if _, _, err := n.Configure(ctx); err != nil {
		return err
	}

	n.backend.BeforeRollbackStart = func(context.Context) error { return restore() }

	first := true

	n.backend.BeforeUpgradeStart = func(ctx context.Context) error {
		if err := n.cleanupNetwork(ctx); err != nil {
			return err
		}

		if err := n.setupNetwork(ctx); err != nil {
			return err
		}

		if !first {
			return nil
		}

		first = false

		// A repaved node bootstraps fresh credentials, as the new nspawn machine
		// does. The old identity belongs to the Node object that was deleted.
		return n.resetKubeletIdentity()
	}

	return n.backend.Upgrade(ctx, r, func(ctx context.Context) error {
		if err := n.waitKubelet(ctx); err != nil {
			return err
		}

		return n.waitBootstrap(ctx)
	})
}

func (n *Node) configSnapshotDir() string {
	if n.p.ConfigSnapshotDir != "" {
		return n.p.ConfigSnapshotDir
	}

	return filepath.Join(n.p.HostRoot, goalstates.AgentConfigDir, "none-upgrade-config")
}

// resetKubeletIdentity removes the previous kubelet client credentials when the
// node authenticates with a bootstrap token, so the next start requests new
// ones. Direct kubeconfig and exec credential modes are rewritten by Configure.
func (n *Node) resetKubeletIdentity() error {
	k := n.gs.NodeStart.Kubelet
	if k.BootstrapToken == "" || len(k.KubeconfigData) > 0 || k.ExecCredential != nil {
		return nil
	}

	for _, p := range []string{goalstates.KubeletKubeconfigPath, "/var/lib/kubelet/pki"} {
		if err := os.RemoveAll(n.gs.NodeStart.Root().Path(p)); err != nil {
			return fmt.Errorf("remove previous kubelet credentials %s: %w", p, err)
		}
	}

	return nil
}

// PruneVersions removes every installed version except the active one. It runs
// after an upgrade succeeds, mirroring the removal of the old nspawn machine.
func (n *Node) PruneVersions() error {
	versions, err := n.p.Layout.Versions()
	if err != nil {
		return err
	}

	var errs []error

	for _, v := range versions {
		if err := n.p.Layout.Remove(v); err != nil && !strings.Contains(err.Error(), "is active") {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
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

func (n *Node) kernelModulesTask() phases.Task {
	return LoadKernelModules(n.p.Log, n.p.Run)
}

func execSpec(args ...string) isolation.ExecSpec { return isolation.ExecSpec{Args: args} }
