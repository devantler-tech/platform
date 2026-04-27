# Disaster recovery runbook

The single source of truth for "how do I get the platform back" — covering
single-node loss, full-cluster loss, and credential rotation. Designed so
that with this repo + the off-cluster artifacts listed below + ~30 minutes
of manual control-plane work, prod can be reconstructed to a state
indistinguishable from the day before the incident.

> **RPO target:** 24 h (daily snapshots).
> **RTO target:** 4 h (mostly slack for manual Hetzner / Cloudflare clicks;
> the automated portion is &lt; 15 minutes in CI).

---

## Off-cluster artifacts you must keep safe

The repo + these items are the entire seed for a rebuild. Lose all of
these simultaneously and you cannot recover.

| Artifact                                | Where it lives                       | Recovery if lost                     |
| --------------------------------------- | ------------------------------------ | ------------------------------------ |
| **SOPS Age private keys** (one per env) | Secure vault + offline backup        | Re-encrypt all `*.enc.yaml` (below)  |
| **Cloudflare R2 access keys**           | Secure vault                         | Mint new in Cloudflare; SOPS-update  |
| **Hetzner Cloud API token**             | Secure vault                         | Mint new in Hetzner Cloud console    |
| **Cloudflare API token**                | Secure vault                         | Mint new in Cloudflare dashboard     |

> Recommendation: store these in a shared vault accessible by at least one
> additional trusted operator, plus an offline copy in a second physical
> location.

---

## Scenario 1 — Single node loss

Expected behaviour: PDBs keep every multi-replica workload serving traffic.
Re-scale workers or re-run `ksail cluster update` to replace the lost node.

```bash
# Inspect state
kubectl get nodes
kubectl get pods -A --field-selector=status.phase!=Running
kubectl get pdb -A    # all should show ALLOWED-DISRUPTIONS=1

# Replace the failed node (re-runs Hetzner provisioning for missing members)
ksail --config ksail.prod.yaml cluster update
```

If any workload is stuck in Pending because all replicas were on the dead
node and the PDB is blocking eviction on the new one, force a rollout:

```bash
kubectl -n <ns> rollout restart deployment/<name>
```

---

## Scenario 2 — Planned rolling Talos / Kubernetes upgrade

Bump the Talos ISO ID in `ksail.prod.yaml` (or the Kubernetes version
in the ksail config) and re-run `ksail cluster update`. ksail cordons and
replaces nodes one at a time; PDBs hold the line.

```bash
# Pre-flight: confirm every multi-replica workload has a PDB
kubectl get pdb -A

# Pre-flight: confirm RollingUpdate strategy uses maxUnavailable: 0
kubectl get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}\t{.spec.strategy.rollingUpdate.maxUnavailable}{"\n"}{end}'

# Apply the upgrade
ksail --config ksail.prod.yaml cluster update
```

If anything reports `maxUnavailable` other than `0`, that workload was
either added without an HA configuration or has a chart limitation — fix
before upgrading.

---

## Scenario 3 — etcd corruption / control-plane loss

With Omni retired, there is no managed etcd snapshot. Recovery path is
**full cluster rebuild** (Scenario 4) followed by Velero + CNPG restores.
This is an accepted trade-off documented in the migration decision:
workload state lives in R2-backed Velero and CNPG backups; the control
plane is a cattle resource that ksail can re-provision in &lt; 15 min.

---

## Scenario 4 — Full cluster rebuild from zero

The "everything is gone" path. ~10 min of Hetzner provisioning + ~15 min of
Flux reconciliation.

```bash
# 1. Set credentials locally
export HCLOUD_TOKEN=<hetzner-cloud-api-token>
export GHCR_TOKEN=<ghcr-pat-with-packages-read-write>
export SOPS_AGE_KEY_FILE=~/.config/sops/age/keys.txt  # points at the env's Age key

# 2. Boot a fresh cluster (ksail handles Talos boot, CCM, CSI, kubeconfig)
ksail --config ksail.prod.yaml cluster create

# 3. Bootstrap Flux from this repo
ksail --config ksail.prod.yaml workload push       # packages -> GHCR
ksail --config ksail.prod.yaml workload reconcile  # Flux pulls and applies

# 4. Wait for Flux to settle
flux get kustomizations -A
# Re-run if any are NotReady; expect convergence in 10-15 minutes

# 5. Point public DNS at the new Hetzner Cloud Load Balancer
kubectl -n kube-system get svc cilium-gateway-platform \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
# Update A/AAAA records for ${domain} and *.${domain} at your DNS provider.

# 6. Restore Velero backups (apps + PVCs)
kubectl -n velero create -f - <<EOF
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: rebuild-$(date +%s)
  namespace: velero
spec:
  backupName: <pick-latest-from-velero-backup-get>
  includedNamespaces:
    - "*"
  excludedNamespaces:
    - kube-system
    - velero
EOF

# 7. (If any CNPG Cluster exists) restore from R2
kubectl cnpg restore <new-cluster-name> \
  --backup <backup-name> \
  --target-time '<RFC3339-timestamp-or-omit-for-latest>'
```

If this is the **first time** restoring after losing the SOPS keys, replace
step 3 with the rotation flow in Scenario 6 first.

---

## Scenario 5 — Velero / CNPG restore (single namespace or app)

