---
title: "OCI OKE RDMA with DRANET"
date: 2026-09-08T00:00:00Z
---

DRANET gives pods IPvlan children of the RDMA NICs of an OKE bare metal GPU node, so the RDMA NICs stay on the host. On Oracle Kubernetes Engine (OKE), Oracle Cloud Agent (OCA) configures each Ethernet RDMA NIC (`rdma0`, `rdma1`, and so on; the number of RDMA NICs depends on the shape) with one IPv4 address from the RDMA network. OCA uses `10.224.0.0/12` as that network by default.

The examples for the OKE shapes live under [examples/oci_oke_examples](https://github.com/kubernetes-sigs/dranet/tree/main/examples/oci_oke_examples).

## What the OKE provider publishes

The provider reads the OCI Instance Metadata Service (IMDS) at startup and refreshes it every 5 minutes. Every device gets these node-level attributes when IMDS supplies them: `oke.dra.net/hpcIslandId`, `oke.dra.net/networkBlockId`, `oke.dra.net/localBlockId`, `oke.dra.net/rackId`, `oke.dra.net/gpuMemoryFabricId`, `oke.dra.net/shape`, `oke.dra.net/rdmaFabricIpv6`, and `oke.dra.net/rdmaFabricPlanes`.

On a node with an IPv4 RDMA fabric, every device also gets `oke.dra.net/rdmaChildIpv4Cidr`, the configured child range (see [The child range](#the-child-range)). This value comes from the cloud provider options, not from IMDS. The attribute describes the node, so a selector on it must also select the RDMA NICs.

## The oke-rdma profile

The provider advertises the `oke-rdma` profile for each Ethernet RDMA NIC. A claim for an RDMA NIC gets an IPvlan child of the RDMA NIC with its own address in the child range (`10.208.0.0/12` by default), a route table with a source rule, and the ARP settings of the RDMA NIC. The RDMA NIC itself stays on the host with its address and settings. One pod uses an RDMA NIC at a time. The child keeps the parent name inside the pod.

The child address has its own GID index on the RDMA NIC, different from the index of the parent address. Do not set `NCCL_IB_GID_INDEX` for a job that uses the children. NCCL selects the GID index itself.

The profile needs these conditions:

- Each RDMA NIC has the `rdmaN` name or an address in the OCA RDMA network. Some shapes, for example BM.Optimized3.36, keep the operating system name of the RDMA NIC. On such a shape the provider identifies the RDMA NIC by that address. A claim that is prepared before OCA assigns the address moves the RDMA NIC into the pod.
- The RDMA subsystem runs in shared network namespace mode (`netns_mode=1` for `ib_core`). In exclusive mode the claim fails with the message `use shared RDMA mode`.
- All nodes that share one RDMA network use one primary VNIC subnet.
- The child range is large enough for the primary VNIC subnet. See [The child range](#the-child-range).
- No VCN subnet, pod CIDR, or service CIDR uses the child range. For each claim, the provider checks the OCA RDMA network, every VNIC subnet of the node, every host route that overlaps the child range except the default route, and the Kubernetes service address. It cannot see the whole pod CIDR or service CIDR.

The profile rejects `interface.type: Passthrough`, DHCP, SLAAC and unnumbered addressing, and addresses in the claim. A claim with its own `routes`, `rules`, or a VRF owns the routing.

RDMA NICs on an IPv6 fabric get no profile and move into the pod as before.

The profile needs `--profile-provider=cloud`, the default. With `webhook`, the webhook receives the `oke-rdma` profile and must resolve it itself. With `none` the profile is removed but the IPvlan type stays, so a claim needs its own `addresses` and the RDMA NIC stays on the host. The profile validation does not run under `none`, so a claim that sets `interface.type: Passthrough` explicitly moves the RDMA NIC into the pod, and the OCA routing of that RDMA NIC is not restored on return.

## The child range

The child of RDMA NIC index `n` gets the address `child range + n * subnet size + host position`. The subnet is the primary VNIC subnet of the node. The default child range `10.208.0.0/12` holds 16 RDMA NICs for a `/16` subnet, the largest OCI subnet.

Set another range when the default conflicts with the network plan of the cluster. A range for 16 RDMA NICs is 4 bits larger than the subnet:

| Primary VNIC subnet | Child range for 16 RDMA NICs |
|---|---|
| `/16` | `/12` |
| `/17` | `/13` |
| `/18` | `/14` |
| `/19` | `/15` |
| `/24` | `/20` |

A smaller range holds fewer RDMA NICs. A claim for an RDMA NIC outside the range fails with an error that names the RDMA NIC, the subnet, and the range size for 16 RDMA NICs. A claim also fails when the RDMA NIC index is above 15. The range cannot be smaller than the subnet. The prefix length is from 8 to 30. The range cannot overlap the this-network (`0.0.0.0/8`), loopback, link-local, multicast, or reserved IPv4 ranges.

Set the range with the cloud provider option `oke.rdma-child-ipv4-cidr`. Each `--cloud-provider-options` flag takes one option:

```sh
--cloud-provider-options=oke.rdma-child-ipv4-cidr=10.192.0.0/14
```

With the Helm chart:

```yaml
args:
  cloudProviderOptions:
    oke:
      rdma-child-ipv4-cidr: "10.192.0.0/14"
```

DRANET stops at startup when the value is not a valid range. DRANET also stops at startup when `--cloud-provider-hint` names a provider other than OKE. When auto-discovery selects another provider, DRANET ignores the option and logs a warning.

Use the same range on all nodes that share one RDMA network. A child reaches other children through an on-link route for the range, so a child in an old range cannot reach a child in a new range. The `oke.dra.net/rdmaChildIpv4Cidr` attribute shows the configured range of each node. Prepared RDMA NICs keep the range that they started with, so drain the RDMA workloads before a change of the range.

## Native InfiniBand RDMA NICs

A native InfiniBand RDMA NIC and its RDMA device stay on the host. Host monitoring reads both in the host namespace. A node with such RDMA NICs needs these settings:

- DRANET runs with `--move-ib-interfaces=false` (`args.moveIBInterfaces: false` in the Helm chart). Each RDMA NIC is then an RDMA-only device without an interface name. Select it by the `dra.net/rdmaDevice` attribute, for example `mlx5_0`.
- The RDMA subsystem runs in shared network namespace mode (`netns_mode=1` for `ib_core`).

A pod gets only the character devices of the claimed RDMA device. No interface moves into the pod.

With another setting, the provider fails the claim with an error that names the setting. This check needs `--profile-provider=cloud`, like the profile. With `none`, the claim moves the RDMA NIC or the RDMA device into the pod.

## Oracle Cloud Agent (OCA) configuration

The provider needs OCA to configure the RDMA NICs. On a node where OCA does not configure them, an RDMA NIC without the `rdmaN` name gets no profile, and a claim moves it into the pod. An `rdmaN` NIC whose address does not follow the OCA layout fails every claim. On such nodes, use `--profile-provider=webhook` or `--profile-provider=none`, and assign the child addresses in the webhook or in the claim.

The provider assumes the OCA defaults. Mount the OCA configuration directory on an image that uses another RDMA network. With the Helm chart:

```yaml
extraVolumes:
  - name: oca-hpc-config
    hostPath:
      path: /etc/oracle-cloud-agent/plugins/oci-hpc/oci-hpc-configure
      type: DirectoryOrCreate
extraVolumeMounts:
  - name: oca-hpc-config
    mountPath: /etc/oracle-cloud-agent/plugins/oci-hpc/oci-hpc-configure
    readOnly: true
```

The provider reads the files when the process starts. Restart the DRANET pods after a change.

## Example

A DeviceClass for all DRANET devices and a claim for one RDMA NIC:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: dra.net
spec:
  selectors:
  - cel:
      expression: device.driver == "dra.net"
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: rdma15
spec:
  devices:
    requests:
    - name: rdma15
      exactly:
        deviceClassName: dra.net
        selectors:
        - cel:
            expression: has(device.attributes["dra.net"].ifName) && device.attributes["dra.net"].ifName == "rdma15"
```

Inside a pod that references the claim:

```text
rdma15  10.209.237.19/12
table 115: 10.208.0.0/12 dev rdma15 scope link src 10.209.237.19
rule: from 10.209.237.19 lookup 115
```

For a training job, request every RDMA NIC of the node in one claim, one request per RDMA NIC.
