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
provisional because that mode is. The agent does not cordon or drain the node;
see [Before you start](#before-you-start).
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

## Before you start

The agent does not touch Kubernetes Node objects during a migration. You are
responsible for cordoning and draining the node before you create the
operation, and for uncordoning it afterward. For example:

```shell
kubectl cordon worker-01
kubectl drain worker-01 --ignore-daemonsets --delete-emptydir-data
kubectl unbounded machine operation create migrate-worker-01 \
  --kind IsolationMigrate --machine worker-01 --param targetMode=container --wait
kubectl uncordon worker-01
```

Because the daemon does not call the Kubernetes API for a drain, no cluster
permission beyond what it already has is needed.

## In-place steps

`nspawn` and `container` can run side by side in different slots, so the agent
prepares the target while the source still runs. The target is built in the
alternate slot (`kube1` and `kube2`), from the applied config with only
`Isolation` changed.

1. Preflight the target backend. Failure here changes nothing.
2. Download the target's artifacts. Failure here changes nothing.
3. Prepare the target in the alternate slot. Leftovers of an earlier attempt in
   that slot are removed first.
4. Stop the source node stack.
5. Clean up host networking, as a repave does.
6. Start the target. Kubelet gets fresh credentials through the normal
   bootstrap flow: the bootstrap token in the applied config (the one a repave
   uses) is exchanged for a new kubelet client certificate. No kubelet
   credentials are copied, and a metalman-attested host is not attested again.
   The Node name, labels, and taints are kept.
7. Record the new mode and slot in the applied config.
8. Wait for the target to come up, for up to five minutes, with the check a
   repave makes (kubelet active on the host, with its credentials).
9. On success, destroy the source slot and remove its applied config. `status.observedIsolation` is updated from the new applied
    config.

### Rollback

If anything fails after the target exists, the agent stops the target, removes
its applied config and restores the source's, destroys the target, restarts the
source. The operation then reports the failure. If the source could not be
restored the error says so and the node needs attention. The source slot and its
applied config are kept until step 8 passes.

### Interruption

Before each step that changes the host, the daemon writes the step to
`/etc/unbounded/agent/isolation-migration.json` (atomically, next to the applied
configs). An operation that is already `InProgress` is not reconciled again, so a
daemon that restarts mid-migration resolves the journal itself when it starts,
before it takes any work, and then finishes the operation:

| Last step recorded | At the next daemon start |
|---|---|
| `preparing` (or `draining`, written by an earlier agent) | The source never stopped. The partial target is removed. The operation fails with "interrupted". |
| `stopping-source`, `starting-target`, `persisting` | Roll back as above: stop and remove the target, restore the source's applied config, restart the source. The operation fails with "interrupted". |
| `awaiting-ready` | Wait for the target again. On success finish the migration; on timeout roll back. |
| `ready` | Finish the cleanup of the source. The operation succeeds. |
| `complete`, `rolled-back`, `failed` | Report the recorded outcome and remove the journal. |

A step that crashed the daemon is rolled back, not repeated, so a step that
cannot succeed does not loop. While both slots hold an applied config, the
journal decides which one describes the running node.

## Reprovision

`reprovision` resets the host, and bootstraps again in the new
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
3. It records the journal step `reprovisioning` and starts the unit `unbounded-agent-reprovision.service`, then returns. The
   operation stays `InProgress`.
4. The unit runs the hidden `isolation-reprovision` command from the copy. It
   waits for the daemon to release installation ownership, runs the agent reset,
   then runs the bootstrap for the target mode. It is retried by systemd every
   30 seconds, up to ten times an hour, and is enabled so a reboot continues it.
   A reset that finished is not repeated.
5. The daemon that bootstrap installs finds the journal when it starts. Once the
   applied config records the target mode, it marks the operation
   `Complete`, and removes the unit and the staging directory.

A failure before step 3 undoes the staging. If the
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
