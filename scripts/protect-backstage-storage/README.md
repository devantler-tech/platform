# Protect Backstage storage

This command performs only the retained-storage prerequisite for stateless portal
retirement. It changes the reclaim policy of the Backstage database's exactly
bound Longhorn volumes from `Delete` to `Retain`. An already retained volume is a
no-op. It does not retire the portal, alter database redundancy, remove claims,
delete storage, change credentials or add prune annotations.

Run it only through the manually confirmed
[`protect-backstage-storage.yaml`](../../.github/workflows/protect-backstage-storage.yaml)
workflow after its reviewed source has merged and deployed. The invocation must
be the first attempt on current `main`, with the workflow's exact repository,
event and path. The requested artifact revision must be live and accepted by a
successful normal production CI/CD deployment on that same source commit.
The immutable artifact is also cryptographically verified against its registry-held
SLSA provenance. Its signed certificate must name that source commit, the exact
accepted CI/CD workflow and ref, and the accepted run and attempt. A successful
run beside an unrelated live digest is not sufficient; user-controlled predicate
fields cannot supply the missing identity.
GitHub API reads use the short-lived Actions job token with read-only permissions.
OCI reads use the established production pull credential: the shared helper
decrypts only its Docker-config scalar from Git/SOPS after the dispatch, source
and offline-control checks. It validates that config in a private runner-temporary
directory and removes the credential files even when verification fails. No new
repository package grant or publication credential is needed. Tokens are not put
in command arguments. Missing registry authentication stops verification rather
than falling back to API-held attestations, which this publication path does not
create.

Before changing anything, the command reads complete native Kubernetes lists and
joins every retained claim to its volume by namespace, name, UID and storage class.
The configured database instances must all have owned, Ready pods mounting their
own claims. An existing namespace retirement journal, incomplete census, changed
source, unsettled Flux layer or ambiguous identity stops the operation.

Two separated samples precede writes. Each write takes another complete fresh
sample and uses a JSON Patch that tests the volume's UID, resource version and
entire existing specification before replacing only its reclaim policy. A stale
or failed patch is never retried. Two separated final samples verify physical
retention and preserved identities/specifications. Completed protections are never
reversed when a later step fails.

The native API offers no transaction across the database, namespace, claims and
volumes. These checks bound the operation; they do not claim an atomic cross-object
snapshot. Stateless retirement remains a separate workflow and still requires
its captured-identity admission fence, installed only after storage protection.

All reads and patch responses remain bounded in process memory. The command reads
no Secrets and emits only a compact count receipt; it never uploads cluster
snapshots, raw patch failures or storage identities.

## Diagnose a hold without changing storage

After merge and normal deployment, select the workflow's explicit `check`
operation and confirm `CHECK BACKSTAGE STORAGE ONLY`. The normal `protect`
operation still requires `PROTECT BACKSTAGE STORAGE ONLY`; the two confirmations
cannot be exchanged. Both modes retain the same protected main-only invocation,
accepted deployment, signed provenance, complete storage census and separated
sample checks.

Check mode submits no patch. A successful `CHECKED` receipt reports only counts,
including how many bound volumes still have `Delete` policy; it is not a
`PROTECTED` result, a write authorization or permission to retry a consumed run.
Failure emits `HOLD` with a fixed `PORTAL_STORAGE_REASON` guard category, never a
raw transport error. Diagnose that guard and independently read back existing
storage before preparing a new approved write-capable attempt.

## A released volume stops the census

Replacing a database instance can leave its old `Retain` volume in `Released`
with a reference to a claim UID that no longer exists. This is still retained
data, not an empty volume. The storage-protection command stops on that reverse-census
mismatch; do not skip the volume, remove protection, clear its claim reference,
or retry the consumed operation to make the check pass.

1. Use read-only Kubernetes and Longhorn views to identify the exact old volume,
   its claim UID, backing-volume identity, retention policy and attachments. Keep
   this inventory private. Prove the old claim UID is absent from a complete claim
   census; a replacement with the same name is a different identity.
2. Verify the remaining database instances, replication and backups are healthy.
   Prove no running workload or attachment still uses the old backing volume.
   If any evidence is incomplete or the volume is attached, remain on HOLD.
3. Prepare a separate, reviewed recovery operation to preserve the orphan's own
   data: take and verify a recoverable Longhorn backup, or recover it through a
   dedicated recovery claim and verify its backup. A backup of the current
   primary alone is not proof that this old volume is recoverable.
4. Record the retention decision and obtain the operation-specific approval.
   A protected maintenance workflow may then rehome the preserved volume to the
   reviewed recovery claim, or retire only its Kubernetes PV identity while
   explicitly preserving the Longhorn backing volume and verified backup.
   Deleting backing data is a different destructive operation, never implied by
   clearing this hold. The general retention migration remains tracked in #4441.
5. If a retirement journal already exists, keep it and inspect that operation;
   do not clear it or bypass its full-specification fence. Any needed journal or
   volume maintenance requires a separately reviewed, authorized recovery.
6. Read back the retained backing data, recovery/backup evidence and complete
   claim/volume census. Continue only with a new approved protected attempt whose
   source and deployment are current; never rerun a consumed attempt.

This workflow intentionally implements none of that orphan recovery or deletion.
Until the separate operation is delivered and verified, HOLD is the safe result.

## Offline checks

```sh
go test -race ./scripts/protect-backstage-storage ./internal/portalstorage
go vet ./scripts/protect-backstage-storage ./internal/portalstorage
```

With `kubectl` installed, an anonymous loopback API exercises the real patch
transport and rejects a replaced volume identity. Set
`PORTAL_STORAGE_REQUIRE_NATIVE_CONTROL=1` to require this control instead of
skipping it when `kubectl` is absent. The test creates a private temporary
kubeconfig pointing only at that loopback server; it never connects to production.
