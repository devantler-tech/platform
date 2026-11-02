# Historical backup catalogue reader

This implementation observes metadata in the exact Kopia fork used by Velero
1.18.4. Tests use owned temporary repositories and an HTTP fixture. Source and
fixture validation do not establish production execution or historical recovery.

The reader installs a fixed-scope GET/HEAD transport before constructing the
pinned S3 client. It rejects writes, redirects, changed endpoints, bucket/prefix
escapes, malformed responses, incomplete bodies and exhausted request, time or
byte budgets. Range responses must match the requested offsets. Only the exact
optional `kopia.blobcfg` may be absent, after strict `NoSuchKey` error validation.

Listing validates each complete XML page, its scope and final marker, metadata,
ordering and unique cursor progression. The stock iterator does not preserve all
the envelope fields needed for those checks. Repository opening disables
actions, repository logging, permissive cache loading and disk caching. Its
private temporary configuration contains an opaque in-process handle; backend
credentials and the repository password stay in memory. The manifest-skipping
environment override is rejected before access.

Every snapshot manifest is loaded through the native API with strict JSON and
metadata binding. One failed load or a changed index rejects the entire result.
Raw provenance remains private; serialized receipts contain counts and hashes.

The opt-in `Observe Historical Longhorn Backups` workflow accepts only a
confirmed first attempt on `main`, in the protected production environment and
the shared production deployment concurrency group. It reads the Wedding and
Budget repositories through five fixed native Kubernetes GET paths. Credentials
stay in runner memory; the kubeconfig and credential-free temporary repository
configuration use an owned private directory under `/dev/shm`. Both repositories
must complete before any receipt is printed. The command checks the current
`main` identity before each repository and after both reads.

Deployment bindings must match the reviewed base and production variable files,
the Ready BackupRepository and Available BackupStorageLocation, and the two
existing Velero credential Secrets. Complete before/after responses must remain
identical. The private comparison digest includes Secret data and never enters
the returned catalogue. Backend HTTP uses a dedicated transport without ambient
proxies, with TLS, connection, request, response and total-read limits.

Velero constructs Kopia paths from the BSL prefix, `kopia` and the repository's
`volumeNamespace`; the BackupRepository UID is not an object-key prefix. The
repository password comes from `velero/velero-repo-credentials`, key
`repository-password`, with whitespace trimmed and no missing-Secret fallback.
See the pinned [provider bindings](https://github.com/velero-io/velero/blob/v1.18.4/pkg/repository/provider/unified_repo.go#L426-L471)
and [prefix construction](https://github.com/velero-io/velero/blob/v1.18.4/pkg/repository/provider/unified_repo.go#L537-L564).

Every historical entry remains in the private catalogue even when its producer's
provenance is unrecognized. The current producer's CSI path convention cannot
prove how an older FSB producer labeled a snapshot, and a namespace or timestamp
match cannot prove equivalence to either original Longhorn snapshot.

These observations do not establish content integrity, exact historical-state
equivalence, restore success, an atomic repository snapshot or permission to
erase a disk. Those remain separate migration gates.

Run `go test -race ./internal/backupcatalogue ./scripts/observe-historical-longhorn-backups`
from the repository root. Dispatch the protected workflow from reviewed `main`
with the exact confirmation `observe-historical-longhorn-backups`; never rerun
an old attempt or execute the production command on an agent host.
