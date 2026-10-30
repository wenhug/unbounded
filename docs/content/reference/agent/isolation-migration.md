---
title: "Migrating Between Modes"
weight: 7
description: "Move a live node to another isolation mode with an IsolationMigrate MachineOperation."
---

Changing `spec.isolation` on a running Machine does not move the node. It marks
the Machine as drifted from the mode recorded on the host. The node moves only
when an `IsolationMigrate` [MachineOperation]({{< relref "guides/operations" >}})
runs.

{{< callout type="important" >}}
**Availability.** The agent validates the request, picks the strategy, and
rejects unsupported moves before touching the node. Both strategies are wired to
host operations and are tested against fake backends and stubbed hosts. They
have not been run against real hosts, and any move that involves `container` is
provisional because that mode is. Draining needs a permission the shipped
daemon role does not grant yet; see [Cluster permissions](#cluster-permissions).
{{< /callout >}}

## Create an operation

`IsolationMigrate` takes two parameters:

| Parameter | Required | Values |
|---|---|---|
| `targetMode` | Yes | `nspawn`, `none`, or `container` |
| `strategy` | No | `inPlace` or `reprovision` |

```bash
kubectl unbounded machine operation create migrate-worker-01 \
  --kind IsolationMigrate \
  --machine worker-01 \
  --param targetMode=container \
  --param strategy=inPlace \
  --wait
```

`kubectl unbounded` validates the parameters before it creates the operation. When
`strategy` is omitted, the agent chooses `inPlace` if the pair supports it and
`reprovision` otherwise.

## Supported moves

| From \ To | `nspawn` | `container` | `none` |
|---|---|---|---|
| `nspawn` | n/a | `inPlace` or `reprovision` | `reprovision` |
| `container` | `inPlace` or `reprovision` | n/a | `reprovision` |
| `none` | `reprovision` | `reprovision` | n/a |

`reprovision` is always available. Requesting `inPlace` for a pair that involves
`none` fails, because `none` has no slot to prepare beside the running node.
Requesting a move to the mode the node is already in also fails. A rejected
request marks the operation `Failed` (reasons `UnsupportedMigration` or
`InvalidParameters`) before anything is changed.

## Cluster permissions

The migration cordons and drains the node with the daemon's own credentials (the
`system:node:<name>` identity plus the `unbounded-agent-daemons` group). With the
Node authorizer enabled, which is the default on kubeadm and most managed
clusters, that identity can already read pods and patch its own Node. Eviction
is not part of it. Grant the daemon group `create` on `pods/eviction`, for
example by adding this rule to the `unbounded-daemon-controller-machine`
ClusterRole in `deploy/machina/07-bootstrapper-rbac.yaml.tmpl`:

```yaml
- apiGroups: [""]
  resources: ["pods/eviction"]
  verbs: ["create"]
```

Where the Node authorizer is not in use, also grant `get` and `list` on `pods`
and `get` and `patch` on `nodes`. Without the permission the drain fails with a
message that names it, and the migration is rolled back or never starts.

The drain follows `kubectl drain`: DaemonSet pods, mirror pods, and finished pods
are left alone, and everything else is evicted through the Eviction API, so
PodDisruptionBudgets are honored. A pod that no controller would recreate makes
the drain fail before anything is evicted; delete it or move it first. The drain
times out after ten minutes.

## In-place steps

`nspawn` and `container` can run side by side in different slots, so the agent
prepares the target while the source still runs. The target is built in the
alternate slot (`kube1` and `kube2`), from the applied config with only
`Isolation` changed.

1. Preflight the target backend. Failure here changes nothing.
2. Download the target's artifacts. Failure here changes nothing.
3. Prepare the target in the alternate slot. Leftovers of an earlier attempt in
   that slot are removed first.
4. Cordon and drain the node. A node that was already cordoned stays cordoned
   afterward.
5. Stop the source node stack.
6. Clean up host networking, as a repave does.
7. Start the target. Kubelet gets fresh credentials through the normal
   bootstrap flow: the bootstrap token in the applied config (the one a repave
   uses) is exchanged for a new kubelet client certificate. No kubelet
   credentials are copied, and a metalman-attested host is not attested again.
   The Node name, labels, and taints are kept.
8. Record the new mode and slot in the applied config.
9. Wait for kubelet Ready, for up to five minutes: first the check a repave
   makes (kubelet active on the host, with its credentials), then the Node's
   `Ready` condition through the API, which must have been reported after the
   target started so that the previous kubelet's last report is not mistaken for
   the new one.
