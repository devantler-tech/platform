# Observe namespace metadata before benchmark cleanup

This protected, manually confirmed workflow checks whether production can
return a complete metadata inventory of the existing Longhorn namespace.
It is the read-only foundation for #5666, not a benchmark cleanup procedure.

After the change has merged, dispatch
[`observe-longhorn-namespace-inventory.yaml`](../../.github/workflows/observe-longhorn-namespace-inventory.yaml)
on current `main`, with confirmation `observe-longhorn-namespace-inventory`.
Only its first attempt is accepted. Current-main checks bracket the actual
observation, under the production deployment lock. Do not rerun a failed
attempt; retain its result and diagnose the cause first.

The command makes fixed GET requests. It discovers every served API version,
then lists metadata for every namespaced parent resource that can be deleted.
It refuses missing list support, partial or ambiguous lists, discovery changes,
namespace replacement, and a remotely aggregated API with unexplained deletion
coverage. It requests metadata-only responses, including for Secrets, and
never falls back to their values. Nothing is created, changed or deleted.

Each response is bounded to 1 MiB, with at most 512 requests and 16 MiB total.
The inventory has a two-minute deadline inside the three-minute command
deadline. List continuation or a nonzero remaining-item count is a hold;
pagination is intentionally not implemented. The source check and fixed native
Namespace read have separate command deadlines and a 2 MiB output limit.

Authentication uses the official Kubernetes client for the consumer's pinned
Kubernetes minor version. The workflow stores its kubeconfig in a private
memory-backed directory and selects the explicit production context. The
loader requires verified inline TLS trust and either an inline bearer token
or an inline client certificate and key. External credential files, executable
authentication plugins, proxy overrides, impersonation and ambient contexts
are refused. Credentials, names, object identities and topology are not printed
or uploaded.

A passing public receipt says only that the metadata observation completed
with a stable Namespace identity and source/run binding. Its
`deletionAuthorized` field is always false. It proves no default-resource
ownership, cleanup admission coverage, benchmark result, storage retirement,
or data preservation. The future cleanup controller must bind its own private
inventory to original receipted object UIDs and prove its deletion guard before
deleting a benchmark namespace.

The package also exposes `Observe` for a receipted benchmark namespace. Its
returned bindings stay private, and its only other allowed scope is the fixed
Longhorn calibration above. No public command accepts an arbitrary namespace
or UID from an issue, log or workflow input.

Run the offline regressions without a cluster or credentials:

```bash
go test -race -count=1 -timeout=3m ./internal/namespaceinventory ./scripts/observe-longhorn-namespace-inventory
```
