# Longhorn client placement maintenance

The pinned Longhorn chart requires every volume to be detached before changing
its system-managed component selector. Database hibernation requires healthy
clusters first. Changing the selector without satisfying both conditions can
leave volume clients and pending database standbys unable to start.

The protected production deploy invokes four phases:

1. `before-publish` inventories consumers, retained storage, primary identities,
   and fresh backups where a backup plugin is configured. If missing clients
   prevent healthy database redundancy, it proves fail-closed replica exclusion
   and temporarily enables only the reviewed compute workers' clients. It then
   journals the original identities, suspends each affected Helm owner, pauses
   the three storage-consuming Deployments, and uses supported CNPG hibernation.
   Publication is refused until every volume is detached and no active Pod uses
   the retained claims.
2. `after-publish` requests infrastructure-controller reconciliation without
   waiting for temporarily suspended consumers. It checks client rollout, CSI
   registration, the corrected disk setting, and compute-worker replica
   exclusion, then restores owned pauses and temporary labels.
3. `verify` follows the existing full Flux gates. Two separated samples must
   prove database redundancy and archiving, unchanged primary and storage
   identities, retained detached recovery storage, and ready resumed consumers.
4. `restore` also runs on failure or cancellation. It attempts every owned
   restoration, even when another fails, and refuses replaced resources,
   foreign ownership, or intervening specification changes.

The journal is created before any mutation and is never overwritten or adopted
by another run. A completed migration is not repeated. An unfinished journal
requires its owning run's restoration; it is a hold, not permission to replay an
outage. The command never force-deletes Pods and never deletes claims, volumes,
databases, or backup objects. Supported CNPG hibernation gracefully terminates
database Pods while retaining their claims.
Backstage currently has no configured backup plugin; this procedure does not
claim otherwise.

These phases are not local administration commands. They require the protected
workflow identity and run only through the existing serialized production
deployment. The normal signer, publication, Flux, Talos, and recovery gates
remain in place. Local tests use fake resources or an explicitly supplied private
read-only snapshot; they perform no production mutation.
