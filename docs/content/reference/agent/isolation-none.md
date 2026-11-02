---
title: "none Mode"
weight: 5
description: "Run the Kubernetes node stack directly on the host as agent-owned systemd units."
---

In `none` mode there is no machine and no container. containerd and kubelet run
directly on the host as systemd units that the agent owns, installed from the
same release artifacts the agent downloads for the other modes. The agent and
its daemon run on the host, as in every mode. See
[Isolation Modes]({{< relref "reference/agent/isolation" >}}) to compare.

{{< callout type="important" >}}
`none` mode is experimental and has not yet been validated on real hosts. Moving
a host to or from `none` is a reprovision; see
[Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}}).
{{< /callout >}}

## Requirements

- Writable `/etc`, `/opt`, and `/var`. `/usr` is not modified.
- `systemctl` on the host.
- **No kubelet or containerd that the agent does not own.** Preflight fails if it
  finds, for the standard names, a `kubelet.service` or `containerd.service`
  unit file in the systemd unit directories, a `kubelet` or `containerd`
  executable on the standard binary paths, or a running `kubelet` or
  `containerd` process. Remove them, or use `nspawn` or `container`.
- `/usr` is never modified, so a read-only `/usr` is expected to work. This has
  not yet been validated on a read-only `/usr` host.
- Units start after `network-online.target`.

Preflight does not change the node. Its writability check creates and removes a
small temporary file in `/etc`, `/var` and the node base directory (or the
nearest existing parent of it).

## Selecting the mode

Set `spec.isolation: none` on the `Machine`, `Isolation` to `none` in the agent
config, pass `--isolation none` to `kubectl unbounded machine manual-bootstrap`, or pass
`--isolation none` to `unbounded-agent start`.

## Layout

| Path | Description |
|---|---|
| `/opt/unbounded/node/<version>/` | Artifacts for one version. Executables are under `bin/`. |
| `/opt/unbounded/node/current` | Relative symlink to the active version. Units run binaries through `current/bin`, so flipping the link changes what they run. |
| `/etc/systemd/system/unbounded-containerd.service` | containerd, agent-owned. |
| `/etc/systemd/system/unbounded-kubelet.service` | kubelet, agent-owned. Wants and starts after the containerd unit. |
| `/etc/systemd/system/unbounded-localdns.service` | CoreDNS, agent-owned, when LocalDNS is enabled. Starts before containerd and kubelet. |
| `/etc/systemd/system/<unit>.d/` | Drop-ins for the agent-owned units: `PATH` with the active version first, kubelet environment, LocalDNS ordering. |
| `/opt/unbounded/node/<version>/libexec/` | The LocalDNS supervisor script for that version. |

The applied config is recorded under the slot name `kube1` and has no blue/green
slot; the version directory plays that role. The version directory is named for
the Kubernetes version plus a short digest of the other artifact versions, so an
unchanged configuration reuses its directory.

The agent never writes or edits the distro's `kubelet.service` or
`containerd.service`.

