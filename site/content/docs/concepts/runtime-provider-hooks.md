---
title: "Runtime Provider Hooks"
weight: 9
---

Runtime provider hooks apply workload-selected network post-configuration
after DRANET attaches and configures devices and before workload processes
run. A profile provider chooses a binary installed on the node. DRANET adds
it to the container specification as an OCI `createRuntime` hook.

The hook only changes network state in the Pod's namespace. DRANET retains
device attachment and teardown. The hook must not move, rename or re-address
DRANET's interfaces. The workload selects a profile, while the provider
defines and validates its policy.

## Lifecycle

1. During `NodePrepareResources`, DRANET resolves the profile through
   `GetProfileConfig`, then calls `GetRuntimeHook` with the device, full
   ResourceClaim and resolved network configuration. A valid hook is stored
   with the device in the node checkpoint.
2. Kubelet calls CRI `RunPodSandbox`. The runtime establishes the Pod's
   network namespace and primary networking. DRANET's NRI `RunPodSandbox`
   callback attaches and configures the claimed devices.
3. NRI `CreateContainer` adds pending provider hooks to the container's OCI
   specification. It does not execute the binaries or call the webhook.
4. During CRI `StartContainer`, containerd creates the container task. With
   runc, this invokes OCI create and executes the `createRuntime` hooks in
   the runtime's namespaces before starting the workload process.
5. After successful OCI creation, NRI `StartContainer` records completion.
   Later startup attempts in the same sandbox have no provider hooks. A new
   sandbox clears completion and runs the hooks again.
6. During `NodeUnprepareResources`, DRANET calls `ReleaseProfileConfig` for
   each profiled device and removes the claim's stored configuration.

Hook execution is independent of the NRI callback timeout. Containerd's
`plugin_request_timeout` defaults to two seconds, which makes synchronous
binary execution inside an NRI callback unsuitable for slower
post-configuration. OCI hooks have their own runtime-enforced timeout,
while the full container startup is still bounded by kubelet's
`--runtime-request-timeout`, normally two minutes.

The provider hook defaults to 10 seconds and may request up to 30 seconds.
The total hook timeout for a container may not exceed 90 seconds. An
operator can configure a shorter outer startup timeout, so these limits
do not guarantee that every permitted chain fits the node's configuration.
Native attachment behavior and its NRI timeout handling are unchanged.

## Startup Failures

An HTTP error or invalid hook response fails `NodePrepareResources`.
Oversized hook environments or excessive chain timeouts fail NRI
`CreateContainer`. A missing executable, permission error, non-zero exit or
hook timeout fails OCI creation during container startup, before the
container process runs. Kubelet retries startup; the OCI runtime does not
retry the binary itself.

| Pod layout | Failure behavior |
|---|---|
| Single application container | Its process does not run. Its next startup attempt carries the pending hooks. |
| Multiple application containers | After one fails, kubelet can attempt another in the same startup pass. It also carries the pending hooks. Permanent failure blocks all container processes. Another container can complete post-configuration before the failed container retries. |
| Regular init container | A failed hook prevents the init process from running and blocks application startup. Kubelet retries the init container. |
| Restart in a configured sandbox | Hooks are omitted once completion is recorded. A new sandbox runs them again. |

The hook runs before the first successful OCI creation, which may belong to
a different application container than the first attempted one. It cannot
depend on container identity. Completion is recorded before the workload
process starts, so a subsequent process-start failure does not repeat
already-completed post-configuration.

Hook failure does not roll back native attachment or earlier hook changes.
The claim's native network status can already be ready while the Pod is
unready. Partial post-configuration remains for the next attempt. Hooks
must be idempotent and tolerate repeated or concurrent invocation; the
checkpoint does not provide exactly-once execution.

## Cleanup

