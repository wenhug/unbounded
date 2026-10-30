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
**Provisional and experimental.** The `container` backend is registered with the
agent and wired into bootstrap, soft reboot, repave, reset, `exec` and `logs`.
It has been exercised with unit tests and the shared backend conformance suite
against a stubbed runtime and systemd, never against a real `runc`, systemd, or
Azure Container Linux node. It stays provisional until facts about those nodes
are verified: kernel and runtime versions, SELinux mode, kernel module loading,
mount propagation, and whether `systemd-nspawn` and `machinectl` ship on the
image. Details here may change. Do not rely on it for production hosts.
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
- A loadable `runc`. The agent downloads it to
  `/opt/unbounded/runtime/runc` (the version and download source follow the
  configured `CRI.Runc` version and download override, and offline artifacts),
  and only in this mode. `crun` is not supported yet. The host's own docker and
  containerd are not used, and the host's containerd and kubelet units are
  disabled because the container shares `/run/containerd` with the host.
- cgroup v2.
- Kernel modules the node stack needs. The agent loads them on the host and
  lists them in `/etc/modules-load.d/unbounded-kubernetes.conf`, because the
  container does not load modules at boot. `/lib/modules` is bound read-only
  into the container when it exists.
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
runtime bundle (`config.json`) under `/var/lib/unbounded/containers/bundles/<slot>`,
and runs it with `runc`. An agent-owned unit,
`unbounded-node-<slot>.service`, supervises the container. Slots are `kube1` and
`kube2`, as in `nspawn`. The in-rootfs configuration (containerd, kubelet,
CNI, LocalDNS) is written by the same phases the `nspawn` mode uses, against the
container root, and commands inside the container run through `runc exec`.

Paths under the bind-mounted same-path directories (`/var/lib/kubelet`,
`/opt/cni`, `/etc/containerd/certs.d`, `/run/containerd`, `/sys/fs/bpf`) are
written by the agent on the host side, because a bind mount hides anything
written below it in the rootfs. Both slots share these directories.

The container is privileged and shares the host network and, as needed, PID
namespaces. It bind-mounts the same-path contract directories and uses
`rshared` propagation for `/var/lib/kubelet`. Device and cgroup rules come from
the same GPU, device, and mount grants that `nspawn` renders as `.nspawn` binds.
When SELinux is enforcing, the process label is `spc_t`.

Lifecycle reconciliation (pre-start, post-start, reconcile) runs through a hidden
generic `node-lifecycle` command, invoked by the unit from the agent's
rollback-stable helper copy. Pre-start regenerates the bundle, so SELinux or
device changes are picked up on every start. The `nspawn-lifecycle` command and
its config are untouched. Hook commands supplied through the nspawn lifecycle
configuration are not used by this mode.

## Operations

| Operation | Behavior |
|---|---|
| Bootstrap | Host preparation, unpack the rootfs to a slot, generate the OCI bundle, write the unit, start the container, start the stack inside. |
| Start | Start the container unit, then the in-container units. |
| Stop | Stop the in-container units, then the container. |
| Soft reboot (`NodeReboot`) | Restart the container unit, which reruns the lifecycle hooks. The host does not reboot. |
| Repave and upgrade | Blue/green. The alternate slot is built beside the running one, then the old container is stopped, host networking is cleaned, the shared kubelet state is reset (pod mounts unmounted, previous kubelet credentials removed when a bootstrap token is used), and the new container is started and waited on. If the new slot does not come up it is removed and the old container is started again; the error says whether that worked. The applied config moves to the new slot only after it is healthy, then the old slot is removed. |
| Reset and delete | Stop the containers, remove the bundle, rootfs, and unit of each slot, the shared kubelet and CNI directories and the host runtime, then the host-global cleanup in the usual order. |
| Health | The container is running, kubelet is healthy, and LocalDNS answers when enabled. |
| Exec and logs | `unbounded-agent exec` runs through `runc exec`. `unbounded-agent logs` runs `journalctl` in the container. |
| GPU and devices | The grants become OCI mounts and devices; the container runs with every device allowed. The NVIDIA library wiring that the `nspawn` lifecycle hook performs is not implemented for this mode, so NVIDIA GPU nodes are not supported yet. See [NVIDIA]({{< relref "reference/gpu/nvidia" >}}) and [AMD]({{< relref "reference/gpu/amd" >}}). |
| LocalDNS | CoreDNS runs in the container and the LocalDNS network unit is unchanged. |
| Uninstall | The uninstall script reads the mode from the applied config and runs the `container` cleanup. |

```bash
sudo unbounded-agent exec -- crictl ps
sudo unbounded-agent logs --unit kubelet --follow --lines 200
```

## Limits

- Provisional: every item above can change when node facts are verified.
- Unverified assumptions in the implementation: `spc_t` is accepted by the node
  policy; `rshared` on `/var/lib/kubelet` propagates through the host root mount;
  `SIGRTMIN+3` through `runc` stops systemd as PID 1; the capability, device and
  mount set in `config.json` is enough for kubelet, containerd, and the CNI; a
  process in the container has the bundle rootfs as its root in `/proc`; the
  runtime path and the `/lib/modules` bind are right for the image.
- Both slots share the same-path directories, so a repave rewrites the kubelet
  configuration and CNI files the previous slot also sees, and a rollback
  restarts the previous slot against those new files.
- NVIDIA GPU setup, `IsolationMigrate` to or from this mode, and `crun` are not
  implemented.
- Whether Azure Container Linux accepts user-supplied Ignition when AKS
  provisions the node is unverified, as is whether a sysext would be a better
  delivery path.
- Migration to and from `nspawn` can be in place; any move involving `none` is a
  reprovision. See [Migrating Between Modes]({{< relref "reference/agent/isolation-migration" >}}).

## Troubleshooting

| Symptom | Check |
|---|---|
| Preflight fails on the runtime | An existing `/opt/unbounded/runtime/runc` cannot execute. Check that `/opt/unbounded` is writable and not mounted `noexec`. A missing runtime is not an error at preflight, since bootstrap installs it. |
| Unit `unbounded-node-<slot>.service` keeps restarting | `journalctl -u unbounded-node-kube1`. The unit restarts the container; the pre-start hook regenerates the bundle and the post-start hook waits for the runtime to report it running. |
| Preflight fails on the SELinux label | The host policy does not allow the `spc_t` label. Check `getenforce` and the audit log. |
| Container starts but kubelet does not | `sudo unbounded-agent logs --unit kubelet --lines 200`, then `sudo unbounded-agent exec -- systemctl status kubelet`. |
| Missing kernel module | Confirm the module loads on the host. The container uses the host kernel and the agent loads the module list on the host. |
