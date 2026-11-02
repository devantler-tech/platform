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
fail closed. Public output contains only the stage and counts. Credentials and
provider/node payloads remain in runner memory.

The later worker activation must repeat this check immediately before mutable
publication and before updating the pool, then require the full nine-node roster
after Talos readiness. An earlier workflow result alone is insufficient. These
checks do not prove admission cache recovery, local disk layout, replica
preservation, historical recoverability, benchmark results or iSCSI removal.
