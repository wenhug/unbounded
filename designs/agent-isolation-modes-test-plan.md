# Agent isolation modes: test plan

Companion to [agent-isolation-modes.md](agent-isolation-modes.md). This plan
exercises the agent in each isolation mode on a real QEMU VM joined to a Kind
control plane, and produces a report of every scenario and its outcome. Unit
tests, golden tests and the backend conformance suite already cover the code
against fakes; nothing in them runs a real systemd, containerd, kubelet or
runc. This plan does.

## 1. What is tested

| Question | Scenarios |
|---|---|
| Does a host bootstrapped with no isolation setting behave exactly as before (nspawn)? | D01, D02 |
| Does every agent and host operation work in each mode? | D02, N01, X01, C01 |
| Can a node move between modes, and back, keeping its identity? | T01, T02 |
| Does Machine status report the applied mode, and does drift appear and clear? | T03 |
| Does a failed migration put the node and its shared host configuration back? | T04 |
| Does the uninstall script remove the node stack of each mode? | U01, U02, U03 |
| Does container mode work on its intended platform? | A01 |
| Do GPUs work in none and container mode? | G01 |

## 2. Environment

- One Kind cluster (control plane and machina controller, reached from the VM
  over a bridge) and one QEMU VM per scenario, booted from `HOST_BASE_OS`
  (default `ubuntu2404`). The harness is `hack/agent/e2e-kind/`, the same one
  CI uses for the lifecycle suite.
- Host needs: `docker`, `kind` v0.29 or newer, `kubectl`, `go`,
  `qemu-system-x86_64`, `qemu-img`, `genisoimage`, `iptables`, passwordless
  `sudo`, and read/write access to `/dev/kvm`.
- Each scenario starts from a fresh VM disk and a clean Machine and Node, so a
  failure cannot poison the next scenario.

## 3. Scenarios

Each scenario is one e2e suite, run with the stated `ISOLATION_MODE`.
"Unset" means no `--isolation` flag is passed, which must give nspawn.

| ID | Scenario | Start mode | e2e suite | Requires |
|---|---|---|---|---|
| D01 | Default flow with no isolation set: existing lifecycle | unset | `lifecycle` | - |
| D02 | Default (no flag): all operations | unset | `isolation-operations` | - |
| N01 | Explicit nspawn: all operations | nspawn | `isolation-operations` | - |
| X01 | none: all operations | none | `isolation-operations` | - |
| C01 | container: all operations | container | `isolation-operations` | - |
| U01 | Default (no flag): uninstall script | unset | `isolation-uninstall` | - |
| U02 | none: uninstall script | none | `isolation-uninstall` | - |
| U03 | container: uninstall script | container | `isolation-uninstall` | - |
| T01 | Round trip: nspawn to none and back | unset | `isolation-roundtrip` | - |
| T02 | Every ordered pair of modes | unset | `isolation-matrix` | - |
| T03 | Machine status and drift across migrations | unset | `isolation-drift` | - |
| T04 | Failed migration: rollback and config restore | container | `isolation-rollback` | - |
| A01 | container on Azure Container Linux | container | `isolation-operations` | `HOST_BASE_OS=acl` |
| G01 | GPU workloads in none and container mode | none | none | GPU hardware |

`python3 hack/agent/e2e-kind/isolation_plan.py plan` prints this table from the
machine-readable catalog, which is the source the runner uses.

### 3.1 Operations suite (D02, N01, X01, C01)

In order: bootstrap and join, mode check, host root layout, controller health,
node config, persisted agent config, kube-proxy, Machine self-registration,
Machine status, preflight, `exec` and `logs`, workload and DNS, NodeReboot,
host-driven AgentUpgrade, operation AgentUpgrade, AgentUpgrade rollback,
workload, repave by MachineConfiguration version, workload, host reboot,
mode and status check, AgentReset, delete Machine, reinstall on the same
disk, join, mode check, `exec`, workload.

Mode-specific expectations the steps assert:

