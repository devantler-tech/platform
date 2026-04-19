# Omni etcd backups → Cloudflare R2

Disaster-recovery backup of the Talos/Omni control-plane etcd state for the `dev`
and `prod` clusters. Configured **inside Omni** (not in this repo) because Omni
manages cluster lifecycle externally; only the shared R2 bucket and the
credentials live here, where they are reused by Velero (see [velero-cnpg.md](./velero-cnpg.md))
and CNPG.

## Why R2

- S3-compatible API → Omni's built-in S3 backup target works as-is
- **Zero egress fees** — restore from anywhere without a surprise bill
- ~$0.015/GB/mo storage; etcd snapshots are <100 MB each → ≪ $0.05/mo for the
  etcd portion. Velero + CNPG share the same bucket under separate prefixes.
- Already part of the Cloudflare account that fronts the platform

## Single shared bucket (Omni constraint)

Omni supports exactly **one** backup-storage configuration for the whole Omni
instance — there is no per-cluster override. All Omni-managed clusters share the
same S3 target and Omni itself namespaces snapshots by cluster name underneath
that target.

To keep Velero and CNPG from colliding in that same bucket, they run under
per-env prefixes:

```
<your-bucket>/
├── <omni-cluster-name>/...   ← Omni etcd snapshots, bucket root (managed by Omni)
├── velero/dev/               ← Velero, dev cluster
├── velero/prod/              ← Velero, prod cluster
├── cnpg/dev/                 ← CNPG WAL/base backups, dev cluster
└── cnpg/prod/                ← same, prod cluster
```

Bucket and endpoint come from the shared `variables-base` ConfigMap in
`k8s/bases/variables/variables-base-config-map.yaml`; per-env prefixes come from
each `k8s/clusters/<env>/variables/variables-cluster-config-map.yaml`:

| Key                     | Where        | Value                                           |
| ----------------------- | ------------ | ----------------------------------------------- |
| `r2_endpoint`           | base         | `https://<account>.r2.cloudflarestorage.com`    |
| `r2_region`             | base         | `auto`                                          |
| `r2_bucket`             | base         | `<your-bucket>` (e.g. `<your-org>-platform-backups`) |
| `r2_prefix_velero`      | per-cluster  | `velero/dev` or `velero/prod` (local: `velero`) |
| `r2_prefix_cnpg`        | per-cluster  | `cnpg/dev` or `cnpg/prod` (local: `cnpg`)       |

## R2 bucket settings (one-time, in Cloudflare dashboard)

1. Create a bucket (e.g. `<your-org>-platform-backups`) in jurisdiction `default`.
2. **Object Lock**: enabled, **Compliance** mode disabled, **Governance** mode
   default retention **30 days**. Prevents an attacker (or `rm -rf` typo) from
   deleting backups before the retention window.
3. **Versioning**: enabled. Pairs with object lock and lets restores reach back
   past an accidental overwrite.
4. **Lifecycle rules** (all under `<your-bucket>/`):
   - `velero/` and `cnpg/`: transition to Infrequent Access after 14 days;
     abort incomplete multipart uploads after 1 day.
   - Bucket root (Omni snapshots): same.
5. **Server-side encryption**: SSE-S3 (AES-256), which is the R2 default and
   cannot be disabled. No additional config needed.
6. Create a **scoped S3 API token** with permission `Object Read & Write`
   limited to this bucket. Save the Access Key ID and Secret Access Key — these
   are the values that go into the SOPS-encrypted cluster secrets as
   `r2_access_key_id` / `r2_secret_access_key` (one copy in `dev`, one in
   `prod`; the key can be the same or rotated independently — same bucket).

## Omni-side configuration (one-time, in the Omni UI)

Omni's **Settings → Backup Storage** page takes a single S3 config that applies
to every cluster Omni manages. Open it and paste:

| Field             | Value                                                               |
| ----------------- | ------------------------------------------------------------------- |
| Endpoint          | `https://<account-id>.r2.cloudflarestorage.com`                    |
| Bucket            | `<your-bucket>`                                                     |
| Region            | `auto` (any value works; R2 ignores it)                             |
| Access Key ID     | the R2 S3 token ID from step 6 above                                |
| Secret Access Key | the R2 S3 token secret from step 6 above                            |
| Session Token     | *(leave blank)*                                                     |

Save. Then for each cluster (dev, prod) enable backups under
**Cluster → Settings → etcd Backups** with:

| Setting               | Value                              |
| --------------------- | ---------------------------------- |
| Schedule              | `@daily`                           |
| Daily retention       | 14 snapshots (~14 days)            |
| Weekly long-term      | 4 snapshots (~28 days)             |
| Encryption in transit | TLS to R2 endpoint                 |
| Encryption at rest    | SSE-S3 (R2 default)                |
| RPO target            | 24 h (matches plan goal)           |

Omni writes snapshots at `<bucket>/<cluster-name>/<snapshot-id>` — the
per-cluster folder is added by Omni, not by us. That's why our own Velero/CNPG
prefixes don't overlap.

## Restore (high level)

Detailed in [`runbook.md`](./runbook.md). Summary:

1. Provision a fresh Talos node from the snapshot used by `hetzner/`.
2. In Omni, create a new cluster pointed at the same machine.
3. `Cluster → Backups → Restore from S3` → pick the most recent snapshot under
   the target cluster's folder in the R2 bucket.
4. Omni rolls a new etcd member from the snapshot and rejoins the control plane.
5. Once the API server is back, Flux reconciles the rest of the platform from
   GHCR — no further manual steps for the workload tier.

## Local clusters

Skipped. Local clusters use Docker, have a single control plane, and exist only
for the lifetime of `ksail cluster create` → `ksail cluster delete`. There is
nothing to restore that isn't already in this repo.

## Related

- [DR runbook](./runbook.md) — full rebuild-from-zero procedure
- [Velero + CNPG backups](./velero-cnpg.md) — application/PV layer
- [HA primitives](../../README.md) — cluster environments and topology
