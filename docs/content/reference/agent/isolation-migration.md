---
title: "Migrating Between Modes"
weight: 7
description: "Move a live node to another isolation mode with an IsolationMigrate MachineOperation."
---

Changing `spec.isolation` on a running Machine does not move the node. It marks
the Machine as drifted from the mode recorded on the host. The node moves only
when an `IsolationMigrate` [MachineOperation]({{< relref "guides/operations" >}})
runs. When a migration succeeds, the agent sets `spec.isolation` to the new
mode, so the drift condition clears. This is the only change the agent makes to
the spec of an existing Machine. It needs the `patch` verb on `machines`, which
the `unbounded-daemon-controller-machine` ClusterRole grants. If the patch
fails, the migration still succeeds and the agent logs a warning; set
`spec.isolation` to match to clear the drift.

{{< callout type="important" >}}
Migration is experimental and has not yet been validated on real hosts. Any move
that involves `container` is experimental because that mode is. The agent
validates the request, picks the strategy, and rejects unsupported moves before
touching the node. It does not cordon or drain the node; see
[Before you start](#before-you-start).
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
3. Save the host configuration files that both stacks write at the same paths:
   the kubelet configuration, kubeconfig and client credentials, the containerd
   configuration, the CNI configuration and the LocalDNS files. They are copied
   byte for byte, with their modes and owners, to
   `/etc/unbounded/agent/isolation-migration-config`, next to the journal. Failure here
   changes nothing.
4. Prepare the target in the alternate slot. Leftovers of an earlier attempt in
   that slot are removed first.
5. Stop the source node stack.
6. Clean up host networking, as a repave does.
7. Start the target. Kubelet gets fresh credentials through the normal
   bootstrap flow: the bootstrap token in the applied config (the one a repave
   uses) is exchanged for a new kubelet client certificate. No kubelet
   credentials are copied. The bootstrap token is not renewed, so an in-place
   migration of a node whose token has expired is not different from a repave of
   it.
   The Node name, labels, and taints are kept.
8. Record the new mode and slot in the applied config.
9. Wait for the target to come up, for up to five minutes, with the check a
   repave makes (kubelet active on the host, with its credentials).
10. On success, destroy the source slot, remove its applied config and the saved
    configuration. `status.observedIsolation` is updated from the new applied
    config.

### Rollback

If anything fails after the target exists, the agent stops the target, removes
its applied config and restores the source's, destroys the target, puts the
saved host configuration back (so files the target rewrote or removed, such as
the kubelet credentials, are the source's again, and files it added are gone),
and restarts the source. The operation then reports the failure. If the source
could not be restored the error says so and the node needs attention. If the
configuration could not be put back the error says so and the saved copy is kept
in `/etc/unbounded/agent/isolation-migration-config`. The source slot and its
applied config are kept until step 9 passes.

### Interruption

Before each step that changes the host, the daemon writes the step to
`/etc/unbounded/agent/isolation-migration.json` (atomically, next to the applied
configs). An operation that is already `InProgress` is not reconciled again, so a
daemon that restarts mid-migration resolves the journal itself when it starts,
before it takes any work, and then finishes the operation:

| Last step recorded | At the next daemon start |
|---|---|
| `preparing` | The source never stopped. The partial target is removed and the saved host configuration is put back. The operation fails with "interrupted". |
| `stopping-source`, `starting-target`, `persisting` | Roll back as above: stop and remove the target, restore the source's applied config and the saved host configuration, restart the source. The operation fails with "interrupted". |
| `awaiting-ready` | Wait for the target again. On success finish the migration; on timeout roll back. |
| `ready` | Finish the cleanup of the source and remove the saved configuration. The operation succeeds. |
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

1. The daemon checks the target's host requirements. For a `none` target it
   runs the static half of the `none` preflight: the cgroup filesystem,
   `systemctl`, and writable `/etc`, `/opt/unbounded/node` and `/var`. The other
   half refuses a host that runs a kubelet or containerd the agent does not
   own, and this one does until it is reset, so that half runs in the bootstrap
   after the reset, on the clean host.
2. It checks that the bootstrap after the reset will have a credential. If the
   host was bootstrapped with attestation (the agent recorded the attestation
   URL in `/etc/unbounded/agent/attest.json`), the staged configuration carries
   it and the bootstrap attests again for a new token. Otherwise the bootstrap
   token recorded in the applied config is used, and the daemon asks the API
   server whether it still accepts it. If the server answers `401 Unauthorized`
   (the token expired or was revoked), the operation fails here, before
   anything is reset, with a message to renew the token or bootstrap with
   attestation and try again. If the server cannot be asked, the migration goes
   ahead. A host that authenticates with an exec credential has no token to
   check. A host bootstrapped by an earlier agent has no recorded attestation
   URL and takes the token path.
3. It stages `/var/lib/unbounded-agent-reprovision` with a copy of the agent
   binary, the target configuration, and a journal, and asks the copy to
   validate the configuration and the bootstrap plan for the target mode. A mode
   with no plan fails here, before anything changes.
4. It records the journal step `reprovisioning` and starts the unit `unbounded-agent-reprovision.service`, then returns. The
   operation stays `InProgress`.
5. The unit runs the hidden `isolation-reprovision` command from the copy. It
   waits for the daemon to release installation ownership, runs the agent reset,
   then runs the bootstrap for the target mode. A run that fails is retried by
   systemd every 30 seconds, and the unit is enabled so a reboot continues it.
   A reset that finished is not repeated. The journal counts the runs.
6. The daemon that bootstrap installs finds the journal when it starts. Once the
   applied config records the target mode, it marks the operation
   `Complete`, and removes the unit and the staging directory.

A failure before step 4 undoes the staging.

### When the bootstrap never succeeds

After five runs have failed, the command stops retrying. It records the journal
step `failed` with the last error, removes the unit and the staged agent binary
and configuration (which holds the bootstrap token), and exits successfully so
systemd does not run it again. Only the journal stays in
`/var/lib/unbounded-agent-reprovision`.

The host is then reset and has no daemon, so nothing can update the
`MachineOperation` yet: it stays `InProgress` until an agent runs on the host
again. Bootstrap the host again, for example with the install script. The daemon
it installs reads the journal, marks the operation `Failed` with the recorded
message, and removes the directory. Until then the journal
(`/var/lib/unbounded-agent-reprovision/journal.json`) and the unit's log
(`journalctl -u unbounded-agent-reprovision`) say why.

`unbounded-agent reset` removes the reprovision unit, its directory and its
journal along with the rest of the agent, so a reset after a failed reprovision
leaves nothing behind. The reprovision's own reset keeps them.

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
