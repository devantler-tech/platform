# Application & volume backups — Velero + CloudNativePG → R2

The application/PV backup tier. With Omni retired, etcd is a cattle
resource recreated by `ksail cluster create` on demand (see
[runbook.md](./runbook.md) scenario 4). This layer covers everything that
needs to survive a full cluster rebuild — Kubernetes objects, PVC
contents, and Postgres data.

## Architecture

```
                ┌─────────────────────────────────────┐
                │  Cloudflare R2                      │
                │  bucket: <your-bucket>              │
                │    velero/<env>/   (this layer)     │
                │    cnpg/<env>/     (this layer)     │
                └─────────────────────────────────────┘
                              ▲           ▲
                              │           │
                       ┌──────┴───┐  ┌────┴───────────────┐
                       │ Velero   │  │ CloudNativePG      │
                       │ Kopia    │  │ Cluster + Barman   │
                       │ uploader │  │ (per-Cluster)      │
                       └──────────┘  └────────────────────┘
```

The shared `platform-backups` credential is SOPS-encrypted per environment in
`k8s/clusters/<env>/bootstrap/secret.enc.yaml`, seeded into OpenBao at
`infrastructure/backup/r2` by the `seed-r2-credentials` PushSecret, and
materialised into the Velero and participating CNPG namespaces by
ExternalSecrets. Wedding's tenant-isolated destination uses a separate
`wedding-db-backups` bucket and credential, with
`secret-wedding-db-backup-r2.enc.yaml`, the `seed-wedding-db-backup-r2`
PushSecret, and the dedicated `apps/wedding-app/backup/r2` OpenBao path. The two
credential branches rotate independently. The platform's production tenant
patch selects `wedding-db-dedicated` for the live Wedding Cluster and preserves
its `wedding-db-20260909` archive identity. That dedicated credential is the
only backup credential in the `wedding-app` namespace: the shared `wedding-db`
ObjectStore and its projection of the shared credential were retired (#3253)
once the predecessor catalogue had been mirrored into the dedicated bucket, a
restore from the dedicated bucket alone had succeeded, and the shared
destination had been observed refusing the dedicated credential (protected
`main` run 37234932425; see restore-drill.md). The shared credential can read
every shared consumer's backups, so it must not return to a tenant namespace.
The `Mirror Wedding Backup Catalogue` and `Verify Wedding Backup Denial`
workflows both need the shared credential inside `wedding-app`, so neither can
run any more; #4482 re-homes the denial proof and removes the finished cutover
tooling, and #4481 removes the predecessor catalogue that still sits in the
shared bucket. That copy is no longer written to or pruned, and every holder of
the shared credential can read it. Before it is removed, dispatch `Verify
Wedding Shared Backup Coverage` on `main` with
`confirm=verify-wedding-shared-backup-coverage`. It only reads: a pod beside the
shared credential in `umami` and one beside the dedicated credential in
`wedding-app` each list their side, and the verdict is `COVERED` only when every
object in the shared copy is in the dedicated bucket with matching content, or
was provably removed there by the 30-day retention. Remove the shared copy only
after a `COVERED` run, and run the proof again afterwards: it then reports
`EMPTY`. `Verify Wedding Backup Cutover`
is a main-only, protected production dispatch: confirm
`verify-wedding-backup-cutover` to request a fresh online primary backup. It
refuses a shared archive, unhealthy database, changed database identity, or
incomplete backup receipt. Record its backup ID and WAL segment, then confirm
that the dedicated catalogue contains that completed backup and advancing WAL.
Backup completion alone does not prove an isolated restore.

## Velero

- Chart: `vmware-tanzu/velero` (HelmRepository at
  `https://vmware-tanzu.github.io/helm-charts`), Velero 1.18.
- Namespace: `velero`.
- BackupStorageLocation: `default` → R2, prefix `velero/<env>`.
- Daily schedule `daily-full` at 02:17, 14-day TTL, all namespaces except
  `kube-system` and `velero`. Long-term retention is enforced by R2 object
  lock + lifecycle rules (configured on the bucket) so even a misconfigured
  Velero cannot delete history beyond the 30-day governance window.
- HA: `velero_replicas` (prod = 1, leader-elected), hostname topologySpread,
  `Recreate` strategy — **no PDB** (a single leader-elected pod with a PDB
  would deadlock rolling upgrades). The node-agent runs as a DaemonSet on
  every node (required for both Kopia FSB and the CSI data mover).

### Backup method: per-StorageClass (prod) vs uniform FSB (local)

Backups always land in R2 as a Kopia repository, so they are portable for
cross-provider/cross-distribution restore regardless of how the volume was
captured. *How* each volume is captured depends on its storage backend:

| Storage backend                    | Method                               | Why                                                                                  |
| ---------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------ |
| Longhorn (`longhorn` SC)           | CSI snapshot → Kopia data mover → R2  | Crash-consistent; also backs up PVCs of scaled-to-zero apps (no running pod needed).  |
| hcloud + `backup.platform.devantler.tech/volume-data=independently-mirrored` (`openbao/vault-snapshots` only) | Skipped by Velero; the OpenBao CronJob mirrors raft snapshots directly to R2 | The PVC is unmounted during Velero's window, so FSB cannot read it, and Hetzner block storage has no CSI snapshot support. |
| other hcloud claims (OpenBao data/audit and Coroot telemetry) | File-system backup (Kopia) → R2 | hcloud-csi has no snapshot support; mounted controller-created claims use the fail-safe FSB path. The copy is file-level and taken while ClickHouse runs, so Coroot telemetry restores best-effort only ([#3673](https://github.com/devantler-tech/platform/issues/3673)). |
| anything else / new PVCs           | File-system backup (Kopia) → R2       | Fail-safe default.                                                                    |

The routing is declarative through storage attributes, not per-pod annotations:

- `defaultVolumesToFsBackup: true` everywhere is the **fail-safe default** — any
  volume not otherwise routed is Kopia-FSB'd, so nothing is ever silently
  skipped.
- **prod only:** a Velero **Volume Policy** ConfigMap (`velero-volume-policies`,
  referenced by the schedule's `spec.resourcePolicy`) routes `storageClass:
  [longhorn]` to `snapshot`. It skips an hcloud claim only when that PVC also
  carries `backup.platform.devantler.tech/volume-data=independently-mirrored`.
  Only `openbao/vault-snapshots` carries the marker; its CronJob mirrors every
  raft snapshot directly to R2, and no pod mounts it during Velero's window.
  OpenBao data/audit storage, Coroot telemetry storage, future hcloud claims,
  and unknown storage classes retain the fail-safe FSB fallback.
  `snapshotMoveData: true` makes the Longhorn data mover upload CSI snapshots to
  R2 (so they are not tied to Longhorn at restore time).
- **local/CI:** no Volume Policy and no CSI (the docker `local-path` provider
  cannot snapshot) → every mounted volume uses FSB, so the CI restore drill
  still regression-tests the production fallback path.

### CSI snapshot prerequisites (prod/hetzner)

CSI snapshots need cluster-wide plumbing that the hetzner overlay adds:

- **snapshot-controller + the `snapshot.storage.k8s.io` CRDs** — the piraeus
  `snapshot-controller` chart (appVersion = kubernetes-csi external-snapshotter
  v8.5.0, the version Longhorn 1.11 targets), in `kube-system`. The conversion
  webhook is disabled (only the v1 API is used).
- **Longhorn CSI snapshotter sidecar** — enabled via
  `longhorn_csi_snapshotter_replicas: "2"`: a leader-elected active plus a
  warm standby, so snapshot orchestration survives one storage-node failure.
  Do not lower it back to `"1"` during recovery. Longhorn `dependsOn`
  snapshot-controller so the CRDs exist before the sidecar starts.
- **`VolumeSnapshotClass` `longhorn-snapshot-vsc`** (`type: snap`, labelled
  `velero.io/csi-volumesnapshot-class`) — a plain in-cluster Longhorn snapshot
  (NOT a billed cloud snapshot, and NOT Longhorn's own `bak` backup target)
  which the data mover reads and then deletes. It lives in the `infrastructure`
  Flux layer so the CRDs (installed in `infrastructure-controllers`) are
  established first.
- Velero `features: EnableCSI` (the CSI plugin is built into Velero 1.18 core,
  so no extra plugin beyond `velero-plugin-for-aws` for the R2 BSL).

> **Transient disk during the backup window.** For each Longhorn PVC the data
> mover provisions a short-lived PVC from the snapshot (on `longhorn`, so
> ×`longhorn_replica_count`) and a node-agent pod copies it to R2, then deletes
> both. On the space-constrained cx23 workers this transiently consumes up to
> the source volume's size × replica count per PVC during the 02:17 run — fine
> for the current set (umami 5Gi, actual-budget 10Gi, headlamp 256Mi) but worth
> watching if larger Longhorn volumes are added.

## CloudNativePG

- CNPG backup credentials are projected **per-namespace** from OpenBao: each
  CNPG `Cluster` gets an ExternalSecret next to it because the Barman plugin's
  `ObjectStore` can only reference a Secret in the Cluster's own namespace.
  Shared consumers read `infrastructure/backup/r2`. Wedding's active
  `wedding-db-dedicated` ObjectStore reads `apps/wedding-app/backup/r2` and
  points at `wedding-db-backups`. It is the namespace's only backup store: the
  earlier `wedding-db` ObjectStore on the shared path is retired (#3253).
  The earlier reusable `cnpg-r2-credentials` Secret in `cnpg-system` was
  removed because no `Cluster` could reference it across namespaces.
- Live example: `umami-db` — `k8s/bases/apps/umami/external-secret-db-backup.yaml`
  projects `umami-db-backup-r2`, which the `Cluster` in
  `k8s/bases/apps/umami/cluster.yaml` references (with a
  `ScheduledBackup` in `scheduled-backup.yaml`).
- When the next CNPG-backed app lands, choose its isolation boundary explicitly.
  A shared platform consumer can copy the Umami projection and use a distinct
  `s3://${r2_bucket}/cnpg/<app>` prefix. A tenant-isolated database needs the
  complete Wedding pattern: a dedicated bucket and Object Read & Write identity,
  encrypted bootstrap Secret, PushSecret, dedicated OpenBao path, namespace
  ExternalSecret, and hosted manifest plus live backup and restore proof.
- Give each CNPG Barman plugin an explicit `parameters.serverName` that
  identifies the current logical Cluster incarnation. The plugin admission
  webhook forbids this field on the `ObjectStore`; it belongs on the Cluster's
  `spec.plugins` entry. Barman requires an empty archive for a new PostgreSQL
  system ID, so reusing an implicit Cluster-name server after a restore or
  replacement makes continuous WAL archiving fail with `Expected empty
  archive`, even when base backups still report success. Keep the ObjectStore
  destination path stable so retained recovery data remains available, and
  advance the server name in the same reviewed recovery change that creates the
  new Cluster. Do not change it for an ordinary rollout of a healthy Cluster.

## Local clusters: disposable S3 storage replaces R2

The optional local backup components use a pinned **SeaweedFS** Deployment
with one replica and ephemeral storage. The existing `minio` resource names
and S3 endpoint remain compatible with Velero and CNPG configuration. Enable
the components explicitly for a manual restore drill; ordinary CI validates
manifests without creating a cluster. Flux variable overrides in
`k8s/clusters/local/bootstrap/` select the local backend:

| Variable               | Local value                                       |
| ---------------------- | ------------------------------------------------- |
| `r2_endpoint`          | `http://minio.minio.svc.cluster.local:9000`       |
| `r2_region`            | `us-east-1`                                       |
| `r2_bucket`            | `platform-backups`                                |
| `r2_access_key_id`     | `minio` (SOPS-encrypted)                          |
| `r2_secret_access_key` | `minio-local-development-only` (SOPS-encrypted)   |

Velero uses the same backup and restore commands with these endpoint overrides.
The server starts as UID/GID 65532 with a read-only root filesystem and no
capabilities or ServiceAccount token. Its launcher requires valid mounted
credential files and creates one private S3 identity; missing, empty or
malformed credentials stop startup. Internal HTTP APIs bind loopback, and the
network policy permits only S3 HTTP ingress from Pod peers, blocking their access
to the additional management gRPC listener. This does not claim isolation from
trusted node processes. See [restore-drill.md](./restore-drill.md) for the full manual drill.
Fixture S3 operations and the protected Wedding bootstrap verification do not
establish a real Velero/CNPG restore or production R2 compatibility.

Wedding is deliberately absent from the Docker apps overlay because its tenant
deployment depends on production-only GHCR, CNPG, and Longhorn resources. The
local cluster therefore prunes Wedding's PushSecret and carries neither a
dedicated Wedding bootstrap credential nor an unused `wedding-db-backups`
bucket. Dedicated ObjectStore acceptance uses the hosted manifest checks and a
live production backup, WAL archive, and isolated restore instead.

The S3 credentials are hard-coded local-only secrets. They are
SOPS-encrypted at rest per the platform-wide rule, but they are not
sensitive — the bucket is in-cluster and ephemeral, accessible only from
inside the local Docker cluster, and is wiped on every
`ksail cluster delete`.

## Operator commands (post-install)

```bash
# List backup storage locations
kubectl -n velero get backupstoragelocations.velero.io

# Trigger an ad-hoc backup. NOTE: unlike the daily-full schedule, a manual
# Backup does NOT inherit the volume policy. Add resourcePolicy (prod) if you
# want Longhorn PVCs captured via CSI snapshot; without it every volume is
# FSB'd (safe, just not crash-consistent for Longhorn). Omit it on local.
kubectl -n velero create -f - <<EOF
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: manual-$(date +%s)
  namespace: velero
spec:
  ttl: 720h
  defaultVolumesToFsBackup: true
  resourcePolicy:
    kind: configmap
    name: velero-volume-policies
EOF

# Restore (e.g. into a fresh cluster after etcd restore)
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
etc.) see [`runbook.md`](./runbook.md).

## Credential rotation

The shared `platform-backups` credential and Wedding's `wedding-db-backups`
credential rotate independently. The shared credential lives in
`k8s/clusters/<env>/bootstrap/secret.enc.yaml`; the Wedding credential lives in
`k8s/clusters/prod/bootstrap/secret-wedding-db-backup-r2.enc.yaml`. Rotating
one does not refresh the other.

Use the repository's non-printing `sops set --value-stdin` procedure in
runbook.md Scenario 7. It reads each value without terminal echo and passes it
to SOPS on stdin, keeping plaintext out of the editor, shell history, process
arguments, and command output. For the Wedding file, recompute the SHA-256
receipts for that encrypted file and the prod bootstrap `kustomization.yaml`, then replace
`WEDDING_BACKUP_CIPHER_SHA256` and `WEDDING_BACKUP_BOOTSTRAP_SHA256` in
`.github/actions/deploy-prod/action.yml`. These hashes cover encrypted public
bytes and resource membership; calculating them does not decrypt or print the
credential.

After the rotation merges, let Flux reconcile. For Wedding, run `cd.yaml`
manually with `verify-wedding-backup-staging=true`; the verifier checks the
bootstrap Secret, OpenBao projection, the dedicated ObjectStore, the dedicated
live Cluster reference, and live source stability without printing the
credential. It also refuses unless the retired shared ObjectStore, its
ExternalSecret and its Secret are observed absent from `wedding-app`. The
owned Secret is removed just after its ExternalSecret, so a refusal on the first
run after the retiring deploy calls for a fresh dispatch, not a re-run. Use
`Verify Wedding Backup Cutover` to take and verify a fresh
backup after a Wedding credential rotation. The catalogue and WAL checks must
also pass before retiring the replaced dedicated token. `Verify Wedding Backup
Denial` cannot prove that `platform-backups` refuses the new token until #4482
re-homes it, because Wedding's shared access is retired; until then the new
token's scope is confirmed when it is minted and its denial is unproven. The
shared platform identity still serves other databases and
Velero. See runbook.md Scenario 7.

## Related

- [DR runbook](./runbook.md) — restore-from-zero procedure
- [Alerting](./alerting.md) — alarms on missed backups / failures
- [CI restore drill](./restore-drill.md) — automated proof
