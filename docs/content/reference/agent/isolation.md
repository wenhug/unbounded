---
title: "Isolation Modes"
weight: 3
description: "Where the unbounded-agent runs the Kubernetes node stack: nspawn, none, or container. Compare modes and choose one."
---

The unbounded-agent turns a host into a Kubernetes worker. The agent and its
daemon always run on the host. An **isolation mode** decides only where the
node stack runs: containerd, kubelet, CNI plugins, and CoreDNS.

| Mode | Node stack runs | Status |
|---|---|---|
| [`nspawn`]({{< relref "reference/agent/isolation-nspawn" >}}) | In a systemd-nspawn machine | Default. Existing behavior. |
| [`none`]({{< relref "reference/agent/isolation-none" >}}) | Directly on the host as agent-owned systemd units | New and experimental. Wired end to end; not yet exercised on a real host. See the mode page. |
| [`container`]({{< relref "reference/agent/isolation-container" >}}) | In a privileged OCI container, for Azure Container Linux | New and provisional. Wired end to end; verified only against stubbed hosts. See the mode page. |

{{< callout type="important" >}}
Only `nspawn` is production behavior today. `none` and `container` are being
added. Each mode page states exactly what is and is not available in the agent
build you run. Hosts with no recorded mode are `nspawn` hosts, and their
behavior does not change.
{{< /callout >}}

## Comparison

| | `nspawn` | `none` | `container` |
|---|---|---|---|
| Filesystem isolation | Separate rootfs per slot | None, host filesystem | Separate rootfs per slot |
| Node stack source | OCI rootfs image, artifacts downloaded into it | Release artifacts in versioned directories, no distro packages | OCI rootfs image run by an OCI runtime |
| Host `/usr` | Writable (installs `systemd-container`) | Not modified, but a read-only `/usr` is not a supported target; use `container` | May be read-only |
| Supervised by | `systemd-nspawn@<slot>.service` | `unbounded-containerd.service`, `unbounded-kubelet.service` | `unbounded-node-<slot>.service`, an agent-owned unit running `runc` |
| Slots and upgrade | Blue/green, `kube1` and `kube2` | Versioned directories and a `current` link | Blue/green, `kube1` and `kube2` |
| Upgrade downtime | Node moves to a new slot | Brief, the node stack restarts | Node moves to a new slot |
| bpffs | Private mount per machine | Host `/sys/fs/bpf` directly | Host `/sys/fs/bpf` (unverified) |
| Network | Host network namespace | Host | Host network namespace |
| Best for | General Linux hosts | Hosts where you accept running the node stack directly on the host | Immutable hosts such as Azure Container Linux |

Across all modes these paths resolve identically for the node stack and for pods:
`/run/containerd`, `/etc/containerd/certs.d`, `/var/lib/kubelet`, `/opt/cni`, and
bpffs. unbounded-net-node and gantry do not change per mode.

## Choosing a mode

1. Start with `nspawn` unless you have a reason not to. It is the default, it is
   the most exercised, and it keeps the node stack separate from host packages.
2. Choose `container` when the host has a read-only `/usr` and cannot install
   `systemd-container`, such as Azure Container Linux. It is provisional.
3. Choose `none` when you want the node stack on the host with no machine or
   container layer, and the host has no kubelet or containerd of its own. The
   agent refuses to start on a host that already has one.
4. The mode is always explicit. The agent never auto-detects it, and preflight
   fails with a suggested mode rather than switching silently.

## Selecting a mode

Set the mode at provisioning time with any of:

- `spec.isolation` on the `Machine` (`nspawn`, `none`, or `container`).
- `--isolation` on `kubectl unbounded machine manual-bootstrap`.
- `Isolation` in the [agent config]({{< relref "reference/agent/configuration" >}}).

An empty value is `nspawn`. The agent rejects any unrecognized value with a
clear error rather than running it as `nspawn`.

```bash
kubectl unbounded machine manual-bootstrap my-node --site mysite \
  --isolation container | ssh user@host sudo bash
```

Changing `spec.isolation` on a running Machine does not move the node. To switch
a live host, run an `IsolationMigrate` operation; see
[Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}}),
which also lists the extra cluster permission the drain needs.

## Operations by mode

| Operation | `nspawn` | `none` | `container` |
|---|---|---|---|
| Preflight | `systemd-container` and the nspawn runtime | No unmanaged kubelet or containerd, writable `/etc`, `/opt/unbounded/node`, `/var` | cgroup v2, SELinux label, writable `/etc` `/opt` `/var` (the `runc` runtime is installed by bootstrap) |
| Bootstrap | Unpack rootfs to a slot, render nspawn config, start machine, start stack inside | Install versioned artifacts, write units, start units | Unpack rootfs to a slot, generate an OCI bundle, write unit, start container, start stack inside |
| Start | `machinectl start`, then in-machine units | Start host units in order | Start container unit, then in-container units |
| Stop | Stop kubelet and containerd in the machine, stop the machine | Stop host units in reverse order | Stop in-container units, stop the container |
| Soft reboot (`NodeReboot`) | Restart the machine unit | Restart containerd, then kubelet | Restart the container unit |
| Repave and upgrade | Blue/green into the alternate slot | Flip `current`, automatic rollback on failure, brief downtime | Blue/green into the alternate slot |
| Reset and delete | Stop slots, remove config, remove machine, clean bpffs, host-global cleanup | Stop units, remove versioned directories and units, host-global cleanup | Stop container, remove bundle, rootfs, and unit, host-global cleanup |
| Health | Machine registered, kubelet healthy, LocalDNS | Units active, kubelet healthy, LocalDNS | Container running, kubelet healthy, LocalDNS |
| Exec and logs | `systemd-run --machine`, machine journal | Host exec, `journalctl` | Runtime exec, container journal |
| GPU and devices | Bind-mounts and `DeviceAllow=` | Native | OCI mounts and devices (NVIDIA wiring not implemented) |
| LocalDNS | CoreDNS in the machine | CoreDNS host unit | CoreDNS in the container |
| Agent binary upgrade | Unchanged | Unchanged | Unchanged |
| Uninstall | Mode read from the applied config | Same | Same |

Host-global state is mode independent. The agent owns nftables flush, the
LocalDNS dummy interface and rules, WireGuard keys, sysctls, and bpffs mounts in
every mode; a backend only declares ordering dependencies on them.

## Running commands and reading logs

`unbounded-agent exec` and `unbounded-agent logs` work in every mode and replace
mode-specific recipes such as `machinectl shell`:

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

`exec` runs the command after `--` in the node stack. `logs` streams the journal
for a unit in the node stack: `--unit` selects it, `--follow` keeps the stream
open, and `--lines` limits output to the last N entries.

{{< callout type="note" >}}
`exec` and `logs` are new. A host whose agent build predates them still needs
the mode-specific recipes on the mode pages.
{{< /callout >}}

## Drift

If `spec.isolation` differs from the mode recorded in the host's applied config,
the Machine is drifted. Nothing moves until an `IsolationMigrate` operation runs.
Machine status reporting for this drift and the Machina check that withholds a
non-default mode from agents that do not support it are part of the design and
are not available in every build.

## See also

- [nspawn Mode]({{< relref "reference/agent/isolation-nspawn" >}})
- [none Mode]({{< relref "reference/agent/isolation-none" >}})
- [container Mode]({{< relref "reference/agent/isolation-container" >}})
- [Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}})
- [Agent Configuration]({{< relref "reference/agent/configuration" >}})
