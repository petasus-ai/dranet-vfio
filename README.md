# dranet-vfio

DRA driver (fork of [kubernetes-sigs/dranet](https://github.com/kubernetes-sigs/dranet))
that gives KubeVirt VMs VFIO-PCI passthrough NICs — Ethernet and InfiniBand,
PF and VF alike — allocated through `resource.k8s.io/v1`. No CNI, no Multus,
no device plugin.

Driver name: `vfio.petasus.io`.

## How it works

- Hardware is discovered from sysfs: every PCI function bound to `vfio-pci`
  whose class is a network controller (`0x02xx`/`0x0c06xx`) is published —
  a vfio-pci binding already means passthrough intent. There is no driver
  configuration; selection policy lives entirely in DeviceClass CEL over the
  published attributes.

  Each device is published as a ResourceSlice device `pci-<bdf>` with the
  standard `resource.kubernetes.io/pciBusID` attribute and
  `vfio.petasus.io/{kind,linkType,vendor,deviceID,pfName,pfPciAddress,vfIndex,iommuGroup,pciClass,numaNode,deviceType,rdmaCapable}`
  (plus the uplink VLAN snapshot on Ethernet VFs whose PF keeps a netdev).
  The host is rescanned periodically (`--rescan-interval`), so newly bound
  devices need no driver restart.

- On `NodePrepareResources` the driver validates the device (vfio-pci
  binding, network PCI class, IOMMU-group viability) and applies the claim's
  VF settings on the host — the behavior of the sriov-vfio CNI, absorbed:

  - **Ethernet VF, legacy eswitch:** MAC (with read-back verification),
    VLAN, spoofchk, trust, min/max TX rate via the PF; link state auto.
  - **Ethernet VF, switchdev eswitch:** the VF representor is attached to
    the bridge over the uplink (PF or its bond), with MTU inheritance,
    access-VLAN programming validated against the uplink's bridge-port
    membership, and per-bridge flock serialization.
  - **Everything else** (Ethernet PF, InfiniBand PF/VF): pure passthrough,
    no host configuration; any VF option in the claim fails prepare.

  Original VF settings are captured first and restored on
  `NodeUnprepareResources`, surviving driver restarts (bbolt, `--db-path`).

- Via NRI, `/dev/vfio/vfio` and `/dev/vfio/<iommu group>` are injected into
  the pod's containers owned by uid/gid 107 (qemu in virt-launcher), and the
  KEP-5304 device metadata file is bind-mounted at
  `/var/run/kubernetes.io/dra-device-attributes/resourceclaims/<claim>/<request>/vfio.petasus.io-metadata.json`,
  where KubeVirt's DRA path reads `resource.kubernetes.io/pciBusID` to build
  the libvirt `<hostdev>`.

- VF settings arrive per claim via the opaque device config:

  ```yaml
  config:
    - requests: ["nic"]
      opaque:
        driver: vfio.petasus.io
        parameters:
          mac: "02:00:00:aa:bb:cc"   # omitted for InfiniBand
          vlan: 1234                 # network segment ID; 0/omitted = untagged
          # spoofchk/trust ("on"|"off"), minTxRate/maxTxRate (Mbps),
          # bridge (switchdev auto-detection override) — all optional
  ```

KubeVirt consumes the device with `interfaces[].sriov: {}` and
`spec.networks[].resourceClaim` (`NetworkDevicesWithDRA` feature gate); the
PCI address flows through the metadata file, not a network-status
annotation.

## Deploy

```sh
kubectl apply -f deployments/dranet-vfio.yaml
kubectl apply -f deployments/examples/bundang-10f-sriov.yaml   # example DeviceClasses
```

The upstream dranet documentation below describes the inherited machinery.

---

# DRANET: DRA Kubernetes Network Driver

DRANET is a Kubernetes Network Driver that uses Dynamic Resource Allocation
(DRA) to deliver high-performance networking for demanding applications in
Kubernetes.

## Key Features

- **DRA Integration:** Leverages the power of Kubernetes' Dynamic Resource
  Allocation.
- **High-Performance Networking:** Designed for demanding workloads like AI/ML
  applications.
- **Simplified Management:** Easy to deploy and manage.
- **Enhanced Efficiency:** Optimizes resource utilization for improved overall
  performance.
- **Cluster-Wide Scalability:**  Effectively manages network resources across a
  large number of nodes for seamless operation in Kubernetes deployments.

Our research paper, **"[The Kubernetes Network Driver Model: A Composable Architecture for High-Performance Networking](/site/static/docs/kubernetes_network_driver_model_dranet_paper.pdf)"**, provides a deep dive into the DRANET model and its impact.

<p align="center">
<img src="site/static/images/nccl_all_gather_results.png" width="400" height="300">   <img src="site/static/images/nccl_all_reduce_results.png" width="400" height="300">
</p>

The key findings include:

- **Up to 60% Bandwidth Increase:** By enabling topology-aware scheduling of GPUs and NICs, DRANET boosts bus bandwidth by up to 59.6% for `all_gather` and 58.1% for `all_reduce` operations in distributed AI/ML workloads.
- **Operational Simplicity:** The paper demonstrates how the KND model used by DRANET drastically simplifies the management of high-performance hardware, replacing fragile, multi-component chains with a clean, composable architecture.

## How It Works

The DRANET driver communicates with the Kubelet through the [DRA
API](https://github.com/kubernetes/kubernetes/blob/3bec2450efd29787df0f27415de4e8049979654f/staging/src/k8s.io/kubelet/pkg/apis/dra/v1beta1/api.proto)
and with the Container Runtime via [NRI](https://github.com/containerd/nri).
This architectural approach ensures robust supportability and minimizes
complexity, making it fully compatible with existing CNI plugins in your
cluster.

Upon the creation of a Pod's network namespaces, the Container Runtime initiates
a GRPC call to DRANET via NRI to execute the necessary network configurations.

A more detailed diagram illustrating this process can be found in our
documentation: [How It
Works](https://dranet.sigs.k8s.io/docs/concepts/howitworks/).

## Quick Start

To get started with DRANET, your Kubernetes cluster needs to have [Dynamic
Resource Allocation (DRA)
enabled](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/).
DRA is beta and is disabled by default in Kubernetes v1.32. You will need to
enable both the [feature gates and the API
groups](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#enabling-dynamic-resource-allocation)
for DRA until it reaches GA.

![](site/static/images/dranet.gif)

### Kubernetes Cluster with DRA

#### KIND

If you are using
[KIND](https://github.com/kubernetes-sigs/kind?tab=readme-ov-file#installation-and-usage),
you can create a cluster with the following configuration:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: kindest/node:v1.36.1
- role: worker
  image: kindest/node:v1.36.1
- role: worker
  image: kindest/node:v1.36.1
```

Then to create the cluster:

```sh
kind create cluster --config kind.yaml
```

#### Google Cloud (GKE)

For instructions on setting up DRA on GKE, refer to the official documentation:
[Set up Dynamic Resource
Allocation](https://cloud.google.com/kubernetes-engine/docs/how-to/set-up-dra)

### Installation

Install the latest stable version of DRANET using the provided manifest:

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/dranet/refs/heads/main/install.yaml
```

or using the Helm chart:

```sh
helm install -n kube-system dranet oci://registry.k8s.io/networking/charts/dranet --version v1.4.0
```

For available configuration options, see the [chart README](deployments/helm/dranet/README.md).

DRANET persists prepared pod device configuration in a local bbolt database so
NRI hooks can continue initialization after a driver restart. The default DB
path is `/var/run/dranet/dranet.db`, so deployment manifests must mount
`/var/run/dranet` as writable storage (hostPath with `DirectoryOrCreate` in the
provided manifests).

You can override the DB path with `--db-path`; set it to an empty
string to disable persistence and use in-memory state.

### How to Use It

Once DRANET is running, you can inspect the network interfaces and their
attributes published by the drivers. Users can then create `DeviceClasses`,
`ResourceClaims`, and/or `ResourceClaimTemplates` to schedule pods and allocate
network devices.

For examples of how to use DRANET with `DeviceClass` and `ResourceClaim` to
attach network interfaces to pods, please refer to the [Quick Start
guide](https://dranet.sigs.k8s.io/docs/quick-start).


## Contributing

We welcome your contributions! Please check out our [CONTRIBUTING.md](CONTRIBUTING.md) for more information.

## Further Reading

Explore more concepts and advanced topics:

* **Design:** Understand the architectural choices behind DRANET:
  [Design](https://dranet.sigs.k8s.io/docs/concepts/howitworks)
* **RDMA:** Learn about RDMA components in Linux and their interplay:
  [RDMA](https://dranet.sigs.k8s.io/docs/concepts/rdma)
* **References:** A list of relevant Kubernetes Enhancement Proposals (KEPs) and
  presentations:
  [References](https://dranet.sigs.k8s.io/docs/concepts/references)
