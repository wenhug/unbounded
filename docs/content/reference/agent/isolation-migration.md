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
**Availability.** The agent validates the request, picks the strategy, and rejects
unsupported moves before touching the node. The host operations that carry out a
migration are not yet wired, so in an agent build without them a supported
request fails with `isolation migration from <source> to <target> (<strategy>) is
not yet wired to host operations` and leaves the node untouched. The in-place
procedure below is implemented and tested against fake backends. Check your
build's release notes before relying on it.
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

## In-place steps

`nspawn` and `container` can run side by side in different slots, so the agent
prepares the target while the source still runs.

1. Preflight the target backend. Failure here changes nothing.
2. Download the target's artifacts. Failure here changes nothing.
3. Prepare the target in the alternate slot.
4. Drain the node.
5. Stop the source node stack.
6. Clean up host networking, as a repave does.
7. Start the target. Kubelet gets fresh credentials through the normal bootstrap
   and attestation flow. No credentials are copied. The Node name, labels, and
   taints are kept.
8. Record the new mode, slot, and owning backend in the applied config.
9. Wait for kubelet Ready within a bounded timeout.
10. On success, destroy the source slot and remove its applied config.

### Rollback

If anything fails after the target exists, the agent stops the target, restores
the source's applied config if it was already replaced, destroys the target, and
starts the source again. The operation then reports the failure, and if the
source could not be restored the error says so and the node needs attention. The
source slot is kept until step 9 passes.

## Reprovision

`reprovision` drains the node, resets the host, and bootstraps again in the new
mode. It is the only strategy for any move involving `none`.

## State

- Pod data and the image store are not carried over. Images are pulled again.
- Host-global state is untouched: nftables, the LocalDNS interface and rules,
  WireGuard keys, sysctls, and bpffs mounts.
- Existing repave behavior is not changed to roll back.

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
