# DR restore drill

> **No longer runs in CI.** CI no longer boots a cluster — the local Docker
> cluster is a thin manual test-bed, not a prod stand-in — so this drill is now
> a **manual** procedure. Run it locally (after opting Velero + MinIO into the
> local overlay), and as the periodic prod drill in
> [`runbook.md`](./runbook.md). The copy-paste version is in [Local manual
> run](#local-manual-run) below; the steps here explain what it exercises.

The drill validates the full backup → data-loss → restore cycle end-to-end
against **MinIO** (the local R2 stand-in), so the Velero code path can be
checked **before** changes reach `prod`.

> **Opt-in prerequisite (local):** Velero and MinIO are not in the thin core
> set. Enable them first: in
> `k8s/providers/docker/infrastructure/controllers/kustomization.yaml`
> uncomment `velero/` + `minio/`, and in `…/infrastructure/kustomization.yaml`
> uncomment `vault-backup/` and drop the `velero-r2-credentials` delete-patch.
> Then `ksail workload push && ksail workload reconcile`.

## What it does

1. Bring up a local cluster with Velero + MinIO opted in (above).
2. Wait for `BackupStorageLocation/default` to report `Available`
   (Velero validates against **MinIO**, the local R2 stand-in).
3. Create a marker `Namespace`/`ConfigMap` carrying the GitHub
   `run-id` and `sha` (so identity can be proved later).
4. Create a `Backup` CR scoped to the marker namespace and wait for
   phase `Completed` (failing fast on `Failed`/`PartiallyFailed`).
5. **Simulate data loss**: delete the marker namespace (`kubectl delete
   namespace`).
6. Assert the marker namespace does **not** exist after deletion.
7. Create a `Restore` CR from the backup and wait for `Completed`.
8. Assert the marker `ConfigMap` is back and its `data` matches what you
   wrote before the simulated loss.
9. Tear the cluster down (`ksail cluster delete`) when finished.

> The Velero CRs can be created with `kubectl` rather than the `velero` CLI so
> the drill needs no extra tool install and can never drift from the deployed
> Velero version (the in-CI variant used this; the manual run below uses the
> `velero` CLI for brevity).

> **Why namespace deletion instead of full cluster rebuild?** MinIO runs
> in-cluster with ephemeral storage, so destroying the cluster would also
> destroy the backup target. Namespace deletion simulates data loss while
> keeping MinIO (and thus the backup data) intact, exercising the same
> Velero → S3 → Velero code path end-to-end.

## Wall-clock budget

The drill itself is bounded: 10 min for the `BackupStorageLocation` to
go `Available`, then 5 min each for the backup and the restore to reach
`Completed` (terminal failure phases abort immediately). In practice the
whole sequence takes ~2-3 minutes once the cluster is up. The **4 h
RTO** in [`runbook.md`](./runbook.md) is the operator promise for the
manual prod path; running this drill periodically keeps that promise honest by
surfacing a broken local round trip early.

## What this catches

- A regression in the Velero install (chart version bump, RBAC drift,
  missing AWS plugin).
- A regression in the MinIO install or its credential wiring (Velero
  `BackupStorageLocation` going `Unavailable`).
- Backup format incompatibility introduced by a Velero version bump.
- A reconciliation regression that makes `velero` or `minio` never
  become Ready inside the 10-minute rollout window.

## What this does **not** catch

- Cloudflare R2 specifics (CRC checksum quirk, bucket policy, IAM key
  rotation). That's `prod`-only and needs a periodic manual drill
  documented in [`runbook.md`](./runbook.md#scenario-3-restore-an-app-namespace-from-velero).
- Omni etcd backup/restore — no longer part of the platform; etcd is a
  cattle resource recreated by `ksail cluster create`. Full-cluster
  recovery is covered by [`runbook.md`](./runbook.md#scenario-4-full-cluster-rebuild-from-zero).
- CNPG PITR — covered by the CNPG operator's own e2e; we only verify
  that the `ScheduledBackup` reconciles. A future extension could write
  a row, backup, delete, restore, and read the row back.
- Full cluster rebuild with R2 — in prod the backup survives cluster
  destruction (it lives in R2). That scenario is covered by the manual
  procedure in the runbook.

## Why no etcd encryption verification step

Talos `cluster.secretboxEncryptionSecret` is verified at install time by
Talos itself (it refuses to bootstrap with a malformed key). A separate
"read raw etcd, grep for plaintext" step adds CI complexity for a
property that is structurally enforced. If a future regression suggests
the encryption is silently disabled, add a `talosctl etcd snapshot` +
`etcdctl get --print-value-only ... | grep -aq SECRET && exit 1` step.

## Local manual run

```bash
# First opt Velero + MinIO into the local overlay (see the prerequisite above).
ksail cluster create
ksail workload push && ksail workload reconcile

# Create marker
kubectl create ns dr-drill
kubectl -n dr-drill create configmap dr-marker --from-literal=t=$(date -u +%FT%TZ)

# Backup
velero backup create dr-drill --include-namespaces dr-drill --wait

# Simulate data loss
kubectl delete namespace dr-drill --wait=true --timeout=2m
until ! kubectl get namespace dr-drill >/dev/null 2>&1; do sleep 2; done

# Restore
velero restore create dr-drill-restore --from-backup dr-drill --wait
kubectl -n dr-drill get configmap dr-marker -o yaml
```
