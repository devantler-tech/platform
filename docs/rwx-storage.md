# RWX Storage with Longhorn

Longhorn provides ReadWriteMany (RWX) storage on Hetzner clusters (prod) using
replicated data on the worker nodes' EPHEMERAL filesystems. It replaces the
`hcloud` StorageClass as the cluster default.

> **Local Docker clusters do not support Longhorn** — the iSCSI kernel modules required by Longhorn are not available in Docker-based Talos containers.

## Architecture

```
Worker EPHEMERAL filesystem
  └── /var/lib/longhorn (shared into kubelet)
        └── Longhorn engine and replicas
              ├── longhorn StorageClass (default — RWO + RWX)
              └── hcloud StorageClass (non-default — Hetzner block only)
```

## Prerequisites

### 1. Custom Talos installer image with Longhorn extensions

Longhorn requires two Talos system extensions baked into the installer image.
The installer image also includes one additional recommended extension:

- `siderolabs/iscsi-tools` — **required** by Longhorn for the iSCSI initiator/target data plane
- `siderolabs/util-linux-tools` — **required** by Longhorn for `fstrim` volume trimming
- `siderolabs/qemu-guest-agent` — **recommended** for Hetzner Cloud VM integration (not required by Longhorn)

The extensions are configured declaratively in `ksail.prod.yaml` as
`spec.cluster.talos.extensions`. KSail computes the [Talos Image
Factory](https://factory.talos.dev) schematic ID from that list during config
generation and sets `machine.install.image` automatically (the same schematic
also backs the Hetzner snapshot the Cluster Autoscaler boots new nodes from) —
there is no hand-maintained installer-image patch. Nodes boot from the standard
Hetzner Talos ISO but install the custom image (with extensions) to disk during
first boot or upgrade.

To **change the extension set** (or bump the Talos version), edit
`spec.cluster.talos.extensions` (or `spec.cluster.talos.version`) in
`ksail.prod.yaml` and re-run `ksail cluster update`; KSail recomputes the
schematic and rolls the new installer image to the nodes. You never derive or
paste a schematic ID by hand.

To **apply the image to a single node manually** (e.g. recovering a node that
fell behind a roll), read the derived installer image off a healthy node and
reuse it:

```bash
# The installer image KSail derived for the cluster
IMAGE=$(talosctl --nodes <healthy-IP> get machineconfig -o jsonpath='{.spec.machine.install.image}')

talosctl upgrade --nodes <IP> --image "$IMAGE" --preserve
```

### 2. Worker EPHEMERAL storage

Longhorn stores replicas at `/var/lib/longhorn` on each worker's EPHEMERAL
filesystem. The `talos/workers/mount-longhorn-data.yaml` patch exposes that
host path to kubelet with the recursive shared propagation Longhorn's CSI node
plugin requires. It does not provision or mount a separate block device.

Talos 1.14 makes new EPHEMERAL volumes `noexec` by default, while Longhorn v1
executes its engine binaries below `/var/lib/longhorn`. The worker-only
`talos/workers/allow-longhorn-execution.yaml` patch disables the secure mount
bundle for EPHEMERAL on storage workers so fresh and rebuilt nodes can start
Longhorn. Control planes retain Talos's secure mount defaults.

Do not partition `/dev/sdb` for Longhorn: Hetzner's CSI driver dynamically
attaches Cloud Volumes at `/dev/sdb`, `/dev/sdc`, and later device names for
PVCs using the separate `hcloud` StorageClass.

## StorageClasses

| StorageClass | Default | Access Modes | Backing |
| --- | --- | --- | --- |
| `longhorn` | ✅ Yes | RWO, RWX | Replicated worker EPHEMERAL storage |
| `hcloud` | ❌ No | RWO only | Hetzner Cloud Block Storage |

### Using RWX volumes

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-data
spec:
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: 5Gi
  # storageClassName: longhorn  # optional — it's the default
```

### Using Hetzner block storage explicitly

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fast-block
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: hcloud
  resources:
    requests:
      storage: 10Gi
```

## Tunable variables

These variables can be overridden per environment in `k8s/clusters/<env>/bootstrap/config-map.yaml`:

| Variable | Default | Description |
| --- | --- | --- |
| `longhorn_replica_count` | `3` | Number of volume replicas (matches the storage-worker count) |
| `longhorn_csi_attacher_replicas` | `1` | CSI attacher replica count |
| `longhorn_csi_provisioner_replicas` | `1` | CSI provisioner replica count |
| `longhorn_csi_resizer_replicas` | `1` | CSI resizer replica count |
| `longhorn_csi_snapshotter_replicas` | `1` | CSI snapshotter replica count |
| `longhorn_ui_replicas` | `1` | Longhorn UI replica count |

## Talos upgrades

When upgrading Talos nodes, **always use `--preserve`** to avoid wiping `/var/lib/longhorn`:

```bash
talosctl upgrade --nodes <IP> --image <IMAGE> --preserve
```

See [Longhorn Talos Linux Support](https://longhorn.io/docs/advanced-resources/os-distro-specific/talos-linux-support/#talos-linux-upgrades) for recovery steps if data is accidentally wiped.

## Scaling

Longhorn capacity scales with the EPHEMERAL storage available across the
labelled baseline workers. Add or replace a storage worker with sufficient root
disk capacity, wait for replicas to become healthy, and only then drain the old
worker. Hetzner Cloud Volumes are independent PVC backends for the `hcloud`
StorageClass; resizing one does not add capacity to Longhorn.
