# Webhook Runtime Post-Configuration

This example runs the standard CNI `tuning` binary after DRANET attaches and
configures a claimed interface. It sets `rp_filter=0` on that interface in
the Pod's network namespace. DRANET retains interface attachment, names,
addresses and teardown.

The webhook returns a host wrapper through `GetRuntimeHook`. The wrapper
receives OCI state on stdin, drains it, and translates `DRANET_CLAIMS` and
`DRANET_NETNS` into CNI input. The Pod UID is the stable `CNI_CONTAINERID`.
Each device tagged `data.delegate=tuning` is configured independently. You
can extend the provider's opaque data and wrapper without changing DRANET.

## Node Setup

Use a cluster running DRANET with runtime-provider support and NRI hook adjustments
enabled. On every eligible node, install the CNI plugins distribution's
`tuning` executable at `/opt/cni/bin/tuning`, install `jq`, and install the
wrapper on an executable host filesystem:

```sh
install -D -m 0755 examples/webhook-runtime/cni-tuning.sh /opt/dranet/bin/cni-tuning.sh
go build -o /tmp/dranet-runtime-provider ./examples/webhook-runtime
/tmp/dranet-runtime-provider --bind-address=127.0.0.1:8082
```

Run the provider in a separate terminal or as a node service. When using a
DaemonSet, use host networking for the loopback endpoint and a hostPath
mount to install the wrapper. Provider endpoints should remain reachable
only by trusted node components.

Configure the DRANET DaemonSet with:

```text
--profile-provider=webhook
--webhook-url=http://127.0.0.1:8082
```

Keep the existing cloud provider. The example declares only profile and
runtime-hook capabilities. The profile returns no additional native
configuration and leaves the user and cloud configuration unchanged.

On one test node, create an unused dummy interface for the claim:

```sh
ip link add post0 type dummy
ip link set post0 up
```

Ensure DRANET's inventory filter includes the dummy interface. The claim's
selector matches `post0`; change it to a suitable unused physical interface
for hardware testing. Do not select the node's primary interface.

## Run

```sh
kubectl apply -f examples/webhook-runtime/pod.yaml
kubectl wait --for=condition=ready pod/runtime-hook --timeout=120s
kubectl exec runtime-hook -c app -- cat /proc/sys/net/ipv4/conf/post0/rp_filter
kubectl exec runtime-hook -c second -- cat /proc/sys/net/ipv4/conf/post0/rp_filter
```

Both containers should report `0`. The hook completes before the first
successful container startup; the containers share the configured network
namespace. Successful completion suppresses the hook on later containers
and container restarts in the same sandbox.

The wrapper supplies `prevResult` for the already-attached interface and
calls CNI `ADD`. It only requests namespace-local sysctls. These disappear
when the namespace is destroyed, and this configuration does not create
the `tuning` plugin's interface-attribute backup files. It needs no `DEL`
call. The example's `ReleaseProfileConfig` endpoint is consequently a no-op.
Do not add MAC, MTU, promiscuous-mode or other settings requiring restoration,
and do not use an attachment or IPAM delegate in this wrapper.

Provider-owned resource cleanup belongs to `ReleaseProfileConfig`, which
DRANET calls during `NodeUnprepareResources` after successful, failed or
unattempted runtime configuration. A startup failure keeps the profile
prepared for retries and does not call release immediately. Release receives
the claim UID, device identifiers and stored network configuration, without
hook data or a live network namespace. A stateful provider must retain any
extra cleanup information itself. Release errors are logged without a retry,
so the provider must reclaim orphaned resources. DRANET provides no automatic
OCI cleanup hook or CNI `DEL` call.

This is Multus-like delegation to a CNI binary for post-configuration.
NetworkAttachmentDefinitions, Multus network selection and CNI attachment
are outside this example.

## Failure Behavior

Malformed hook responses fail claim preparation. A missing wrapper or CNI
binary, a rejected sysctl, a non-zero exit or a runtime timeout fails
container startup before its process runs. Use `kubectl describe pod
runtime-hook` to inspect events. The wrapper forwards CNI errors to stderr;
timeouts may report only the runtime's timeout diagnostic.

DRANET's native attachment remains complete after hook failure. Partial
post-configuration is not rolled back. Repeated sysctl assignments are
idempotent, so kubelet can retry safely.

In a single-container Pod, the same container's next startup attempt retries
the hook. With multiple application containers, another container can
complete the pending hook after the first fails. The failed container's
next attempt then starts without repeating it. Permanent failure blocks
all container processes. A failing regular init container blocks
applications until an init attempt succeeds.

This contract does not guarantee exactly-once execution. Hooks must be
independent of container identity and tolerate retries and concurrent
invocations. A new sandbox applies post-configuration again.

## Tests

```sh
go test ./examples/webhook-runtime -count=1
shellcheck examples/webhook-runtime/cni-tuning.sh
bats tests/python_webhook.bats
```

The example tests exercise the provider and wrapper with a fake CNI binary,
including non-zero exit propagation. The Bats tests exercise actual OCI
hook failures, timeouts and recovery with single-container, multi-container
and init-container Pods using a deterministic test provider.

## Cleanup

```sh
kubectl delete -f examples/webhook-runtime/pod.yaml
```

Wait for the Pod to be deleted before removing `post0` from the host. Restore
the previous DRANET provider arguments before stopping the example provider.