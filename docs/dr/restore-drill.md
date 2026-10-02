# DR restore drill

## Dedicated Wedding credential bootstrap proof

Dispatch **Verify Wedding Backup Bootstrap** from `main` with
`confirm=verify-wedding-backup-bootstrap`. This manual workflow uses the protected
production environment and deployment lock. It creates no cluster, cloud resource
or persistent storage. It reads no production backup Secret or application data.

The proof starts an empty, non-dev OpenBao server and an ephemeral MinIO server
in a restricted namespace. It generates new credentials in memory, initializes
OpenBao, installs a token policy limited to the dedicated fixture path, and revokes
the initialization root token. It adapts the repository's dedicated PushSecret and
ExternalSecret to a namespaced fixture SecretStore and a five-second refresh.
The production mappings and one-hour refresh are validated before adaptation.
The existing External Secrets controller seeds the empty KV and projects the
credentials. A temporary network policy permits that controller to reach only
this run's OpenBao endpoint and is owned by the fixture namespace.

The program deletes only the fixture KV metadata and the controller-owned projected
Secret, using a Kubernetes UID precondition. It requires a new projected Secret UID,
an advanced PushSecret refresh time and the unchanged bootstrap source UID, without
forcing synchronization. A probe mounts only the projected Secret and writes and
reads a unique sentinel at the dedicated ObjectStore's bucket and prefix in MinIO.

Success requires `emptyOpenBaoSeedVerified`, `periodicReseedVerified`,
`projectedCredentialStorageVerified` and `cleanupVerified`, bound to the source SHA.
Both the program and a separate `always()` step remove the owned controller policy
and namespace; cleanup failure fails the proof. Never force-delete a stuck fixture
or reuse its run ID. Inspect finalizers and dispatch a fresh run after resolving it.

This proves credential delivery and repair against empty storage. The fixture
adapts the endpoint, token authentication, namespace and refresh period; it does
not prove production Kubernetes authentication, a full platform rebuild, Cloudflare
permissions, or a CNPG backup and restore against MinIO. Those remain separate
bootstrap and archive-retirement acceptance gates.

## Dedicated Wedding database proof

Dispatch **Verify Wedding Dedicated Restore** from `main` with
`confirm=verify-wedding-dedicated-restore`. It shares the production deployment
lock and first takes a fresh online backup through the active dedicated archive.
The database keeps serving guests throughout the proof.

The job creates a separate temporary namespace with a one-instance CloudNativePG
Cluster. It copies only the dedicated backup credential without printing it,
restores the exact fresh backup through the Barman Cloud plugin, and enables no
WAL archiver on the restored Cluster. A Cilium deny rule excludes the application
namespace; a live probe proves local database access works, DNS resolves the
production database, and that database does not answer from the restore pod.
The recovery configuration follows the plugin's documented
[object-store recovery](https://cloudnative-pg.io/plugin-barman-cloud/docs/usage/#restoring-a-cluster).

The comparison reads aggregate pair, guest, answer and room-booking counts plus
the newest data timestamp. Pair and guest counts must match both live snapshots.
Answer and booking counts must fall within the two observed live snapshots, and
the recovered timestamp may lag the first snapshot by at most five minutes for
recent writes. No guest names, codes, answers or notes appear in the receipt.
If writes change the comparison outside these bounds, dispatch a fresh run.

Both the program and an `always()` workflow step remove the run-owned namespace.
Deletion is fenced by its UID, and success requires the namespace and all its
backing PersistentVolumes to disappear. A cleanup failure fails the proof. Never
force-cancel the job or force-delete a stuck namespace; investigate its finalizers.

Require `dedicatedRestoreVerified`, `productionClusterStable` and
`cleanupVerified` to be true in a successful production run, bound to its source
SHA. This demonstrates dedicated archive recovery. Retiring shared access also
requires the shared-destination denial proof below and the bootstrap proof above.

## Shared-destination denial proof

Dispatch **Verify Wedding Backup Denial** from `main` with
`confirm=verify-wedding-backup-denial`. It shares the production deployment lock
and touches neither database. It shows what the shared `platform-backups`
destination enforces against the deployed dedicated credential, rather than how
that credential's token was configured.

The job first checks that the Wedding Cluster archives through
`wedding-db-dedicated`, so the credential under test is the one in use, and that
both ObjectStores name their reviewed bucket, prefix and Secret at the R2
endpoint committed in the bootstrap ConfigMap. It then starts a short-lived pod
in `wedding-app`, where both credentials and R2 egress already exist, from the
same pinned images as the catalogue mirror. The pod never prints a credential or
the endpoint host. Inside it:

1. The dedicated credential lists its own catalogue. A refusal observed later
   therefore cannot come from a broken key, endpoint or network path.
2. The shared credential lists the shared catalogue and names one object in it,
   so the refused targets exist and a mistyped bucket cannot pass.
3. With the dedicated credential, the pod lists the shared catalogue, reads that
   object, and writes a run-owned object under `wedding-backup-denial-probe/` in
   `platform-backups`. Each must be refused with `AccessDenied`, which the
   client reports either as the S3 error code or as its own
   insufficient-permissions error for the shared path.

All three accesses are attempted and reported. An access counts as refused only
when the client exits with an error, reports `AccessDenied`, and leaves no trace:
no listed entry, no local copy of the object, and no probe object when the
shared credential lists the bucket after the write (it must also be absent
before it). A trace or a successful exit is reported as broken isolation, and a
probe object that landed is removed with the shared credential before the run
fails. Any other error, such as a timeout or `NoSuchBucket`, is reported as
unproven rather than counted as a refusal.

Require the receipt with `dedicatedCatalogueReachable`,
`sharedCatalogueReferenced`, `listDenied`, `readDenied` and `writeDenied` all
true, followed by `DENIAL OBSERVED`, in a successful production run bound to its
source SHA. Run it again after every rotation of the dedicated token: the new
token must be refused exactly as the one it replaces.

## Velero namespace drill

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
