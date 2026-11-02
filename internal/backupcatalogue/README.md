# Historical backup catalogue reader

This private implementation observes metadata in the exact Kopia fork used by
Velero 1.18.4. It has no production workflow or public command yet. Tests use
owned temporary repositories and an HTTP fixture; no production credentials or
backup objects have been accessed.

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

These observations do not establish content integrity, exact historical-state
equivalence, restore success, an atomic repository snapshot or permission to
erase a disk. Those remain separate migration gates.

Run `go test -race ./internal/backupcatalogue` from the repository root.
