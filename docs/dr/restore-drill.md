# DR restore drill (CI)

`.github/workflows/ci.yaml` runs a `restore-drill` job on every PR that
touches `k8s/**` or the cluster configs. The job validates the full
backup → cluster-loss → restore round trip end-to-end on a fresh local
Talos+Docker cluster, so an etcd loss / cluster rebuild scenario is
regression-tested **before** changes reach `dev` or `prod`.

## What it does

1. `ksail cluster create` (round 1) and reconcile.
2. Wait for **Velero** + **MinIO** (the local R2 stand-in from PR #4) to
   be ready and `BackupStorageLocation/default` `Available`.
3. Create a marker `Namespace`/`ConfigMap` carrying the GitHub
   `run-id` and `sha` (so we can prove identity later).
4. `velero backup create` against the marker namespace, `--wait` for
   `Completed`.
5. **Destroy the cluster**: `ksail cluster delete`.
6. `ksail cluster create` (round 2 — fresh) and reconcile.
7. Wait for Velero/MinIO again, then poll until Velero rediscovers the
   backup CR from object storage.
8. Assert the marker namespace does **not** pre-exist on the new
   cluster.
9. `velero restore create --from-backup ... --wait` for `Completed`.
10. Assert the marker `ConfigMap` is back and `data.run-id` matches the
    current `GITHUB_RUN_ID`.
11. Tear down the cluster (`if: always()`).

## Wall-clock budget

`timeout-minutes: 240` on the job — matches the **4 h RTO** documented
in [`runbook.md`](./runbook.md). In practice the drill runs in ~15 min.
The 4 h ceiling is the operator promise for the manual prod path; CI
keeps that promise honest by failing fast if the local round trip
explodes.

## What this catches

- A regression in the Velero install (chart version bump, RBAC drift,
  missing AWS plugin).
- A regression in the MinIO install or its credential wiring (Velero
  `BackupStorageLocation` going `Unavailable`).
- Backup format incompatibility introduced by a Velero version bump
  (round-2 cluster runs the same chart and has to read round-1's data).
- A reconciliation regression that makes `velero` or `minio` never
  become Ready inside the 10-minute rollout window.

## What this does **not** catch

- Cloudflare R2 specifics (CRC checksum quirk, bucket policy, IAM key
  rotation). That's `dev`/`prod`-only and needs a periodic manual drill
  documented in [`runbook.md`](./runbook.md#scenario-3-restore-an-app-namespace-from-velero).
- Omni etcd backup/restore — not exercised here because there is no
  Omni in CI. Drill manually per [`omni-etcd-backups.md`](./omni-etcd-backups.md).
- CNPG PITR — covered by the CNPG operator's own e2e; we only verify
  that the `ScheduledBackup` reconciles. A future PR can extend the
  drill to write a row, backup, destroy, restore, read row.

## Why no etcd encryption verification step

Talos `cluster.secretboxEncryptionSecret` is verified at install time by
Talos itself (it refuses to bootstrap with a malformed key). A separate
"read raw etcd, grep for plaintext" step adds CI complexity for a
property that is structurally enforced. If a future regression suggests
the encryption is silently disabled, add a `talosctl etcd snapshot` +
`etcdctl get --print-value-only ... | grep -aq SECRET && exit 1` step.

## Local manual run

```bash
# Round 1
ksail cluster create
ksail workload push && ksail workload reconcile
kubectl create ns dr-drill
kubectl -n dr-drill create configmap dr-marker --from-literal=t=$(date -u +%FT%TZ)
velero backup create dr-drill --include-namespaces dr-drill --wait

# Round 2
ksail cluster delete
ksail cluster create
ksail workload push && ksail workload reconcile
velero backup get   # should list dr-drill
velero restore create dr-drill-restore --from-backup dr-drill --wait
kubectl -n dr-drill get configmap dr-marker -o yaml
```
