# Omni etcd backups → Cloudflare R2

Disaster-recovery backup of the Talos/Omni control-plane etcd state for the `dev`
and `prod` clusters. Configured **inside Omni** (not in this repo) because Omni
manages cluster lifecycle externally; only the shared R2 bucket and the
credentials live here, where they are reused by Velero ([PR #4](./velero-cnpg.md))
and CNPG.

## Why R2

- S3-compatible API → Omni's built-in S3 backup target works as-is
- **Zero egress fees** — restore from anywhere without a surprise bill
- ~$0.015/GB/mo storage; etcd snapshots are <100 MB each → ≪ $0.05/mo for the
  etcd portion. Velero + CNPG share the same bucket under separate prefixes.
- Already part of the Cloudflare account that fronts the platform

## Bucket layout

One bucket per platform install, multiple prefixes:

```
devantler-platform-backups/
├── omni-etcd/<cluster-name>/...   ← this PR
├── velero/                        ← PR #4
└── cnpg/<cluster-name>/...        ← PR #4
```

Bucket name and prefixes are exposed via the shared `variables-base` ConfigMap
in `k8s/bases/variables/variables-base-config-map.yaml`:

| Key                     | Value                                |
| ----------------------- | ------------------------------------ |
| `r2_endpoint`           | `https://<account>.r2.cloudflarestorage.com` |
| `r2_region`             | `auto`                               |
| `r2_bucket`             | `devantler-platform-backups`         |
| `r2_prefix_omni_etcd`   | `omni-etcd`                          |
| `r2_prefix_velero`      | `velero`                             |
| `r2_prefix_cnpg`        | `cnpg`                               |

## R2 bucket settings (one-time, in Cloudflare dashboard)

1. Create bucket `devantler-platform-backups` in jurisdiction `default`.
2. **Object Lock**: enabled, **Compliance** mode disabled, **Governance** mode
   default retention **30 days**. Prevents an attacker (or `rm -rf` typo) from
   deleting backups before the retention window.
3. **Versioning**: enabled. Pairs with object lock and lets restores reach back
   past an accidental overwrite.
4. **Lifecycle rules**:
   - `omni-etcd/`: transition to Infrequent Access after 14 days (still cheap on
     R2; mostly hygiene); abort incomplete multipart uploads after 1 day.
   - `velero/`, `cnpg/`: same.
5. **Server-side encryption**: SSE-S3 (AES-256), which is the R2 default and
   cannot be disabled. No additional config needed.
6. Create a **scoped API token** with permission `Object Read & Write` limited to
   this bucket. Save the access key ID and secret access key — these are the
   values that go into the SOPS-encrypted `variables-base` secret as
   `r2_access_key_id` / `r2_secret_access_key`.

## Omni-side configuration

Omni's "Control Plane Backups" feature writes etcd snapshots directly to S3-
compatible storage. There is no in-cluster cron and no extra workload — the
backup runs from Omni's control plane and only the resulting snapshot blob lands
in R2.

Configure once per cluster in the Omni UI (`Cluster → Backups`) **or** via
`omnictl` with the values below.

```bash
# Example — run for each of dev and prod, substituting the cluster name.
omnictl cluster set-backup \
  --cluster <cluster-name> \
  --type s3 \
  --endpoint "https://634e9016d402443e427865dc35457728.r2.cloudflarestorage.com" \
  --region auto \
  --bucket devantler-platform-backups \
  --prefix "omni-etcd/<cluster-name>" \
  --access-key-id     "$R2_ACCESS_KEY_ID" \
  --secret-access-key "$R2_SECRET_ACCESS_KEY" \
  --schedule "@daily" \
  --retention 14 \
  --long-term-retention 4
```

| Setting               | Value                              |
| --------------------- | ---------------------------------- |
| Schedule              | `@daily`                           |
| Daily retention       | 14 snapshots (~14 days)            |
| Weekly long-term      | 4 snapshots (~28 days)             |
| Encryption in transit | TLS to R2 endpoint                 |
| Encryption at rest    | SSE-S3 (R2 default)                |
| RPO target            | 24 h (matches plan goal)           |

## Restore (high level)

Detailed in `docs/dr/runbook.md` (PR #5). Summary:

1. Provision a fresh Talos node from the snapshot used by `hetzner/`.
2. In Omni, create a new cluster pointed at the same machine.
3. `Cluster → Backups → Restore from S3` → pick the most recent snapshot under
   `omni-etcd/<cluster-name>/`.
4. Omni rolls a new etcd member from the snapshot and rejoins the control plane.
5. Once the API server is back, Flux reconciles the rest of the platform from
   GHCR — no further manual steps for the workload tier.

## Local clusters

Skipped. Local clusters use Docker, have a single control plane, and exist only
for the lifetime of `ksail cluster create` → `ksail cluster delete`. There is
nothing to restore that isn't already in this repo.

## Related

- [DR runbook](./runbook.md) (PR #5) — full rebuild-from-zero procedure
- [Velero + CNPG backups](./velero-cnpg.md) (PR #4) — application/PV layer
- [HA primitives](../../README.md) — PDBs, topology spread, rolling updates
  (PRs #2a / #2b)