10. On success, uncordon the node, destroy the source slot, and remove its
    applied config. `status.observedIsolation` is updated from the new applied
    config.

### Rollback

If anything fails after the target exists, the agent stops the target, removes
its applied config and restores the source's, destroys the target, restarts the
source, and uncordons the node if the migration cordoned it. The operation then
reports the failure. If the source could not be restored the error says so, the
node stays cordoned, and it needs attention. The source slot and its applied
config are kept until step 9 passes.

### Interruption

Before each step that changes the host, the daemon writes the step to
`/etc/unbounded/agent/isolation-migration.json` (atomically, next to the applied
configs). An operation that is already `InProgress` is not reconciled again, so a
daemon that restarts mid-migration resolves the journal itself when it starts,
before it takes any work, and then finishes the operation:

| Last step recorded | At the next daemon start |
|---|---|
| `preparing`, `draining` | The source never stopped. The partial target is removed and the node is uncordoned. The operation fails with "interrupted". |
| `stopping-source`, `starting-target`, `persisting` | Roll back as above: stop and remove the target, restore the source's applied config, restart the source. The operation fails with "interrupted". |
| `awaiting-ready` | Wait for Ready again. On success finish the migration; on timeout roll back. |
| `ready` | Finish the cleanup of the source. The operation succeeds. |
| `complete`, `rolled-back`, `failed` | Report the recorded outcome and remove the journal. |

A step that crashed the daemon is rolled back, not repeated, so a step that
cannot succeed does not loop. While both slots hold an applied config, the
journal decides which one describes the running node.

## Reprovision

`reprovision` drains the node, resets the host, and bootstraps again in the new
mode with the applied config as the configuration and `Isolation` set to the
target. It is the only strategy for any move involving `none`, and the Node name,
labels, and taints are kept. The reset is the existing agent reset, in its
existing order.

The reset removes the daemon and the agent binary, so the daemon cannot finish
the job. Instead:

1. The daemon checks the target's host requirements. For a `none` target only
   writable paths can be checked at this point, because `none` refuses a host
   that runs a kubelet, and this one does until it is reset; the full `none`
   check runs in the bootstrap after the reset.
2. It stages `/var/lib/unbounded-agent-reprovision` with a copy of the agent
   binary, the target configuration, and a journal, and asks the copy to
   validate the configuration and the bootstrap plan for the target mode. A mode
   with no plan fails here, before anything changes.
3. It cordons and drains the node, records the journal step `reprovisioning`, and
   starts the unit `unbounded-agent-reprovision.service`, then returns. The
   operation stays `InProgress`.
4. The unit runs the hidden `isolation-reprovision` command from the copy. It
   waits for the daemon to release installation ownership, runs the agent reset,
   then runs the bootstrap for the target mode. It is retried by systemd every
   30 seconds, up to ten times an hour, and is enabled so a reboot continues it.
   A reset that finished is not repeated.
5. The daemon that bootstrap installs finds the journal when it starts. Once the
   applied config records the target mode and the Node is Ready (reported after
   the reprovision started), it uncordons the node, marks the operation
   `Complete`, and removes the unit and the staging directory.

A failure before step 3 undoes the staging and uncordons the node. If the
bootstrap keeps failing, the host is left reset, the unit is failed, and the
operation stays `InProgress`, because no daemon is left to report it. The
Machine conditions set by the bootstrap and the unit's journal
(`journalctl -u unbounded-agent-reprovision`) say why. `unbounded-agent reset`
removes a host that cannot be recovered, but it does not remove the unit or the
staging directory; remove them by hand.

## State

- Pod data and the image store are not carried over. Images are pulled again.
- Host-global state is untouched: nftables, the LocalDNS interface and rules,
  WireGuard keys, sysctls, and bpffs mounts.
- Existing repave behavior is not changed to roll back.
- Going from `container` to `nspawn` in place needs `systemd-container` on the
  host. The migration checks for it and fails before changing anything, but does
  not install it; a reprovision does.

## Verify

```bash
kubectl get machineoperation migrate-worker-01 -o yaml
kubectl get node worker-01
sudo unbounded-agent exec -- crictl ps
```

On success the operation is `Complete` with the message
`IsolationMigrate to <target> completed (<strategy>)`.

## See also

- [Isolation Modes]({{< relref "reference/agent/isolation" >}})
- [Node and host operations]({{< relref "guides/operations" >}})