Quick path for "I deleted the wrong PVC" or "this Postgres database needs
to roll back to last night".

```bash
# Find the relevant backup
kubectl -n velero get backups
velero backup get   # if velero CLI installed locally

# Namespace restore
kubectl -n velero create -f - <<EOF
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: ns-restore-$(date +%s)
  namespace: velero
spec:
  backupName: daily-full-<date>
  includedNamespaces: ["<your-ns>"]
EOF

# CNPG point-in-time recovery (PITR is "free" once WAL archiving is on)
kubectl cnpg restore <new-cluster-name> \
  --source-cluster <old-cluster> \
  --target-time '2026-04-17T22:00:00Z'
```

---

## Scenario 6 — SOPS Age key rotation

```bash
# 1. Generate a new key
age-keygen -o new-key.txt
NEW_PUB=$(grep '^# public key' new-key.txt | cut -d: -f2 | tr -d ' ')

# 2. Add the new pub key as a recipient *before* removing the old one
#    (gives you a window where both keys can decrypt).
yq -i ".creation_rules[].age += \",\n$NEW_PUB\"" .sops.yaml

# 3. Re-encrypt every SOPS file with the new recipient list
find . -name '*.enc.yaml' -print0 | xargs -0 -n1 sops updatekeys --yes

# 4. Commit + merge. Verify Flux still decrypts (no errors in
#    flux-system pods).

# 5. Rotate the new key into your secret store, distribute to operators.

# 6. Once everyone is on the new key, drop the old one from .sops.yaml
#    and re-run sops updatekeys --yes one more time.

# 7. Securely destroy old-key.txt copies.
```

---

## Scenario 7 — R2 / Cloudflare credential rotation

```bash
# 1. Mint a new R2 token in the Cloudflare dashboard (scoped to your
#    <your-bucket> bucket only). DO NOT revoke the old one
#    yet -- there is a window where both must work.

# 2. Update the encrypted secret in-place
sops --set '["stringData"]["r2_access_key_id"] "<new-id>"' \
  k8s/bases/variables/variables-base-secret.enc.yaml
sops --set '["stringData"]["r2_secret_access_key"] "<new-secret>"' \
  k8s/bases/variables/variables-base-secret.enc.yaml

# 3. PR + merge. Flux propagates within one reconciliation cycle.

# 4. Wait one Velero schedule + one CNPG WAL archive cycle to confirm
#    the new credentials work end-to-end.
kubectl -n velero get backups -w
kubectl logs -n cnpg-system -l app.kubernetes.io/name=cloudnative-pg --tail=50

# 5. Revoke the old token in Cloudflare.
```

---

## Encryption-at-rest verification

Run after any node replacement to confirm secrets are still ciphertext on
disk.

```bash
# Pull a fresh etcd snapshot via talosctl
talosctl --nodes <cp-node> etcd snapshot /tmp/etcd.snapshot

# Inspect a Secret -- must NOT be plain text
etcdctl --endpoints unix:///tmp/etcd.snapshot \
  get --prefix /registry/secrets/ | head -c 200
# Expect bytes that look like cipher (binary garbage). If you see
# Kubernetes Secret YAML, the EncryptionConfiguration was lost.
```

This check is also asserted by the CI restore drill (see [restore-drill.md](./restore-drill.md)).

---

## Local clusters

Local clusters are ephemeral and reconstructed from this repo on every
`ksail cluster create`. There is nothing meaningful to back up — the
restore procedure for local is:

```bash
ksail cluster delete
ksail cluster create
ksail workload push && ksail workload reconcile
```

CI exercises this on every PR (`.github/workflows/ci.yaml`), and also
exercises a Velero backup → restore against the in-cluster
MinIO so the prod code path is regression-tested.

---

## Scenario 8 — Cluster Autoscaler issues

### Autoscaler not scaling up

```bash
# Check for pending pods
kubectl get pods -A --field-selector=status.phase=Pending

# Inspect autoscaler logs
kubectl -n kube-system logs -l app.kubernetes.io/name=cluster-autoscaler --tail=200

# Check status ConfigMap
kubectl -n kube-system get cm cluster-autoscaler-status -o yaml
```

Common causes:
- `autoscaler_talos_image` set to `PLACEHOLDER` — create the Talos snapshot
  and update the variable (see [docs/node-autoscaling.md](./node-autoscaling.md))
- Pool `maxSize` reached — increase `autoscaler_*_pool_max` variables
- `HCLOUD_TOKEN` expired — rotate in SOPS secrets

### Orphaned autoscaler nodes after cluster delete

`ksail cluster delete` may not remove servers created by the Cluster
Autoscaler. Clean up manually:

```bash
hcloud server list --selector cluster.autoscaler.nodeGroupLabel
# Delete each orphaned server
hcloud server delete <server-id>
```

### Autoscaler node not joining cluster

```bash
# Check if the server was created in Hetzner
hcloud server list

# If the server exists but node doesn't appear in kubectl:
# The worker machine config may be invalid or stale.
# Regenerate — see docs/node-autoscaling.md "Generate Talos worker machine config"
```

---

## Related documents

- [Node autoscaling](./node-autoscaling.md) — architecture, prerequisites, and troubleshooting
- [Velero + CNPG → R2](./velero-cnpg.md) — application/PV backups
- [Alerting](./alerting.md) — automated detection of backup failures
