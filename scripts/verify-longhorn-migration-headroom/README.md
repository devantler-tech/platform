# Observe temporary migration headroom

The protected **Observe Longhorn Migration Headroom** workflow reads the complete
Hetzner server inventory and brackets it with two native Kubernetes Node reads.
It performs no provider or Kubernetes mutation. Dispatch a new first attempt
from current `main` with confirmation `observe-longhorn-migration-headroom`.

The migration roster permits the six static nodes, two V1 migration workers and
an optional V2 worker. Static ownership, autoscaler pool membership, private
network, provider IDs, names, addresses, hardware and locations must agree.
Every server counts against capacity, including an unmatched or transitional
server. A positive result preserves one builder slot below the account limit;
it is a point-in-time observation, not a reservation or provisioning authority.

Reads and response sizes are bounded. Incomplete lists, duplicate or case-aliased
JSON fields, changed node identities, unavailable members and unknown workers
fail closed. Public output contains only the stage, counts and fixed validation
outcomes. A rejected provider envelope identifies which pagination or capacity
checks failed, without printing response values or weakening those checks.
Credentials and provider/node payloads remain in runner memory.

If the account is full, confirm `observe-longhorn-provider-account` to reconcile
the migration roster with up to ten provider records. This read-only diagnostic
requires two matching provider lists between unchanged Kubernetes Node reads.
It reports aggregate counts and a fixed classification of at most one
unmatched server based on provider labels. Use a separate, reviewed ownership
and cleanup procedure for any provider write. The normal headroom gate still
requires a reserved builder slot.

If the census cannot identify an unmatched server or reports a control-plane
index mismatch, confirm `observe-longhorn-provider-ownership`. The same
stable reads produce a small locator for the operator, containing only the
unmatched server identity and mismatched control-plane indexes. The workflow
encrypts that output for the maintainer before publishing its logs. A locator
helps find the affected records in Hetzner. It grants no cleanup or admission.

A stable account can remain held by a joined node's ownership metadata. Its
failure report includes the provider-only candidate's fixed label classification,
with ownership and headroom explicitly unverified. The operation still fails and
emits no accepted census receipt. Changed, unreadable or incomplete inventories
produce no candidate classification.

The later worker activation must repeat this check immediately before mutable
publication and before updating the pool, then require the full nine-node roster
after Talos readiness. An earlier workflow result alone is insufficient. These
checks do not prove admission cache recovery, local disk layout, replica
preservation, historical recoverability, benchmark results or iSCSI removal.

## Reconcile the verified provider records

The protected **Retire Abandoned Longhorn Image Builder** workflow binds one
reviewed server to its original creation time and both Primary IP IDs. It requires
an unchanged production Node inventory, the original provider action history,
no attachments or backups, and exactly the two approved auto-delete Primary IPs.
Completion requires a successful delete action and provider readback that the
server and both IPs are gone.

The protected **Repair Longhorn Provider Index** workflow writes the canonical
index label for each reviewed static server. It ties each edit to the native
Node UID and carries forward every other provider label. It validates the
proposed roster before writing and compares the full provider and Node
inventories afterward. If either operation loses its response, inspect provider
state before deciding whether a fresh first attempt is needed.

Both workflows require their own operation name as confirmation on current
`main` and share the protected production deployment lock. The
`plan-retire-builder` and `plan-repair-index` commands exercise the same
preflight through the production read-only Kubernetes context and Hetzner token.
