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
**Availability.** The `none` backend core exists: versioned install layout,
units, preflight checks, start, stop, restart, upgrade with rollback, and logs.
It is not yet registered with the agent. The daemon's node operations are still
being made mode aware, so an agent build without that wiring rejects
`Isolation: none` with an error that the mode has no registered backend, and an
`IsolationMigrate` to or from `none` is not available. Treat this page as the
definition of the mode and check your build's release notes before selecting it.
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

The same-path contract holds: `/run/containerd`, `/etc/containerd/certs.d`,
`/var/lib/kubelet`, `/opt/cni`, and bpffs resolve for the node stack exactly as in
the other modes, so unbounded-net-node and gantry are unchanged.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation, preflight, install the version directory and units, reload systemd, point `current` at the version, start the units. |
| Start | Enable and start containerd, then kubelet. |
| Stop | Stop kubelet, then containerd. Every unit is attempted even if one fails. |
| Soft reboot (`NodeReboot`) | Restart containerd, then kubelet. The host does not reboot. |
| Repave and upgrade | Stop the units, flip `current` to the new version, start the units, and wait for kubelet Ready. If any step after the flip fails, the link is flipped back and the previous version is started. The node is briefly down. |
| Reset and delete | Stop and disable the units, remove the unit files and the whole `/opt/unbounded/node` tree, reload systemd, then the host-global cleanup. |
| Health | Both units are active, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs on the host. `unbounded-agent logs` reads the journal of the agent-owned units; `--unit kubelet` and `--unit containerd` select them. |
| GPU and devices | Native. Nothing is bind-mounted because the stack sees the host directly. See [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs as a host unit and the LocalDNS network unit is unchanged. The CoreDNS host unit is part of the design and is not in the backend core. |
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
  Automatic rollback restores the previous version.
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
