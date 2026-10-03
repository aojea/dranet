# Developer documentation


## Develop locally

Use [KIND](https://kind.sigs.k8s.io/)

1. Create kind cluster with the recommended config

```
make kind-cluster
```

2. Do your changess to the codebase and rollout the custom version to the kind
   cluster

```
make kind-install
```

3. Test your changes locally, use the [examples folder](./examples) for dropping manifests
and README of the scenarios you are testing.

4. Once finish the development add an e2e test in bash using the `bats`
   framework in [the tests folder](./tests)

You can run your tests locally using `bats tests/`


## Develop in a cluster


1. Build and push the image to a registry

```
docker build . --tag aojea/dranet:test --push
```

2. Install dranet

```
kubect apply -f ./install.yaml
```

3. When developing new features update the image of the `dranet` daemonset and
   rollout

```sh
$ kubectl set image ds/dranet -n kube-system dranet=aojea/dranet:test
daemonset.apps/dranet image updated
$ kubectl rollout status ds/dranet -n kube-system
Waiting for daemon set "dranet" rollout to finish: 1 out of 5 new pods have been
updated...
```

Set the ImagePullPolicy to Alwasy to always get the latest changes

```
kubectl patch daemonset dranet -n kube-system --type='strategic' -p='
{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "dranet",
            "imagePullPolicy": "Always"
          }
        ]
      }
    }
  }
}'
```

If you build new images just restart the ds to pull it

```
kubectl -n kube-system rollout restart ds dranet
daemonset.apps/dranet restarted
```

## Runtime provider post-configuration

The optional `runtimeHook` webhook capability extends the profile provider.
`GetRuntimeHook` is called during `NodePrepareResources`, after profile
resolution. It returns a host binary, arguments, a bounded timeout and
opaque per-device data. The container runtime executes it as an OCI
`createRuntime` hook after DRANET's sandbox setup and native network
configuration. There is no webhook request during hook execution.

The hook only performs post-configuration in `DRANET_NETNS`. It must not
move, rename or re-address DRANET's interfaces, and it must drain the OCI
state on stdin without using container identity. DRANET retains attachment
and teardown. The provider installs the binary and dependencies on every
node. The hook runs with runtime privileges; this scope is not enforced by
a sandbox.

### Failure contract

| Failure | Stage | Result |
|---|---|---|
| HTTP error or invalid hook response | `NodePrepareResources` | Preparation fails and kubelet retries it. |
| Hook chain or environment exceeds its limit | NRI `CreateContainer` | Container creation fails before the hook executes. |
| Missing binary, permission error, non-zero exit or runtime timeout | OCI create during container startup | The container process does not run. Native attachment stays complete and provider hooks remain pending. |

`RuntimeHookDone` is recorded for the Pod's devices only when NRI
`StartContainer` follows successful OCI creation. Later containers and
restarts in that sandbox have no provider hooks. A new sandbox clears
completion. Execution is retryable, not exactly once: hooks may run again
after partial failure or loss of the completion checkpoint.

For one application container, a failed hook is retried on its next startup
attempt. For multiple application containers, kubelet can try the next
container after a failure in the same startup pass. That container also
receives the pending hooks and may complete post-configuration. The first
failed container then retries without them. A persistent error prevents
all container processes from running. A failed regular init container
blocks application startup until an init attempt completes the hooks.

Distinct hooks run in stable path/argument order after native configuration. Shared
binary/argument pairs run once per attempt with the largest device timeout.
Earlier hooks and partial changes remain after failure. Hooks must be
idempotent and must not depend on which container carries them. DRANET
does not roll back a failed runtime attempt or invoke an OCI cleanup hook.
Provider cleanup uses `ReleaseProfileConfig` during `NodeUnprepareResources`,
for each profiled device whether its runtime hook completed, failed or never
ran. A startup failure does not trigger release because the prepared profile
is still needed for kubelet's retry. Preparation failures after allocation
also release the profile through the existing error path.

The release request contains device identifiers, claim UID and the stored
network configuration. It does not include hook data or a live Pod network
namespace. Providers must store additional cleanup state under a stable
claim/device identity and handle partial setup idempotently. Release errors
are logged, unprepare continues, and DRANET does not retry the failed release;
providers must reclaim orphaned resources. A provider wrapping CNI must
account for `ADD`/`DEL` ordering and implement required release in its profile
provider. A startup hook alone does not provide the full CNI lifecycle.
Namespace-local post-configuration is illustrated in
`examples/webhook-runtime` with the CNI `tuning` binary.

### Focused validation

```sh
go test ./pkg/apis ./pkg/cloudprovider/webhook ./pkg/driver -run 'TestRuntimeHookValidate|TestWebhookGetRuntimeHook|TestCreateContainerRuntimeHooks|TestUnprepareRuntimeHookProfile' -count=1
go test ./examples/webhook-runtime -count=1
bats tests/python_webhook.bats
```

The Bats suite builds the test image and creates its own kind cluster. It
checks successful post-configuration, non-zero exits, runtime timeouts and
recovery for single- and multi-container Pods, plus failure and timeout in
a regular init container. It verifies that no process starts during a
persistent hook failure and that a successful attempt suppresses hooks on
subsequent startup attempts. The executable unit tests validate the driver
completion state, unprepare cleanup for completed and pending hooks, and CNI
wrapper error propagation without a cluster.

User-visible failure and troubleshooting details are documented in
`site/content/docs/concepts/runtime-provider-hooks.md`. Do not describe
execution as exclusive to the first declared
container or assume that another application container cannot run after
the first container's hook fails.

## Checkpoint database: upgrades and rollbacks

DRANET stores device state (`DeviceConfig`) in a node-local bbolt database
(`--db-path`, default `/var/run/dranet/dranet.db`) so state like DHCP leases
survives daemon restarts. The database uses bbolt's file lock and is local to
each node.

The database layout version is stored under the `meta/schemaVersion` key
(`checkpointSchemaVersion` in `pkg/driver/pod_device_config_bolt.go`). Databases
without a `meta` bucket were created before versioning and are treated as version 1.

Migrations run sequentially (for example, v1 -> v2 -> v3 -> v4). All migrations
and the version update run in a single transaction. If any migration step fails
(for example, 3 -> 4), the entire transaction rolls back to the starting
version (v1). This prevents leaving the database in an intermediate version that
an older version of DRANET cannot read.

### Changing the checkpoint format

`TestDeviceConfigWireFormatGolden` validates the serialized JSON data. When making changes:

1. **Additive changes**: New optional fields (`omitempty`) do not require a
   version bump. Older versions ignore unknown fields. Update `fullDeviceConfig()`
   and update the golden test data if needed.
2. **Breaking changes**: Bump `checkpointSchemaVersion` and add an entry in
   `checkpointMigrations` for breaking changes (renaming fields, changing types,
   or modifying bucket layout).

### Upgrade and rollback behavior

| Scenario | Behavior |
|---|---|
| Upgrade with additive changes | Existing entries load with new fields unset. |
| Upgrade with schema bump | Migrations run when opening the database. If a migration fails, the entire transaction rolls back. |
| Rollback within same schema | The daemon reads existing data and ignores unknown fields. Unmodified entries keep the unknown fields on disk. |
| Rollback across schema bump | The daemon fails to start because it cannot read a newer schema version. Recover by rolling forward to the newer version, or delete the database file if discarding checkpoint state is acceptable. |
| Overlapping pods during rolling update | bbolt allows only one process to hold the database file lock. The new pod waits up to 1 second for the lock, then fails and restarts until the old pod exits. |

Every scenario is covered by tests in `pkg/driver/pod_device_config_bolt_version_test.go`.

## Troubleshooting

```
kubectl -n kube-system get pods -l app=dranet -o wide
NAME           READY   STATUS             RESTARTS         AGE   IP              NODE                                            NOMINATED NODE   READINESS GATES
dranet-9z66b   0/1     CrashLoopBackOff   12 (4m54s ago)   42m   10.146.104.1
```

Git commit is in the first line of logging

```
kubectl -n kube-system logs dranet-9z66b
Defaulted container "dranet" out of: dranet, enable-nri (init)
I0520 09:21:02.486329 1027992 app.go:181] dranet go go1.24.3 build: 3058756228b78265819e96963afae4dfd9497849 time: 2025-05-19T22:57:49Z
I0520 09:21:02.486404 1027992 app.go:75] FLAG: --add_dir_header="false"
I0520 09:21:02.486409 1027992 app.go:75] FLAG: --alsologtostderr="false"
I0520 09:21:02.486411 1027992 app.go:75] FLAG: --bind-address=":9177"
I0520 09:21:02.486413 1027992 app.go:75] FLAG: --filter="attributes[\"dra.net/type\"].StringValue  != \"veth\""
I0520 09:21:02.486415 1027992 app.go:75] FLAG: --hostname-override=""
I0520 09:21:02.486417 1027992 app.go:75] FLAG: --kubeconfig=""
I0520 09:21:02.486418 1027992 app.go:75] FLAG: --log_backtrace_at=":0"
I0520 09:21:02.486423 1027992 app.go:75] FLAG: --log_dir=""
I0520 09:21:02.486424 1027992 app.go:75] FLAG: --log_file=""
I0520 09:21:02.486425 1027992 app.go:75] FLAG: --log_file_max_size="1800"
I0520 09:21:02.486427 1027992 app.go:75] FLAG: --logtostderr="true"
I0520 09:21:02.486429 1027992 app.go:75] FLAG: --one_output="false"
I0520 09:21:02.486430 1027992 app.go:75] FLAG: --skip_headers="false"
I0520 09:21:02.486435 1027992 app.go:75] FLAG: --skip_log_headers="false"
I0520 09:21:02.486436 1027992 app.go:75] FLAG: --stderrthreshold="2"
I0520 09:21:02.486440 1027992 app.go:75] FLAG: --v="4"
I0520 09:21:02.486442 1027992 app.go:75] FLAG: --vmodule=""
I0520 09:21:02.486599 1027992 envvar.go:172] "Feature gate default state" feature="ClientsAllowCBOR" enabled=false
I0520 09:21:02.486609 1027992 envvar.go:172] "Feature gate default state" feature="ClientsPreferCBOR" enabled=false
I0520 09:21:02.486611 1027992 envvar.go:172] "Feature gate default state" feature="InformerResourceVersion" enabled=false
I0520 09:21:02.486614 1027992 envvar.go:172] "Feature gate default state" feature="InOrderInformers" enabled=true
I0520 09:21:02.486616 1027992 envvar.go:172] "Feature gate default state" feature="WatchListClient" enabled=false
I0520 09:21:02.491702 1027992 draplugin.go:486] "Starting"
I0520 09:21:02.491855 1027992 nonblockinggrpcserver.go:88] "GRPC server started" logger="dra"
I0520 09:21:02.491919 1027992 nonblockinggrpcserver.go:88] "GRPC server started" logger="registrar"
time="2025-05-20T09:21:04Z" level=info msg="Created plugin 00-dra.net (dranet, handles RunPodSandbox,StopPodSandbox,RemovePodSandbox)"
I0520 09:21:04.492764 1027992 app.go:157] driver started
I0520 09:21:04.492786 1027992 driver.go:430] Publishing resources
time="2025-05-20T09:21:04Z" level=info msg="Registering plugin 00-dra.net..."
I0520 09:21:04.493135 1027992 cloud.go:38] running on GCE
time="2025-05-20T09:21:04Z" level=info msg="Configuring plugin 00-dra.net for runtime containerd/1.7.24..."
```