The same-path contract holds, as in every mode; see
[Isolation Modes]({{< relref "reference/agent/isolation" >}}).

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation (the same nftables, sysctl, swap and docker phases as `nspawn`, plus loading `overlay` and `br_netfilter`), preflight, install the version directory and units, reload systemd, point `current` at the version, write the configuration at the shared host paths, start the units, wait for kubelet to bootstrap. |
| Start | Enable and start LocalDNS (and wait for it), then containerd (and import preloaded images), then kubelet. |
| Stop | Stop kubelet, then containerd, then LocalDNS. Every unit is attempted even if one fails. |
| Soft reboot (`NodeReboot`) | Restart LocalDNS, containerd, then kubelet, wait for LocalDNS and for kubelet to be active. The host does not reboot. |
| Repave and upgrade | The new version is installed beside the running one. Then the units stop, host networking is cleaned as for an `nspawn` repave, kubelet's old credentials are removed so it bootstraps again, `current` flips, the units start, and the agent waits for LocalDNS and kubelet. If any step after the flip fails, the link is flipped back and the previous version is started. The node is briefly down. The caller drains and deletes the Node first, as for `nspawn`. A reconfiguration that turns LocalDNS off stops and disables its unit before the unit file is removed. |
| Reset and delete | Same order as `nspawn`: stop the units, remove WireGuard keys, remove the configuration the agent wrote at shared paths, end leftover container shims and every process left in the pods' cgroup (`kubepods.slice`, or `kubepods` with the cgroupfs driver), remove the units, drop-ins and `/opt/unbounded/node`, unmount and remove `/var/lib/kubelet`, `/var/lib/containerd`, `/etc/cni/net.d` and `/opt/cni/bin`, then the host-global cleanup. A data directory with a mount that cannot be unmounted is kept and the failure is reported, so a volume mounted below it is never deleted through the mount. |
| Health | Both units are active, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs on the host. `unbounded-agent logs` reads the journal of the agent-owned units; `--unit kubelet` and `--unit containerd` select them. See [Running commands and reading logs]({{< relref "reference/agent/isolation" >}}#running-commands-and-reading-logs). |
| GPU and devices | Native. Nothing is bind-mounted because the stack sees the host directly. NVIDIA GPUs need the NVIDIA Container Toolkit on the host; before each start the agent selects CDI mode and regenerates `/etc/cdi/nvidia.yaml`. Every device and mount request is either honored or refused: a device or mount at a path other than its own, and a library path, are refused. The one exception is `ReadOnly` on a mount: the stack runs on the host and nothing is mounted, so it cannot be enforced, and the grant is accepted with a logged warning. See [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs as the `unbounded-localdns.service` host unit and the LocalDNS network unit is unchanged. The host's own `/etc/resolv.conf` is not rewritten; kubelet uses its dedicated resolver file. |
| Uninstall | The uninstall script reads the mode from the applied config and runs the `none` cleanup. It is not a reset; see [What the uninstall script removes](#what-the-uninstall-script-removes). |

## What the uninstall script removes

The uninstall script is not a reset. `unbounded-agent reset` is the only way to a
clean host.

For a `none` node the script stops `unbounded-kubelet.service`,
`unbounded-containerd.service` and `unbounded-localdns.service`, then removes
those units and `unbounded-localdns-network.service`, their drop-in
directories, `/etc/cdi/nvidia.yaml` and `/opt/unbounded/node`. For every mode it
also removes the WireGuard and tunnel interfaces and keys, the nftables flush
service, `/etc/unbounded/kube`, the Kubernetes sysctl file and the Docker
`daemon.json`, unmasks Docker, restores swap from the saved `/etc/fstab.bak`,
and removes policy routing rules, `/usr/local/bin/unbounded-agent*` and
`/etc/unbounded/agent`.

It leaves, for a `none` node:

- `/var/lib/kubelet`, including the bootstrap kubeconfig and the kubelet's client
  credentials, and `/var/lib/containerd`.
- `/etc/containerd`, `/etc/kubernetes`, `/etc/cni/net.d`, `/opt/cni/bin`,
  `/etc/modules-load.d/unbounded-kubernetes.conf` and `/etc/unbounded/localdns`.
- Running pods and their containerd shims.
- `unbounded-agent-daemon.service` (it is not stopped) and the agent files under
  `/opt/unbounded/agent`.

Use `unbounded-agent reset` to remove all of these.

## Limits

- **No filesystem isolation.** The node stack shares the host filesystem and
  process table. Host packages and the node stack can conflict, which is why
  preflight refuses an existing kubelet or containerd.
- **No per-machine bpffs.** The host `/sys/fs/bpf` is used directly. eBPF CNIs
  such as Cilium that expect a machine-scoped bpffs see the host's. This is a
  documented difference from `nspawn`.
- **Upgrades are not blue/green.** The node is briefly down during the flip.
  Automatic rollback restores the previous binaries. Configuration files live at
  the shared host paths and are not versioned, so the agent saves them before an
  upgrade (the containerd, kubelet, CNI and LocalDNS configuration and the
  kubelet credentials, in `/etc/unbounded/agent/none-upgrade-config`) and puts them back,
  byte for byte, before the previous version starts again. If they cannot be put
  back the error says so and the saved copy is kept.
- **Pods outlive the units.** containerd runs with `KillMode=process`, so a plain
  unit stop leaves pods running, and ending a shim reparents the pod's processes
  instead of stopping them. Reset ends the shims and then kills the pods' whole
  cgroup (`cgroup.kill`; on a kernel without it, each process listed in the
  cgroup). An upgrade does not need to because the operator drains the Node first.
- **NVIDIA GPUs.** The host driver and the NVIDIA Container Toolkit must be
  installed on the host; the agent does not install them in this mode. The
  toolkit paths are configurable, and the agent selects CDI mode and regenerates
  the CDI spec before each start. A device or mount request the host view cannot
  honor fails `Prepare` instead of being ignored. See
  [Toolkit and CDI in `none` mode]({{< relref "reference/gpu/nvidia" >}}#toolkit-and-cdi-in-none-mode).
- **Migration.** Any move to or from `none` is a reprovision. In-place migration
  is not supported. See [Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}}).

## Troubleshooting

| Symptom | Check |
|---|---|
| Preflight reports an unmanaged kubelet or containerd | Remove the listed unit, binary, or process, or choose another mode. The message lists each finding. |
| Preflight reports a path is not writable | `/etc`, `/opt/unbounded/node`, and `/var` must be writable. Check for a read-only mount. |
| Node not Ready after an upgrade | `sudo unbounded-agent logs --unit kubelet --lines 200`. The agent flips `current` back automatically; `readlink /opt/unbounded/node/current` shows which version is active. |
| A unit is down | `systemctl status unbounded-kubelet.service unbounded-containerd.service`. |
| Cilium or another eBPF CNI cannot find its bpffs | Expected difference: `none` uses the host `/sys/fs/bpf`. |
