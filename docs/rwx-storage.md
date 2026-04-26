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

### 1. Custom Talos installer image with Longhorn extensions

Longhorn requires two Talos system extensions baked into the installer image.
The installer image also includes one additional recommended extension:

- `siderolabs/iscsi-tools` — **required** by Longhorn for the iSCSI initiator/target data plane
- `siderolabs/util-linux-tools` — **required** by Longhorn for `fstrim` volume trimming
- `siderolabs/qemu-guest-agent` — **recommended** for Hetzner Cloud VM integration (not required by Longhorn)

The extensions are configured via [Talos Image Factory](https://factory.talos.dev).
A schematic ID encodes the set of extensions; it is referenced in
`talos/cluster/install-image.yaml` as `machine.install.image`. Nodes boot
from the standard Hetzner Talos ISO but install the custom image (with
extensions) to disk during first boot or upgrade.

To **regenerate the schematic** (e.g. when adding more extensions or bumping
the Talos version):

```bash
# Create a new schematic via the Image Factory API
curl -s -X POST https://factory.talos.dev/schematics \
  -H 'Content-Type: application/yaml' -d '
customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/iscsi-tools
      - siderolabs/util-linux-tools
      - siderolabs/qemu-guest-agent
'
# → {"id":"<SCHEMATIC_ID>"}

# Update talos/cluster/install-image.yaml:
#   machine.install.image: factory.talos.dev/installer/<SCHEMATIC_ID>:v<TALOS_VERSION>
```

To **apply the new image to existing nodes**:

```bash
talosctl upgrade \
  --nodes <IP> \
  --image factory.talos.dev/installer/<SCHEMATIC_ID>:v<TALOS_VERSION> \
  --preserve
```

### 2. Hetzner Cloud Volumes for workers

Each worker node needs a dedicated Hetzner Cloud Volume mounted at `/var/lib/longhorn`.

**Create and attach volumes** (repeat for each worker):

```bash
# List servers to find worker names
hcloud server list

# Create a volume and attach it to a worker
# Do NOT use --format — Talos expects to partition/format the disk itself
# The volume appears as /dev/sdb on the worker
hcloud volume create \
  --name <cluster>-worker-<n>-longhorn \
  --size 50 \
  --server <worker-server-name>
```

The Talos machine config patch (`talos/workers/longhorn.yaml`) handles mounting `/dev/sdb` at `/var/lib/longhorn`.

> **Verify the device path** after attaching: on Hetzner Cloud, the first attached volume
> consistently appears as `/dev/sdb`. Confirm with `talosctl disks --nodes <worker-ip>`.
> If the volume shows a different path, update `talos/workers/longhorn.yaml` accordingly.

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

After resizing, the Hetzner block device grows immediately but the XFS partition and filesystem must be expanded:

```bash
# From a privileged pod on the worker (or via talosctl debug container):
sgdisk -e /dev/sdb          # Fix GPT to use all space
growpart /dev/sdb 1          # Grow partition 1 to fill disk
xfs_growfs /var/lib/longhorn # Expand XFS filesystem online
```

Longhorn detects the additional space automatically once the filesystem is grown.
