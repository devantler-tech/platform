# RWX Storage with Longhorn

Longhorn provides ReadWriteMany (RWX) storage on Hetzner clusters (dev/prod) using dedicated Hetzner Cloud Volumes attached to each worker node. It replaces the `hcloud` StorageClass as the cluster default.

> **Local Docker clusters do not support Longhorn** — the iSCSI kernel modules required by Longhorn are not available in Docker-based Talos containers.

## Architecture

```
Hetzner Cloud Volume (per worker)
  └── mounted at /var/lib/longhorn (Talos machine config)
        └── Longhorn engine
              ├── longhorn StorageClass (default — RWO + RWX)
              └── hcloud StorageClass (non-default — Hetzner block only)
```

## Prerequisites

### 1. Custom Talos ISO with Longhorn extensions

Longhorn requires two Talos system extensions baked into the installer image:

- `siderolabs/iscsi-tools` — iSCSI initiator/target for Longhorn's data plane
- `siderolabs/util-linux-tools` — provides `fstrim` for Longhorn volume trimming

Generate a custom ISO via [Talos Image Factory](https://factory.talos.dev):

1. Select **Talos v1.11.2** (matching the version pinned in `ksail.{dev,prod}.yaml`)
2. Choose **Hetzner Cloud** as the platform
3. Add system extensions: `iscsi-tools`, `util-linux-tools`
4. Download the **ISO** (amd64)
5. Upload to Hetzner Cloud:
   ```bash
   hcloud iso upload --name talos-v1.11.2-longhorn talos-amd64.iso
   ```
6. Note the ISO ID from the output
7. Update `ksail.dev.yaml` and `ksail.prod.yaml`:
   ```yaml
   spec:
     cluster:
       talos:
         iso: <NEW_ISO_ID>
   ```

### 2. Hetzner Cloud Volumes for workers

Each worker node needs a dedicated Hetzner Cloud Volume mounted at `/var/lib/longhorn`.

**Create and attach volumes** (repeat for each worker):

```bash
# List servers to find worker names
hcloud server list

# Create a volume and attach it to a worker
# The volume appears as /dev/sdb on the worker
hcloud volume create \
  --name <cluster>-worker-<n>-longhorn \
  --size 20 \
  --server <worker-server-name> \
  --format ext4 \
  --location fsn1
```

The Talos machine config patch (`talos/workers/longhorn.yaml`) handles mounting `/dev/sdb` at `/var/lib/longhorn`.

## StorageClasses

| StorageClass | Default | Access Modes | Backing |
|---|---|---|---|
| `longhorn` | ✅ Yes | RWO, RWX | Longhorn on Hetzner volumes |
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

These variables can be overridden per environment in `k8s/clusters/<env>/variables/variables-cluster-config-map.yaml`:

| Variable | Default | Description |
|---|---|---|
| `longhorn_replica_count` | `2` | Number of volume replicas (should match worker count) |
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

To change the Hetzner volume size:

```bash
# Volumes can only be resized up, not down
hcloud volume resize --size 50 <volume-id>
```

After resizing, Longhorn detects the additional space automatically.