Provider cleanup uses the existing profile lifecycle. DRANET invokes
`ReleaseProfileConfig` during `NodeUnprepareResources` whether the runtime
hook completed, failed or never ran. A failed startup leaves the profile
prepared for kubelet's next attempt and does not immediately trigger
release. Preparation errors after profile allocation also use
`ReleaseProfileConfig` to release allocated state.

The release request contains device identifiers, the ResourceClaim UID and
the stored network configuration. It does not contain runtime-hook `data`
or a usable Pod network namespace. Providers must retain any additional
cleanup information under a stable `(claimUID, device)` identity and make
release safe when setup was partial or no resources were allocated.

DRANET logs release failures and continues unprepare without retrying the
failed release. Providers are responsible for reclaiming orphaned
resources after an unsuccessful cleanup or node loss. Namespace-local
settings disappear when the Pod's network namespace is destroyed.

There is no automatic OCI cleanup hook or CNI `DEL` invocation. A provider
reusing a CNI delegate must handle any required release through its profile
provider and account for the namespace being unavailable at unprepare. CNI
normally pairs `ADD` with `DEL`, including after failed `ADD`, and discourages
repeated `ADD` for the same identity without `DEL`. This extension therefore
supports selected idempotent post-configuration delegates, rather than
arbitrary CNI attachment or complete Multus behavior.

## Hook Environment

All provider hooks receive the same Pod-scoped environment:

| Variable | Content |
|---|---|
| `DRANET_POD_UID` | Pod UID. |
| `DRANET_POD_NAMESPACE`, `DRANET_POD_NAME` | Pod namespace and name. |
| `DRANET_NETNS` | Path of the Pod's network namespace. |
| `DRANET_CLAIMS` | JSON array containing the prepared ResourceClaims and devices. |

Each claim contains `claim`, the ResourceClaim snapshot without
`managedFields`, and a sorted `devices` array. Each device contains its
ResourceSlice name, host identifiers (`device`), DRANET's final network
configuration (`config`) and provider-defined opaque `data`.

```json
[{"claim":{"metadata":{"namespace":"default","name":"network","uid":"claim-uid"}},
  "devices":[{"name":"net0","device":{"name":"net0"},
              "config":{"profile":"example.com/tuning","interface":{"name":"net0"}},
              "data":{"delegate":"tuning"}}]}]
```

The kernel limits an individual environment string to 128 KiB. DRANET
checks the claim environment at container creation. Provider data is
limited to 4 KiB per device at preparation.

The runtime passes OCI container state on stdin. Hooks must drain it and
use `DRANET_NETNS` for Pod networking rather than deriving their behavior
from container identity. OCI hooks run with runtime privileges; the
post-configuration scope is a provider contract, not a security sandbox.

Devices sharing a binary path and arguments share one invocation with the
largest requested timeout. Distinct invocations are sorted by path and
arguments. Every hook receives all devices, so providers should identify
their devices through a marker in opaque data. A provider needing dependent
operations should return one wrapper that owns their ordering.

## Troubleshooting

```sh
kubectl describe pod <pod-name>
kubectl get pod <pod-name> -o json
```

Inspect `status.containerStatuses`, `status.initContainerStatuses` and
events for `createRuntime` hook errors. A non-zero exit can include the
binary's stderr. A timeout can report only the runtime's timeout message.
The hook runs on the node and has no application-container logs.

Verify the hook and its dependencies are executable on every eligible
node, inspect provider and runtime logs, and check the startup timeouts.
The NRI validator must permit OCI hook adjustments. Configure required NRI
plugins when container creation must fail if DRANET is disconnected.

After correcting the cause, kubelet retries automatically. Delete and
recreate the Pod if partial changes require a fresh namespace. Deletion
also lets kubelet reach resource unprepare and provider release; stopping
or restarting an individual container does not unprepare its claims.

The counters `dranet_driver_container_hooks_total{type="provider"}` and
`dranet_driver_runtime_hooks_completed_total` distinguish hook injections
from completed Pod post-configuration. Repeated injections without
completion can indicate startup failures.