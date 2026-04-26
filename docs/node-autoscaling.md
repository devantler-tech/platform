# Node Autoscaling

Automatic horizontal and vertical scaling of Hetzner worker nodes via the
[Kubernetes Cluster Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/hetzner)
with the Hetzner Cloud provider.

> **Requires:** [devantler-tech/ksail#4365](https://github.com/devantler-tech/ksail/pull/4365)
> (`spec.cluster.nodeAutoscaling: Enabled`).

---

## Architecture

```
KSail (static baseline)
├── 3 control planes (cx23, never autoscaled)
└── 1 static worker (cx23, guaranteed minimum)

Cluster Autoscaler (dynamic workers)
├── Pool: autoscale-small  → 0-5 × CX23 (2 vCPU, 4 GB)
├── Pool: autoscale-medium → 0-3 × CX33 (4 vCPU, 8 GB)
└── Expander: least-waste
```

- **Horizontal scaling** — autoscaler adds workers when pods are Pending due
  to insufficient resources, and removes underutilized workers after a
  configurable cooldown.
- **Vertical scaling** — multiple node pools with different server types.
  The `least-waste` expander picks the smallest pool whose nodes can fit
  the pending pod's resource requests.
- **KSail coexistence** — `nodeAutoscaling: Enabled` in `ksail.{dev,prod}.yaml`
  prevents `ksail cluster update` from modifying worker counts, avoiding
  conflicts with autoscaler-managed nodes.

### How new nodes join

1. Cluster Autoscaler detects Pending pods with unmet resource requests.
2. It calls the Hetzner API to create a new server using:
   - `HCLOUD_IMAGE` — a Talos snapshot (not the ISO).
   - `cloudInit` from `HCLOUD_CLUSTER_CONFIG` — the Talos worker machine
     config YAML. Hetzner passes this as user-data; Talos reads it on boot.
3. The server boots Talos, applies the machine config, and joins the cluster.
4. Once the node is Ready, pending pods are scheduled.

---

## Prerequisites

### 1. Create a Talos Hetzner snapshot

The Cluster Autoscaler creates servers with `--image`, which requires a
Hetzner Cloud **image** (snapshot), not an ISO. KSail boots from ISO `122630`
but the autoscaler can't use ISOs.

**Option A — Rescue mode (simplest)**

```bash
# 1. Create a temporary CX22 server (cheapest)
hcloud server create --name talos-snapshot-builder \
  --type cx22 --location fsn1 --image ubuntu-22.04

# 2. Enable rescue mode and reboot
hcloud server enable-rescue talos-snapshot-builder --type linux64
hcloud server reboot talos-snapshot-builder

# 3. SSH in and write the Talos image to disk
ssh root@<server-ip>
cd /tmp
# Use the same schematic as KSail's ISO (Hetzner + qemu-guest-agent)
wget -O talos.raw.xz "https://factory.talos.dev/image/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515/v1.11.2/hcloud-amd64.raw.xz"
xz -d -c talos.raw.xz | dd of=/dev/sda bs=4M && sync
shutdown -h now

# 4. Create a snapshot from the Hetzner console or CLI
hcloud server create-image talos-snapshot-builder --type snapshot \
  --description "Talos v1.11.2 amd64"
# Note the snapshot ID from the output

# 5. Clean up the temporary server
hcloud server delete talos-snapshot-builder
```

**Option B — Packer (automated, repeatable)**

See the [Talos Hetzner installation guide](https://www.talos.dev/v1.11/talos-guides/install/cloud-platforms/hetzner/)
for a packer configuration.

### 2. Generate Talos worker machine config

Extract a worker machine config compatible with the existing cluster:

```bash
# From a machine with talosctl configured for the cluster:
talosctl gen config <cluster-name> https://<api-endpoint>:6443 \
  --output-types worker \
  --with-examples=false --with-docs=false \
  --config-patch @talos/cluster/cni.yaml \
  --config-patch @talos/cluster/rotate-server-certificates.yaml \
  --config-patch @talos/workers/longhorn.yaml
```

> **Important:** The generated config must use the same cluster CA and
> bootstrap token as the running cluster. If you're unsure, extract the
> secrets from the existing cluster first:
> ```bash
> talosctl -n <cp-ip> get machineconfig -o yaml
> ```

### 3. Build the cluster-config Secret

1. Base64-encode the worker machine config:
   ```bash
   cat worker.yaml | base64 -w0 > worker-b64.txt
   ```

2. Edit the Secret at
   `k8s/providers/hetzner/infrastructure/controllers/cluster-autoscaler/cluster-autoscaler-config-secret.yaml`:
   - Replace `PLACEHOLDER_BASE64_TALOS_WORKER_CONFIG` with the base64 content
     for each pool.

3. Encrypt with SOPS:
   ```bash
   sops -e cluster-autoscaler-config-secret.yaml > cluster-autoscaler-config-secret.enc.yaml
   ```

4. Commit `cluster-autoscaler-config-secret.enc.yaml`, delete the plaintext.

### 4. Set the Talos snapshot ID

Update `autoscaler_talos_image` in both:
- `k8s/clusters/prod/variables/variables-cluster-config-map.yaml`
- `k8s/clusters/dev/variables/variables-cluster-config-map.yaml`

---

## Configuration

All autoscaler parameters are configurable via per-environment variables in
`k8s/clusters/<env>/variables/variables-cluster-config-map.yaml`:

| Variable | Default | Description |
|----------|---------|-------------|
| `autoscaler_talos_image` | — | Hetzner snapshot ID for Talos worker nodes |
| `autoscaler_small_server_type` | `cx23` | Server type for the small pool |
| `autoscaler_small_pool_min` | `0` | Minimum nodes in the small pool |
| `autoscaler_small_pool_max` | `5` | Maximum nodes in the small pool |
| `autoscaler_medium_server_type` | `cx33` | Server type for the medium pool |
| `autoscaler_medium_pool_min` | `0` | Minimum nodes in the medium pool |
| `autoscaler_medium_pool_max` | `3` | Maximum nodes in the medium pool |
| `autoscaler_location` | `fsn1` | Hetzner datacenter for autoscaled nodes |

### Cost guardrails

- **Hard max per pool** — `autoscaler_*_pool_max` caps each pool.
- **Expander** — `least-waste` prefers cheaper, smaller nodes when possible.
- **Scale-down** — underutilized nodes are removed after 10 minutes
  (`scale-down-unneeded-time`).

### Adding more pools

1. Add a new entry to `autoscalingGroups` in the HelmRelease.
2. Add a matching `nodeConfigs` entry in the cluster-config Secret.
3. Add variables for the new pool's min/max/type.

---

## Troubleshooting

### Autoscaler not scaling up

```bash
# Check autoscaler logs
kubectl -n kube-system logs -l app.kubernetes.io/name=cluster-autoscaler --tail=100

# Check for unschedulable pods
kubectl get pods -A --field-selector=status.phase=Pending

# Check autoscaler status ConfigMap
kubectl -n kube-system get cm cluster-autoscaler-status -o yaml
```

### Autoscaler nodes not joining

```bash
# Check if the Hetzner server was created
hcloud server list --selector cluster.autoscaler.nodeGroupLabel

# Check Talos bootstrap status (if server IP is known)
talosctl -n <node-ip> health

# Verify the machine config is valid
talosctl validate --config worker.yaml --mode cloud
```

### KSail conflict

If `ksail cluster update` unexpectedly modifies node counts:
1. Verify `nodeAutoscaling: Enabled` is set in the ksail config.
2. Verify KSail version includes [#4365](https://github.com/devantler-tech/ksail/pull/4365).

### Cluster rebuild

After a full cluster rebuild (`ksail cluster delete` + `create`):
1. The Talos snapshot can be reused (it's version-specific, not cluster-specific).
2. The worker machine config must be **regenerated** — it contains the cluster
   CA and bootstrap token which change on every `cluster create`.
3. Re-encrypt the cluster-config Secret and commit.

---

## Maintenance

### Talos version upgrades

When bumping the Talos version in `ksail.{dev,prod}.yaml`:
1. Create a new Talos snapshot matching the new version.
2. Update `autoscaler_talos_image` in both cluster variable files.
3. Regenerate the worker machine config if the Talos config schema changed.

### Hetzner server type changes

Hetzner periodically renames or retires server types. Check the
[Hetzner Cloud changelog](https://docs.hetzner.cloud/changelog) and update
the `autoscaler_*_server_type` variables accordingly.
