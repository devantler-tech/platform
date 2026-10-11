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
original resource and backing identity is proven.

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

Temporary legacy client eligibility can create an empty default disk while the
old disk setting is active. Fail-closed admission and the existing storage
policies must still exclude every compute worker from replica placement; the
procedure checks this before hibernation and after migration. It never removes
those restrictions or deletes a disk to clean up evidence.

These phases are not local administration commands. They require the protected
workflow identity and run only through the existing serialized production
deployment. The normal signer, publication, Flux, Talos, and recovery gates
remain in place. Local tests use fake resources or an explicitly supplied private
read-only snapshot; they perform no production mutation.
