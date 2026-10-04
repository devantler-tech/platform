# Read-only storage retention audit

Capture with an approved read-only context into a new private directory, then
audit immediately:

```bash
bash scripts/audit-storage-retention/capture.sh CONTEXT /private/tmp/NEW-CAPTURE
go run ./scripts/audit-storage-retention --snapshot-dir /private/tmp/NEW-CAPTURE
go test ./scripts/audit-storage-retention
```

The capture reads four fixed API list endpoints without a page limit, preserving
the typed list and its resource version. Ordinary kubectl JSON flattens these
into a generic list and loses the list version, so it is rejected. A failed
read leaves no completion receipt. The auditor rejects missing lists, API error
objects, pagination tokens, missing resource versions, duplicate identities,
empty storage coverage, stale captures over five minutes, future timestamps and
inconsistent volume/claim bindings in both directions, including an extra Bound
claim pointing at an absent volume. The lists are separate API snapshots: a
failed UID join during controller activity means **UNKNOWN**, followed by a new
capture, never clean coverage.

Exit codes: `0` CLEAN, `1` WARNING, `2` UNKNOWN or invalid usage. Output contains
aggregate counts only; raw API captures remain private. Keep `go run`'s own exit
behavior in mind: build a binary when the caller needs to distinguish exit 1
from exit 2 directly. The check never patches, applies or deletes anything and
does not establish backup or recovery coverage.

Warnings cover every Delete class, every non-temporary Delete volume, and
Released volumes older than `--released-threshold` (default 24 hours). All
Released volumes are counted, including those below the threshold. Missing or
future phase-transition timestamps are UNKNOWN. This threshold reports age; it
does not authorize disposal or decide retention spend.

A backup exemption requires a Bound PV/PVC name and UID join, the same storage
class and reciprocal binding, a claim in the configured platform namespace
`velero`, a controller owner reference to a currently observed
`velero.io/v2alpha1` DataUpload with matching name and UID, and the CSI exposer's
VolumeSnapshot data source. Namespace or labels alone cannot exempt a volume.
Released volumes and restored application claims are never exempted.

The owner reference and claim-name shape were checked against the official
[Velero v1.18.1 CSI snapshot exposer](https://github.com/velero-io/velero/blob/v1.18.1/pkg/exposer/csi_snapshot.go).
Recheck the shape and negative controls before accepting a different backup
controller version. This implementation is an operator-invoked audit; a
standing scheduled check and the one-way Retain migration remain part of #4441.
