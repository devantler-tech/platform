# ADR: Git owns deletion; the storage layer keeps the data

- **Status:** Accepted. The maintainer set the direction on 2026-08-25; this records it and the order
  it rolls out in. Retention is not deployed: the readback on 2026-10-04 still found all four
  StorageClasses and all 30 PersistentVolumes using `Delete`. The force-replacement safeguard
  is enforced independently; it does not establish retention or authorize removing prune opt-outs.
- **Decision:** A resource removed from Git is removed from the cluster, whatever it holds. Data
  survives because every StorageClass and every PersistentVolume uses the `Retain` reclaim policy,
  not because a resource is exempt from pruning. No resource keeps
  `kustomize.toolkit.fluxcd.io/prune: disabled` as a standing protection.
- **Tracks:** [#3369](https://github.com/devantler-tech/platform/issues/3369). Unblocks
  [#3367](https://github.com/devantler-tech/platform/issues/3367).

## Context

### Deletion today

Every Flux `Kustomization` prunes, and `scripts/tests/test-flux-kustomization-prune.sh` keeps it
that way. Production then takes most of what pruning would delete out of its reach:

- The `annotations-transformers` component stamps `kustomize.toolkit.fluxcd.io/prune: disabled` on
  every PersistentVolumeClaim, HelmRelease and Namespace. All four production roots include it
  (`bootstrap`, `infrastructure/controllers`, `infrastructure`, `apps`). In production that is 37
  of 43 namespaces and 44 of 44 HelmReleases.
- Hand-written annotations add the four CloudNativePG clusters, one claim, one namespace, five
  objects of one tenant's host policy and one whole tenant directory.

The protection arrived with #3168, after #3167. A merge-queue candidate is deployed to production
before it merges. If the candidate deletes something and is then evicted, the deletion has already
happened, and restoring `main` cannot undo it. In #3167 a claim removed by an evicted candidate
stayed `Terminating` under a running pod and failed every later deploy.

What the protection costs:

- A tenant removed from Git keeps running until someone deletes it by hand (#3367).
- A namespace or release that a failed candidate created stays when `main` is restored. The heal
  job and a daily check report such leftovers (#3502), but nothing removes them.
- Retiring a claim, a release or a namespace takes two merged revisions and a manual deletion
  (`AGENTS.md`, "Persistence retirement is always two-stage").

One tree already protects itself at the resource layer instead. The UniFi tree prunes, and its
managed resources are stripped of the `Delete` management policy, so removing one from Git never
deletes the network object behind it (#3373).

### Storage today

Measured on the production cluster on 2026-10-03:

| Class | Provisioner | Binding | Declared by | Volumes |
| --- | --- | --- | --- | --- |
| `hcloud` | `csi.hetzner.cloud` | WaitForFirstConsumer | a value of the `hcloud-csi` chart | 16 (180 GiB) |
| `longhorn-wffc` (default) | `driver.longhorn.io` | WaitForFirstConsumer | a manifest in this repository | 12 (57 GiB) |
| `longhorn` | `driver.longhorn.io` | Immediate | a value of the `longhorn` chart, applied by Longhorn | 2 (20 GiB) |
| `longhorn-static` | `driver.longhorn.io` | Immediate | Longhorn, when the class is missing; not in Git | 0 |

All four classes are `Delete`. All 30 volumes are `Delete` and bound.

The 30 claims sit in 9 namespaces. Flux applies one of them as a manifest. Controllers create the
other 29: CloudNativePG creates 11 for four database clusters, the Coroot operator creates 8, and
10 come from StatefulSet claim templates and charts. So the annotation on claims protects one
claim. The other 29 are protected only through the annotation on the HelmRelease, Namespace or
database cluster whose deletion would take them along.

Which workloads hold data cannot be read from this repository. One tenant's database is declared in
the tenant's own repository, and this repository renders no claim, release or database cluster for
it.

### What the pinned versions do

Read from the charts this repository references and from the released source of each component:

- **Kubernetes 1.36.** `reclaimPolicy` on a StorageClass cannot be changed, and neither can its
  parameters, provisioner or binding mode. A volume copies the policy from its class when it is
  provisioned and keeps its own copy afterwards, so changing a class changes no existing volume.
  The policy on a volume can be edited. When a claim is deleted, a `Retain` volume moves to
  `Released` and the volume controller does nothing more with it.
- **CSI provisioner.** It deletes the storage behind a volume only when the volume is `Released`
  and its policy is `Delete`.
- **Flux kustomize-controller 1.8.** Platform and generated tenant layers set `force: false`:
  an immutable conflict fails without replacing the object. Three setup Jobs explicitly opt
  into recreation with `kustomize.toolkit.fluxcd.io/force: enabled`. That annotation is only an
  opt-in; `force: disabled` cannot exempt an object from a forcing layer. The repository guard
  rejects forcing layers, including tenant templates and rendered patches, and force opt-ins
  on manifest-owned claims or database clusters. Within one Kustomization, classes are applied
  in an earlier stage than HelmReleases.
- **Flux helm-controller 1.5 (Helm 4.2).** Helm updates an object in place and never deletes it to
  recreate it, so a changed `reclaimPolicy` fails the upgrade. Helm creates an object that is
  missing, and deletes one that left the chart unless it carries `helm.sh/resource-policy: keep`.
- **Longhorn 1.12.1.** The chart writes the `longhorn` class into a ConfigMap, and Longhorn deletes
  and recreates the class when that ConfigMap changes. Longhorn creates `longhorn-static` only when
  it is missing and sets no policy on it, so the class is `Delete`; the PersistentVolumes Longhorn
  itself creates with that class are always `Retain`. Longhorn never deletes a volume because its
  claim went away: only the CSI delete call does that. Longhorn refuses to uninstall unless a
  confirmation setting is turned on, and that setting is off here.
- **hcloud-csi 2.23.0.** The chart renders each entry of `storageClasses` as an ordinary
  Helm-managed class. An empty list renders none.
- **Velero 1.18.** Its data mover creates a temporary claim and volume in Velero's own namespace
  for each snapshot backup. It sets `Delete` on that volume before it deletes the claim, so the
  volume is removed when Velero cleans up after the backup, whatever the class says.

## Options considered

### Retain at the storage layer — adopted

`Retain` is the Kubernetes primitive for "delete the claim, keep the data". The claim stays an
ordinary declarative resource. It protects all 30 volumes in the same way, however the claim is
deleted, and it does not need to know which tenants are stateful.

### Deny deletion at admission — rejected

A Kyverno policy that denies deletion without explicit intent is declarative and would also stop a
candidate that was never merged. But a denied prune makes the owning Flux `Kustomization` report a
reconciliation error, and that stops it for every tenant it manages, not only the one being
removed. `Retain` lets the deletion succeed.

### Narrow the annotation to stateful resources — rejected

#3368 kept the annotation on claims and releases and took it off namespaces. A narrower opt-out is
still an opt-out: whatever keeps the annotation is still outside declarative management. Any rule
that treats stateless tenants differently also needs to know which tenants are stateless, and that
cannot be read from this repository.

### New class names instead of replacing the classes — rejected

Adding `Retain` classes under new names avoids replacing a class. But a claim cannot change its
class, and a StatefulSet cannot change its claim template, so every workload that names an old class
would keep creating `Delete` volumes until the workload itself is recreated.

## Decision

1. **Data is retained at the storage layer.** Every StorageClass is `Retain`. Every
   PersistentVolume is `Retain`, except a temporary volume that its own controller sets to `Delete`
   in order to remove it, as Velero's data mover does.
2. **Nothing opts out of pruning to protect what it holds.** The transformer clause and every
   hand-written `prune: disabled` are removed. The opt-outs that guard something other than stored
   data (the admission controller's namespace, one tenant's host policy, one handover between
   controllers) fall under the same rule: each is replaced by protection at the layer that owns
   the risk, or removed. An annotation that exists only while ownership of an object moves from one
   controller to another is not a standing opt-out, and it names the change that removes it.
3. **Classes keep their names and are replaced in place:**

   | Class | Desired declaration | Existing-cluster prerequisite |
   | --- | --- | --- |
   | `longhorn-wffc` | set `reclaimPolicy: Retain` in its manifest | A separately reviewed, bounded class replacement is needed; Flux with `force: false` rejects this immutable change. |
   | `longhorn` | set `persistence.reclaimPolicy: Retain` in the chart values | Rehearse Longhorn's ConfigMap-driven recreation and preserve every other class field. |
   | `longhorn-static` | declare it in Git with `Retain` | Longhorn leaves an existing class alone. A separately reviewed replacement must establish Retain before ordinary Flux ownership can converge. |
   | `hcloud` | move it from the chart to a manifest with `Retain` | Prove both directions of Helm ownership transfer and bounded replacement. `keep` alone and ordinary Flux apply do not replace an immutable field. |

   The replacement design must fence concurrent provisioning and preserve class identity fields,
   every PVC/PV UID and binding, and workload availability. Delete only the specifically reviewed
   StorageClass; never a claim or volume. The expected gap may delay provisioning and must be
   measured in a rehearsal, rather than assumed safe. On a
   cluster rebuilt from nothing, each class is created with the declared policy. KSail installs the
   `hcloud-csi` chart with its defaults before Flux takes over. The bootstrap-to-Git handoff must
   also reach Retain without re-enabling layer-wide force or leaving a second default class.
4. **Existing volumes are changed one by one, through Git.** A one-shot job or a policy sets
   `Retain` on every volume that has `Delete`, so the change is reviewed and can run again on a
   rebuilt cluster. It only ever moves `Delete` to `Retain`. A namespace alone cannot identify
   Velero's temporary volumes. Exemption requires reciprocal Bound PV/PVC names, matching claim
   UID, and controller ownership joined to the live DataUpload UID and CSI snapshot source.
   An incomplete or inconsistent read is UNKNOWN and stops migration; a Released volume is never
   automatically exempted. A deliberate way to discard a single released volume remains.
5. **Forced replacement is a separate hazard with the same backstop.** Platform and generated
   tenant layers set `force: false`, and the repository guard rejects forcing layers or force
   opt-ins on manifest-owned claims and database clusters. Three setup Jobs explicitly opt into
   recreation. `force: disabled` still cannot exempt an object from a forcing layer, while
   `Retain` protects data but not service availability. The force-safety safeguard tracked by
   #4448 is enforced separately from the storage-retention rollout described here.
6. **The order is fixed:** retain the data, prove it can be brought back, stop evicted candidates
   deleting anything, and only then remove the opt-outs. Steps 2 and 3 of the order first written
   on #3369 are swapped; the reason is under "Before the opt-outs are removed".

## Rollout

| Step | Change | Rollback | Issue |
| --- | --- | --- | --- |
| 1 | Rehearse and review bounded replacement; the three Longhorn classes become `Retain` with layer-wide force still false. | Prefer forward repair preserving Retain. A plain Git revert cannot replace the immutable fields. Any reverse replacement needs its own reviewed identity and provisioning fences; existing PV policies remain Retain. | #4439 |
| 2 | Rehearse the ownership transfer and bounded replacement; `hcloud` becomes `Retain` and moves from the chart to a manifest. | Prefer forward repair preserving Retain. An ordinary revert may fail on immutable fields or delete the class during ownership transfer. Review and rehearse the complete reverse order before deploying it. | #4440 |
| 3 | Every existing volume becomes `Retain`, apart from Velero's temporary volumes. A standing check reports any class that is `Delete`, any other volume that is `Delete`, and any volume left `Released`. | Remove the mechanism. A bound volume may be set back by hand. A released volume is never set back to `Delete` unless it is being discarded, because that deletes its data at once. | #4441 |
| 4 | The reclaim procedure below is written into the disaster-recovery runbook and drilled once per driver. | Revert the documentation. | #4442 |
| 5 | A merge-queue candidate that is evicted has deleted nothing in production. | Revert to the current merge-queue deploy. | #4443 |
| 6 | Every prune opt-out is removed, apart from a handover annotation that is still in flight, and the contract flips from "everything must be protected" to "nothing may be". | Revert; the annotations return on the next reconcile. Anything pruned in between is not restored by the revert: its volumes are released and are brought back with the reclaim procedure. | #4444 |
| 7 | Tenant decommissioning is documented as deleting the tenant's directory. | Revert the documentation. | #4445 |

Read-only observation for step 3 can land first. It changes no reclaim policy and clears none of
the later gates. Class migrations in steps 1 and 2 require a reviewed replacement design and
rehearsal; step 5 is independent. The existing-volume migration in step 3 follows 1 and 2, step 4
follows 3, step 6 follows 3, 4 and 5, and step 7 follows 6. Rendered output alone cannot prove class
replacement, recovery, or the absence of production deletions by an evicted candidate.

## Reclaiming a released volume

When a claim is deleted, its volume moves to `Released`. The PersistentVolume object stays and
still names the deleted claim. The storage behind it is untouched. Nothing reuses or removes it on
its own.

The procedure below follows from how Kubernetes and the CSI provisioner behave. It has not been run
on this cluster yet: step 4 of the rollout drills it, and nothing depends on it before then.

**To bring the data back:**

1. Act before the claim is recreated. A claim that comes back first, for example because its
   manifest was restored, is given a new, empty volume.
2. Reserve the released volume for the claim that should own it: edit the claim reference on the
   volume so that it holds only that claim's namespace and name, and remove the old claim's `uid`
   and `resourceVersion` from it. The volume becomes `Available`, and only that claim can bind it.
   Leaving the old `uid` in place keeps the reference tied to the deleted claim and prevents the
   replacement claim from binding, even when the namespace and name are unchanged.
3. Let the claim be created, by restoring the manifest or by letting the operator or StatefulSet
   create it. Verify that its storage class, requested access modes, volume mode, selector and
   capacity match the retained volume. Verify that its workload can schedule under the volume's
   node affinity; reservation does not make an incompatible placement usable. After binding,
   compare the volume UID and storage handle with the private pre-drill record and read back a
   seeded data checksum. A new empty volume or a Pending claim is a failed recovery drill.
4. A claim that an operator owns may also need the labels and annotations the operator expects on
   it. Step 4 of the rollout records these per operator. For databases, the backups described in
   [`dr/velero-cnpg.md`](../dr/velero-cnpg.md) remain the recovery path of record.

**To discard the data:**

1. Confirm nothing needs it, and that a backup exists where one is expected.
2. Set the policy of that one volume to `Delete`. The CSI provisioner then deletes the storage
   behind it and the PersistentVolume object.
3. If the PersistentVolume object was deleted first, the storage is left with nothing in
   Kubernetes pointing at it. Delete the Longhorn volume, or the Hetzner volume, by the handle that
   was recorded on the PersistentVolume.

**What a released volume costs until then.** This is a spend trade-off, and this record does not
decide how long a released volume is kept; that is the maintainer's decision, volume by volume.

- An `hcloud` volume stays in the Hetzner project, detached, and is billed for as long as it
  exists.
- A Longhorn volume has no bill of its own, but its replicas keep their space on the storage nodes.
  Running out of that space has already stopped replicas from rebuilding (#3201).

Every deleted claim leaves a released volume, including the ones a controller deletes in normal
operation, such as a database operator replacing an instance. Step 3 reports them so that none is
kept by accident.

## Before the opt-outs are removed

All of the following hold first:

- **Every class and every volume is `Retain` on the live cluster**, apart from Velero's temporary
  volumes, and a standing check reports any that is not. The check reads the cluster, not the
  manifests: 29 of the 30 claims are not manifests in this repository.
- **A released volume has been brought back, and one has been discarded**, once for each driver,
  by following the written procedure, including a claim owned by an operator.
- **An evicted merge-queue candidate deletes nothing in production.** `Retain` keeps the data but
  not the service. With the opt-outs gone and candidates still deployed before they merge, any
  evicted pull request that removes a tenant, a release or a namespace would cause an outage that
  restoring `main` cannot repair, and the failure in #3167 would return. Removing the opt-outs does
  not clean up after such a candidate either: an object that a failed apply never recorded in a
  Flux inventory is invisible to pruning (#3502).
- **No rule depends on knowing which tenants are stateful.** That cannot be read from this
  repository, so nothing in the rollout classifies tenants.
- **Each removed opt-out names what else is deleted with the object, and where that is kept.**
  `Retain` covers volumes only. It does not cover a secret that exists only in the cluster, a
  custom resource removed together with its definition, an external resource managed through
  Crossplane, or the storage system itself.
- **Backups stay the recovery path of record**, and step 6 checks that every claim on the live
  cluster is covered by one or records why it needs none. A retained volume is on the same disks
  and in the same account as the workload it belonged to. It is not a backup.

## Consequences

**Positive.** Removing a tenant is deleting its directory. An object that a failed deploy left in a
Flux inventory is pruned when `main` is restored. Retirement no longer needs two revisions; the
manual step that remains is discarding the released volume. The protection no longer depends on an
annotation reaching 29 claims indirectly.

**Trade-offs.** Deleted claims leave volumes that cost money or disk space until someone reclaims
them. Restoring a manifest does not restore its data unless the released volume is reserved first.
Replacing a class is a short interruption to provisioning. The `hcloud` class changes owner, from
the chart to a manifest.
