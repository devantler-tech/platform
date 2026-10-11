# Longhorn client placement maintenance

The pinned Longhorn chart requires every volume to be detached before changing
its system-managed component selector. Database hibernation requires healthy
clusters first. Changing the selector without satisfying both conditions can
leave volume clients and pending database standbys unable to start.

Delivery is staged. Merge this procedure and its legacy-publication guard
without changing the live selector first. A follow-up configuration repair then
activates the audited chart migration. Recovery uses the already-merged guard:
it refuses to restore the obsolete selector after the migration is live. That
refusal remains a visible failure, not a successful rollback or relaxed gate.

The protected production deploy invokes four phases:

1. `before-publish` inventories consumers, retained storage, primary identities,
   and fresh backups where a backup plugin is configured. Every database must
   already be fully healthy and every node Ready before any maintenance write.
   An unhealthy database requires recovery first; the procedure never borrows
   client labels. It journals the original identities, suspends each affected Helm owner, pauses
   the three storage-consuming Deployments, and uses supported CNPG hibernation.
   Publication is refused until every volume is detached and no active Pod uses
   the retained claims.
2. `after-publish` requests infrastructure-controller reconciliation without
   waiting for temporarily suspended consumers. It checks client rollout, CSI
   registration, the corrected disk setting, and compute-worker replica
   exclusion, then restores owned service pauses.
3. `verify` follows the existing full Flux gates. Two separated samples must
   prove database redundancy and archiving, unchanged primary and storage
   identities, retained detached recovery storage, and ready resumed consumers.
4. `restore` also runs on failure or cancellation. It attempts every owned
   restoration, even when another fails, and refuses replaced resources,
   foreign ownership, or intervening specification changes. Once publication
   might have started, consumers remain paused until the new clients positively
   converge. A failed publication is not evidence that it published nothing.
   A failed transition stays HOLD for protected recovery, never a false success.

The journal is created before any mutation. A completed migration is not
repeated. An unfinished pause requires its owning run's restoration. A fully
restored journal can be superseded only after live proof that all original
resources have unchanged identities, cleared ownership and restored pause
controls, and a UID/resourceVersion-guarded journal replacement. Later reviewed
consumer upgrades may change unowned specifications; a fresh plan is collected
before another pause. Pausing and interrupted restoration still require the
strict original specification proof. An interrupted writer can be
taken over only when GitHub proves its exact run attempt completed and live
resource ownership and retained storage still match. It can resume a fully
detached publication, or restore and restart the recorded transition. Live or
unknown previous writers remain HOLD. An unchanged legacy selector can be
republished after positively unstarted publication only once release of every
original resource and backing identity is proven. A later storage expansion
does not block that released-state proof: claim/PV bindings and every other
specification field, plus backing identity and format, must still match. Active
or interrupted maintenance continues to require the original capacity too.

The initial migration revision must change no production manifest except the
Longhorn selector manifest. A combined consumer upgrade is rejected before any
pause rather than verifying it against stale pre-publication specifications.
Fail-closed admission and every existing autoscaled Longhorn Node's storage
exclusion are checked even when the compute pool is empty or already labeled.

The command never force-deletes Pods and never deletes claims, volumes,
databases, or backup objects. Supported CNPG hibernation gracefully terminates
database Pods while retaining their claims.
Backstage currently has no configured backup plugin; this procedure does not
claim otherwise.

Fail-closed admission and the existing storage policies must still exclude every
compute worker from replica placement before hibernation and after migration.
The procedure never patches Kubernetes Nodes, removes storage restrictions or
deletes disks. Journals containing temporary client-label targets are outside
the reviewed scope and are rejected before cleanup; that experimental path was
removed before this procedure was deployed.

These phases are not local administration commands. They require the protected
workflow identity and run only through the existing serialized production
deployment. The normal signer, publication, Flux, Talos, and recovery gates
remain in place. Local tests use fake resources or an explicitly supplied private
read-only snapshot; they perform no production mutation.
