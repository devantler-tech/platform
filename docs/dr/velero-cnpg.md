# Application & volume backups — Velero + CloudNativePG → R2

The application/PV backup tier. Etcd snapshots are handled by Omni
([PR #3](./omni-etcd-backups.md)); this layer covers everything that lives
*inside* the cluster — Kubernetes objects, PVC contents, and Postgres data.

## Architecture

```
                ┌─────────────────────────────────────┐
                │  Cloudflare R2                      │
                │  bucket: devantler-platform-backups │
                │    omni-etcd/      (PR #3, Omni)    │
                │    velero/         ← this PR        │
                │    cnpg/<cluster>/ ← this PR        │
                └─────────────────────────────────────┘
                              ▲           ▲
                              │           │
                       ┌──────┴───┐  ┌────┴───────────────┐
                       │ Velero   │  │ CloudNativePG      │
                       │ Kopia    │  │ Cluster + Barman   │
                       │ uploader │  │ (per-Cluster)      │
                       └──────────┘  └────────────────────┘
```

Same R2 bucket as Omni etcd, separate prefixes (PR #3). Credentials are
SOPS-encrypted in `variables-base-secret.enc.yaml` and substituted into both
the Velero and CNPG secrets at Flux apply time.

## Velero

- Chart: `vmware-tanzu/velero` (HelmRepository at
  `https://vmware-tanzu.github.io/helm-charts`).
- Namespace: `velero`.
- File-level backups via **Kopia** (modern uploader, dedup, no per-GB Hetzner
  CSI snapshot cost). `defaultVolumesToFsBackup: true` so any new PVC is
  picked up automatically — no opt-in annotation needed.
- BackupStorageLocation: `default` → R2, prefix `velero`.
- VolumeSnapshotLocation: **none**. Cost decision (CSI snapshots are billed
  per-GB on Hetzner Cloud, file-level on R2 is flat).
- HA: 2 replicas, PDB minAvailable=1, hostname topologySpread, RollingUpdate
  maxUnavailable=0 — same posture as the rest of the operator tier (PR #2b).
- Daily schedule `daily-full` at 02:17, 14-day TTL, all namespaces except
  `kube-system` and `velero`. Long-term retention is enforced by R2 object
  lock + lifecycle (PR #3) so even a misconfigured Velero cannot delete
  history beyond the 30-day governance window.

## CloudNativePG

- The operator was already installed; this PR adds a **reusable Barman
  credentials Secret** (`cnpg-r2-credentials` in `cnpg-system`) that any
  future `Cluster` resource references via `barmanObjectStore.s3Credentials`.
- The recipe is documented inline in
  `k8s/bases/infrastructure/controllers/cloudnative-pg/r2-credentials-secret.yaml`.
- Per-cluster `Cluster` + `ScheduledBackup` resources are intentionally not
  added speculatively. When the first stateful application lands, drop a
  `Cluster` next to it that references this secret and the shared `${r2_*}`
  variables.

## Local clusters: MinIO replaces R2

Same Velero install, different backend. Local uses an in-cluster
**Bitnami MinIO** chart (single replica, ephemeral storage) so the entire
S3 code path runs end-to-end in CI. The redirection happens via Flux
variable overrides in `k8s/clusters/local/variables/`:

| Variable               | Local value                                       |
| ---------------------- | ------------------------------------------------- |
| `r2_endpoint`          | `http://minio.minio.svc.cluster.local:9000`       |
| `r2_region`            | `us-east-1` (MinIO ignores; Velero requires)      |
| `r2_bucket`            | `platform-backups`                                |
| `r2_access_key_id`     | `minio` (SOPS-encrypted)                          |
| `r2_secret_access_key` | `minio-local-development-only` (SOPS-encrypted)   |

No code changes between local and prod — only the variable values differ.
This is the whole point of the substitution layer: PR #7 (CI restore drill)
will exercise the *exact* same `velero backup` / `velero restore` calls
that an operator would run against R2 in prod.

The MinIO credentials are hard-coded local-only secrets. They are
SOPS-encrypted at rest per the platform-wide rule, but they are not
sensitive — the bucket is in-cluster and ephemeral, accessible only from
inside the local Docker cluster, and is wiped on every
`ksail cluster delete`.

## Operator commands (post-install)

```bash
# List backup storage locations
kubectl -n velero get backupstoragelocations.velero.io

# Trigger an ad-hoc backup
kubectl -n velero create -f - <<EOF
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: manual-$(date +%s)
  namespace: velero
spec:
  ttl: 720h
  defaultVolumesToFsBackup: true
EOF

# Restore (e.g. into a fresh cluster after PR #3 etcd restore)
kubectl -n velero create -f - <<EOF
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: full-restore-$(date +%s)
  namespace: velero
spec:
  backupName: <backup-name>
EOF
```

For the full DR procedure (which order to restore in, expected RTO breakdown,
etc.) see [`runbook.md`](./runbook.md) (PR #5).

## Credential rotation

Stored in `k8s/bases/variables/variables-base-secret.enc.yaml`. Rotation
flow:

```bash
# 1. Mint a new R2 token in Cloudflare; revoke the old one only after step 4.
# 2. Update both keys in-place with sops:
sops --set '["stringData"]["r2_access_key_id"] "<new-id>"' \
  k8s/bases/variables/variables-base-secret.enc.yaml
sops --set '["stringData"]["r2_secret_access_key"] "<new-secret>"' \
  k8s/bases/variables/variables-base-secret.enc.yaml
# 3. PR + merge -> Flux reconciles the new Secret -> Velero/CNPG pick it up
#    on next run (Velero re-reads the credentials secret per backup).
# 4. Revoke the old token in Cloudflare.
```

## Related

- [Omni etcd backups](./omni-etcd-backups.md) (PR #3) — control-plane layer
- [DR runbook](./runbook.md) (PR #5) — restore-from-zero procedure
- [Alerting](./alerting.md) (PR #6) — alarms on missed backups / failures
- [CI restore drill](../../.github/workflows/ci.yaml) (PR #7) — automated proof
