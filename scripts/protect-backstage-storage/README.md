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
The registry and GitHub API readers use the short-lived Actions job token with
read-only permissions, including `packages: read`. The OCI publication credential
never enters this job. Tokens are not put in command arguments. Missing
registry authentication stops verification rather than falling back to API-held
attestations, which this publication path does not create.

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
