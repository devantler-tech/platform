# Disaster recovery runbook

The single source of truth for "how do I get the platform back" — covering
single-node loss, full-cluster loss, and credential rotation. Designed so
that with this repo + the off-cluster artifacts listed below + ~30 minutes
of manual control-plane work, dev or prod can be reconstructed to a state
indistinguishable from the day before the incident.

> **RPO target:** 24 h (daily snapshots).
> **RTO target:** 4 h (mostly slack for manual Omni / Cloudflare clicks; the
> automated portion is &lt; 15 minutes in CI).

---

## Off-cluster artifacts you must keep safe

The repo + these four items are the entire seed for a rebuild. Lose all of
these simultaneously and you cannot recover.

| Artifact                                | Where it lives                       | Recovery if lost                     |
| --------------------------------------- | ------------------------------------ | ------------------------------------ |
| **SOPS Age private keys** (one per env) | Secure vault + offline backup        | Re-encrypt all `*.enc.yaml` (below)  |
| **Cloudflare R2 access keys**           | Secure vault                         | Mint new in Cloudflare; SOPS-update  |
| **Omni admin credentials**              | Secure vault + `omnictl` config      | Reset via Sidero support             |
| **Cloudflare API token**                | Secure vault                         | Mint new in Cloudflare dashboard     |

> Recommendation: store these in a shared vault accessible by at least one
> additional trusted operator, plus an offline copy in a second physical
> location.

---

## Scenario 1 — Single node loss

Expected behaviour: PDBs keep every multi-replica workload
serving traffic. Omni replaces the failed node within ~5 minutes.

**Action:** none required if PDBs and Omni autoscaling are healthy. Verify
afterwards:

```bash
kubectl get nodes
kubectl get pods -A --field-selector=status.phase!=Running
kubectl get pdb -A    # all should show ALLOWED-DISRUPTIONS=1
```

If any workload is stuck in Pending because all replicas were on the dead
node and the PDB is blocking eviction on the new one, force a rollout:

```bash
kubectl -n <ns> rollout restart deployment/<name>
```

---

## Scenario 2 — Planned rolling Talos / Kubernetes upgrade

Driven by Omni; nodes drain one at a time. PDBs hold the line.

```bash
# Pre-flight: confirm every multi-replica workload has a PDB
kubectl get pdb -A

# Pre-flight: confirm RollingUpdate strategy uses maxUnavailable: 0
kubectl get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}\t{.spec.strategy.rollingUpdate.maxUnavailable}{"\n"}{end}'
```

If anything reports `maxUnavailable` other than `0`, that workload was
either added without an HA configuration or has a chart limitation — fix before
upgrading.

---

## Scenario 3 — etcd corruption / control-plane loss

Restore from the most recent Omni snapshot (see [omni-etcd-backups.md](./omni-etcd-backups.md)).

1. **Omni dashboard** → `Cluster → Backups → Restore from S3`.
2. Pick the most recent snapshot under `omni-etcd/<cluster-name>/` (Omni
   lists them automatically; default sort is most-recent-first).
3. Confirm. Omni rolls a fresh etcd member from the snapshot and rejoins
   it to the control plane.
4. Wait for `kubectl get componentstatuses` (or `kubectl get --raw=/livez`)
   to come green.
5. Flux reconciles everything else from GHCR — no manual workload steps.

If the dashboard is unavailable, `omnictl` equivalent:

```bash
omnictl cluster restore-backup \
  --cluster <cluster-name> \
  --snapshot <snapshot-id-from-omni>
```

**RPO:** ≤ 24 h (daily schedule). **RTO:** typically &lt; 15 min for the
restore + ~5 min for Flux to converge.

---

## Scenario 4 — Full cluster rebuild from zero

The "everything is gone" path. ~30 min of manual clicks + ~15 min of Flux
reconciliation.

```bash
# 1. Provision a fresh Hetzner snapshot if needed
./hetzner/create-snapshot.sh --token "$HCLOUD_TOKEN" --media-path /path/to/talos-metal.iso

# 2. Provision the cluster nodes from that snapshot
./hetzner/create-server.sh --token "$HCLOUD_TOKEN" --server-name <name> --image-id <id>
# repeat for the desired control plane + worker count

# 3. Register the cluster in Omni (UI: Add Cluster -> point at the new machines)

# 4. Apply the Talos machine config from this repo (talos-omni/) via Omni's
#    config-patches feature. This sets the encryption-at-rest key, CNI=none,
#    etc. -- all values are committed here, no out-of-band drift.

# 5. Bootstrap Flux against this repo
ksail --config ksail.prod.yaml workload push       # packages -> GHCR
ksail --config ksail.prod.yaml workload reconcile  # Flux pulls and applies
# Flux will install Cilium, cert-manager, the rest of infrastructure, then apps

# 6. Wait for Flux to settle
flux get kustomizations -A
# Re-run if any are NotReady; expect convergence in 10-15 minutes

# 7. Restore Velero backups (apps + PVCs)
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

# 8. (If any CNPG Cluster exists) restore from R2
kubectl cnpg restore <new-cluster-name> \
  --backup <backup-name> \
  --target-time '<RFC3339-timestamp-or-omit-for-latest>'
```

If this is the **first time** restoring after losing the SOPS keys, replace
step 5 with the rotation flow in Scenario 6 first.

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
#    platform-backups bucket only). DO NOT revoke the old one
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

# 6. Update the in-Omni R2 credentials (Omni etcd backups, see omni-etcd-backups.md).
omnictl cluster set-backup --cluster <c> --access-key-id <new> --secret-access-key <new>
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

## Related documents

- [Omni etcd backups](./omni-etcd-backups.md) — control-plane backups
- [Velero + CNPG → R2](./velero-cnpg.md) — application/PV backups
- [Alerting](./alerting.md) — automated detection of backup failures
