---
title: "container Mode (Experimental)"
weight: 6
description: "Provisional mode that runs the Kubernetes node stack in a privileged OCI container, for Azure Container Linux."
---

`container` mode runs the node stack in a privileged OCI container on hosts that
cannot run systemd-nspawn or install host packages, such as Azure Container
Linux. See [Isolation Modes]({{< relref "reference/agent/isolation" >}}) to
compare.

{{< callout type="warning" >}}
**Provisional and experimental.** This page describes the designed behavior. It
stays provisional until facts about Azure Container Linux nodes are verified on a
live node: kernel and runtime versions, SELinux mode, kernel module loading,
mount propagation, and whether `systemd-nspawn` and `machinectl` ship on the
image. The backend is not yet available in every agent build, and details here
may change. Do not rely on it for production hosts.
{{< /callout >}}

## Target host

Azure Container Linux is derived from Flatcar. These properties are confirmed by
Microsoft documentation: a read-only `/usr` protected by dm-verity, weekly
image-based updates, a signed unified kernel image with Secure Boot, and
enforcing SELinux. Public documentation does not cover container runtimes,
kernel modules, or mount propagation, so the requirements below are the
design's assumptions.

## Requirements

- Writable `/etc`, `/opt`, and `/var`. `/usr` is read-only and is not used.
- A loadable `runc` or `crun`. The agent downloads it as an artifact under
  `/opt/unbounded`. The host's own docker and containerd are not used.
- cgroup v2.
- Kernel modules the node stack needs (for example WireGuard) available to the
  container.
- With SELinux enforcing, support for the `spc_t` process label. Preflight checks
  it.
- Ignition provisioning (`ProvisioningFormat: ignition`). No new format is added.

## Selecting the mode

Set `spec.isolation: container` on the `Machine`, `Isolation` to `container` in
the agent config, or pass `--isolation container` to
`kubectl unbounded machine manual-bootstrap`.

## How it works

The agent reuses the OCI unpack step to place an `agent-*` rootfs image (with
systemd as PID 1) under `/var/lib/unbounded/containers/<slot>`, generates an OCI
runtime bundle over it, and runs it with `runc` or `crun`. An agent-owned systemd
unit supervises the container. Slots are `kube1` and `kube2`, as in `nspawn`.

The container is privileged and shares the host network and, as needed, PID
namespaces. It bind-mounts the same-path contract directories and uses
`rshared` propagation for `/var/lib/kubelet`. Device and cgroup rules come from
the same GPU, device, and mount grants that `nspawn` renders as `.nspawn` binds.
When SELinux is enforcing, the process label is `spc_t`.

Lifecycle reconciliation (pre-start, post-start, reconcile) runs through a hidden
generic `node-lifecycle` command. The `nspawn-lifecycle` command and its config
are untouched.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation, unpack the rootfs to a slot, generate the OCI bundle, write the unit, start the container, start the stack inside. |
| Start | Start the container unit, then the in-container units. |
| Stop | Stop the in-container units, then the container. |
| Soft reboot (`NodeReboot`) | Restart the container unit, which reruns the lifecycle hooks. The host does not reboot. |
| Repave and upgrade | Blue/green into the alternate slot, then clean up the old slot. |
| Reset and delete | Stop the container, remove the bundle, rootfs, and unit, then the host-global cleanup. |
| Health | The container is running, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs through the container runtime. `unbounded-agent logs` reads the container journal. |
| GPU and devices | The grants become OCI mounts, devices, and cgroup device rules. See [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). GPU support on this mode is unverified. |
| LocalDNS | CoreDNS runs in the container and the LocalDNS network unit is unchanged. |
| Uninstall | The uninstall script reads the mode from the applied config and runs the `container` cleanup. |

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

## Limits

- Provisional: every item above can change when node facts are verified.
- Requires a runtime artifact and kernel features that are not confirmed for
  Azure Container Linux.
- Whether Azure Container Linux accepts user-supplied Ignition when AKS
  provisions the node is unverified, as is whether a sysext would be a better
  delivery path.
- Migration to and from `nspawn` can be in place; any move involving `none` is a
  reprovision. See [Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}}).

## Troubleshooting

| Symptom | Check |
|---|---|
| Preflight fails on the runtime | The `runc` or `crun` artifact is missing or cannot execute. Check that `/opt/unbounded` is writable and not mounted `noexec`. |
| Preflight fails on the SELinux label | The host policy does not allow the `spc_t` label. Check `getenforce` and the audit log. |
| Container starts but kubelet does not | `sudo unbounded-agent logs --unit kubelet --lines 200`, then `sudo unbounded-agent exec -- systemctl status kubelet`. |
| Missing kernel module | Confirm the module loads on the host. The container uses the host kernel. |
