# RWX Storage with Longhorn

Longhorn provides ReadWriteMany (RWX) storage on Hetzner clusters (prod). It stores replicas on each storage worker's root filesystem under `/var/lib/longhorn`, and its `longhorn-wffc` StorageClass is the cluster default.

> **Local Docker clusters do not support Longhorn** — the iSCSI kernel modules required by Longhorn are not available in Docker-based Talos containers.

## Architecture

The Longhorn dashboard is available at `https://longhorn.<platform-domain>`
and through the Homepage Infrastructure group. It uses the existing Dex OIDC
login and requires membership in the platform maintainer team. This browser
access is read-only: the authentication proxy and network policies allow only
audited GET/HEAD paths and the snapshot/backup listing and retrieval actions that
Longhorn implements with POST. Administrative actions are denied, including
changes made by sending API requests directly. Longhorn may still display
administrative controls; using them does not grant write access. Operational
changes continue through reviewed configuration and protected workflows.
Diagnostic-bundle and backing-image downloads are unavailable in this view;
Longhorn can delete a diagnostic bundle as a side effect of downloading it.

The route exists only in the Hetzner deployment. Local Docker deployments
continue without a Longhorn dashboard. Recheck the read-action list against
Longhorn's API when upgrading it; new POST actions remain denied by default.

```
Worker root filesystem: /var/lib/longhorn (bind-mounted into the kubelet by Talos)
  └── Longhorn engine
        ├── longhorn-wffc StorageClass (default — RWO + RWX, WaitForFirstConsumer)
        └── longhorn StorageClass (non-default — RWO + RWX, Immediate)
hcloud StorageClass (non-default — Hetzner block only)
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

### 2. Longhorn data directory on workers

Longhorn needs no extra disk. The Talos patch `talos/workers/mount-longhorn-data.yaml`
bind-mounts `/var/lib/longhorn` from the worker's root filesystem into the kubelet so the CSI
driver can reach it. Do not partition `/dev/sdb` for Longhorn: hcloud-csi attaches its Cloud
Volumes at `/dev/sdb`, `/dev/sdc` and so on, and partitioning would destroy them.

## Storage consumers on compute-only workers

Compute-only (autoscaled) workers run the Longhorn volume client, so a pod can
mount a Longhorn volume there, but they hold no replicas: replica storage stays
on the storage workers. A volume consumer such as Kubescape's result store can
therefore run on any Linux worker. Kyverno gives the store the soft
`prefer-baseline-nodes` preference at admission, so it lands on a baseline worker
whenever one has room.

The production regression `scripts/tests/test-kubescape-storage-placement.sh`
renders the pinned Kubescape chart for install and upgrade and checks that its
store is not pinned to the storage workers by a node selector, a required node
affinity or a node name, that no post-renderer changes any component's
placement, and that the store keeps its single-writer strategy, claim, security
settings and health probes.

## StorageClasses

| StorageClass | Default | Access Modes | Backing |
| --- | --- | --- | --- |
| `longhorn-wffc` | ✅ Yes | RWO, RWX | Longhorn on worker root filesystems; binds when the consuming pod is scheduled (WaitForFirstConsumer) |
| `longhorn` | ❌ No | RWO, RWX | Same Longhorn backing, binds immediately; kept for existing claims that name it |
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
  # storageClassName: longhorn-wffc  # optional — it's the default
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

Longhorn's capacity on each storage worker is the free space on that worker's root filesystem.
There is no separate volume to resize; capacity follows the worker's disk and the number of
storage workers.