| Operation | nspawn | none | container |
|---|---|---|---|
| Node stack | `systemd-nspawn@kubeN` machine | `unbounded-containerd.service`, `unbounded-kubelet.service`, `/opt/unbounded/node/<version>` | one `unbounded-node-<slot>.service`, root `/var/lib/unbounded/containers/<slot>` |
| exec / logs | `unbounded-agent exec` into the machine | `exec` on the host | `exec` into the container |
| NodeReboot | node stack restarts | node stack restarts, host boot ID unchanged | node stack restarts |
| Repave | other slot becomes active, bpffs is fresh (D01) | `current` link flips to a new version | other slot becomes active |
| AgentReset | no machine left | no units, no `/opt/unbounded/node` | no container root |
| Preflight | `nspawn-runtime` check runs | `none-backend` runs | `container-backend` runs |

### 3.2 Transitions (T01 to T04)

Any pair that involves none is a reprovision; nspawn and container may move in
place. After every migration the suites assert: operation `Complete` with the
daemon's message, same Node name, labels and taints, new mode recorded in the
applied config and in `status.observedIsolation`, a workload and DNS run, and
`exec` works.

- T01: nspawn to none to nspawn, with a soft reboot and a host reboot at each stop.
- T02: nspawn to container (in place), container to none, none to container,
  container to nspawn (in place), nspawn to none, none to nspawn, nspawn to
  container and back by reprovision. All six ordered pairs.
- T03: `IsolationDrift` is True while `spec.isolation` differs from the applied
  mode and False after it is set to the applied mode.
- T04: starts in container mode, the only mode that keeps its configuration at
  the shared host paths. An unsupported in-place pair (container to none) is
  rejected and the node is untouched. An in-place container to nspawn
  migration is failed after the source stopped; the source runs again, the
  recorded mode is unchanged and a digest of the shared host configuration
  (containerd, kubelet, CNI, PKI) is identical before and after.

## 4. Evidence and report

For every scenario the runner records start and end time, per-step result and
duration, the mode in effect at each step, the full suite log, and after the
suite a JSON snapshot of: the Machine's `spec.isolation`,
`status.observedIsolation` and `IsolationDrift` condition, the applied-config
mode of each slot, the node-stack units, the `none` current version, `machinectl`
output, daemon state and version, host boot ID and the Node line. A failed
scenario also keeps the harness diagnostics. Bootstrap tokens are redacted from
logs and the report.

The report (`report.md`, `report.html`, `results.json` under
`tmp/isolation-report/<run-id>/`) has: environment, a summary table, an
operation by mode coverage matrix, a mode transition matrix, and per-scenario
detail with the failing step and the end of its log.

Result meanings: PASS (every step passed), FAIL (a step failed), ERROR (the
harness could not run it: cluster or VM would not start), SKIP (not applicable
here, with the reason).

## 5. Applicability and limits

- container mode is provisional. On `ubuntu2404` C01, T02 and T04 exercise the
  runc runtime, the OCI bundle and the unit, not the Azure Container Linux
  assumptions (SELinux label, PID 1 shutdown, mount propagation). A01 is the
  scenario that checks those, and needs an ACL image.
- G01 needs GPU passthrough. It is listed so the report shows the gap.
- Rollback of a failed reprovision has no harness fault and is covered by unit
  tests only.
- T03 sets `spec.isolation` on the Machine; it is exploratory for side effects
  on generated agent config.
- D01 contains nspawn-only checks (host and nspawn distro pair, bpffs isolation
  across repave) that other modes skip by design.
- No scenario here replaces `make test`; run that first.

## 6. Running it

```sh
# once: groups take effect on a new login, or wrap the command in `sg kvm -c`
PATH="$PWD/tmp/bin:$PATH" python3 hack/agent/e2e-kind/isolation_plan.py run
python3 hack/agent/e2e-kind/isolation_plan.py run --scenario D01 T01   # a selection
python3 hack/agent/e2e-kind/isolation_plan.py run --category transitions --fail-fast
python3 hack/agent/e2e-kind/isolation_plan.py run --exclude-tag provisional
```

`--no-up` reuses a cluster and bridge you created; `--keep-env` leaves the
cluster, bridge and VM for inspection. `render <results.json> --html`
regenerates the report from saved results.
