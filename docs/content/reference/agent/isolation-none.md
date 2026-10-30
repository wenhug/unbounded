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
**Availability.** The `none` backend is registered with the agent and wired into
bootstrap, soft reboot, upgrade, reset, `exec` and `logs`. It has been exercised
with unit tests and the shared backend conformance suite against stubbed systemd
and containerd, not yet against a real host, so treat it as experimental until
your release notes say otherwise. Moving a host to or from `none` is a
reprovision; `IsolationMigrate` between modes is not wired to host operations.
{{< /callout >}}

## Requirements

- Writable `/etc`, `/opt`, and `/var`. `/usr` is not modified.
- `systemctl` on the host.
- **No kubelet or containerd that the agent does not own.** Preflight fails if it
  finds, for the standard names, a `kubelet.service` or `containerd.service`
  unit file in the systemd unit directories, a `kubelet` or `containerd`
  executable on the standard binary paths, or a running `kubelet` or
  `containerd` process. Remove them, or use `nspawn` or `container`.
- A read-only `/usr` is not a supported target. Use `container` there.
- Units start after `network-online.target`.

Preflight reads and mutates nothing on the host.

## Selecting the mode

Set `spec.isolation: none` on the `Machine`, `Isolation` to `none` in the agent
config, or pass `--isolation none` to `kubectl unbounded machine manual-bootstrap`.

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

The same-path contract holds: `/run/containerd`, `/etc/containerd/certs.d`,
`/var/lib/kubelet`, `/opt/cni`, and bpffs resolve for the node stack exactly as in
the other modes, so unbounded-net-node and gantry are unchanged.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation (the same nftables, sysctl, swap and docker phases as `nspawn`, plus loading `overlay` and `br_netfilter`), preflight, install the version directory and units, reload systemd, point `current` at the version, write the configuration at the shared host paths, start the units, wait for kubelet to bootstrap. |
| Start | Enable and start LocalDNS (and wait for it), then containerd (and import preloaded images), then kubelet. |
| Stop | Stop kubelet, then containerd, then LocalDNS. Every unit is attempted even if one fails. |
| Soft reboot (`NodeReboot`) | Restart LocalDNS, containerd, then kubelet, wait for LocalDNS and for kubelet to be active. The host does not reboot. |
| Repave and upgrade | The new version is installed beside the running one. Then the units stop, host networking is cleaned as for an `nspawn` repave, kubelet's old credentials are removed so it bootstraps again, `current` flips, the units start, and the agent waits for LocalDNS and kubelet. If any step after the flip fails, the link is flipped back and the previous version is started. The node is briefly down. The caller drains and deletes the Node first, as for `nspawn`. |
| Reset and delete | Same order as `nspawn`: stop the units, remove WireGuard keys, remove the configuration the agent wrote at shared paths, end leftover container shims, remove the units, drop-ins and `/opt/unbounded/node`, unmount and remove `/var/lib/kubelet`, `/var/lib/containerd`, `/etc/cni/net.d` and `/opt/cni/bin`, then the host-global cleanup. |
| Health | Both units are active, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs on the host. `unbounded-agent logs` reads the journal of the agent-owned units; `--unit kubelet` and `--unit containerd` select them. |
| GPU and devices | Native. Nothing is bind-mounted because the stack sees the host directly. See [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs as the `unbounded-localdns.service` host unit and the LocalDNS network unit is unchanged. The host's own `/etc/resolv.conf` is not rewritten; kubelet uses its dedicated resolver file. |
| Uninstall | The uninstall script reads the mode from the applied config and runs the `none` cleanup. |

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

## Limits

- **No filesystem isolation.** The node stack shares the host filesystem and
  process table. Host packages and the node stack can conflict, which is why
  preflight refuses an existing kubelet or containerd.
- **No per-machine bpffs.** The host `/sys/fs/bpf` is used directly. eBPF CNIs
  such as Cilium that expect a machine-scoped bpffs see the host's. This is a
  documented difference from `nspawn`.
- **Upgrades are not blue/green.** The node is briefly down during the flip.
  Automatic rollback restores the previous binaries. Configuration files live at
  the shared host paths and are not versioned, so a rollback does not restore the
  previous configuration.
- **Pods outlive the units.** containerd runs with `KillMode=process`, so a plain
  unit stop leaves pods running. Reset ends the shims; an upgrade does not need
  to because the Node is drained first.
- **No NVIDIA setup.** The nspawn lifecycle hooks that wire NVIDIA libraries do
  not run. The host driver and container toolkit must be installed on the host.
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
