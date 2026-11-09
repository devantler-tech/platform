# Prepare Longhorn V2 workers

`inventory-storage-layout` adds read-only native block metadata to the storage-root
observation for a fixed temporary worker. Confirm it with
`inventory-longhorn-storage-layout-<selection>`. Two complete, bounded native volume
status and physical-discovery collections must agree inside the unchanged node,
hardware and boot bracket. Its schema-2 observation-only receipt reports sizes,
phases, encryption properties and counts, while withholding device paths and
identities. It grants no reset clearance and never treats a failed read as absence.
The original root-only mode keeps its existing scope and schema.

`inspect-host-memory` is a read-only diagnostic for a fixed permanent worker,
confirmed with `inspect-longhorn-v2-host-memory-<worker>`. It reads only the native
huge-page allocation and selected `/proc/meminfo` counters, bracketed by unchanged
Kubernetes, storage, hardware and kernel-boot identity and the current reviewed
source. Its observation-only receipt contains counters and commitments, never raw
host text or private identities. An allocation shortfall is reported as observed;
it grants no reset, reboot, restart or V2-activation clearance. Incomplete reads,
changing allocation, identity changes and close failures produce no success receipt.
Unassigned replacement replicas do not prevent this counter-only observation.
Replica inventories still require complete placement, and reboot recovery still
requires two intact peer copies before scheduling and full redundancy afterward.

For a confirmed allocation shortfall, `reboot-host-for-hugepages` selects exactly one
permanent worker with `reboot-longhorn-v2-host-<worker>`. It requires the loaded
reservation, unused native huge pages, disabled V2, the complete fixed migration roster, all V1
volumes fully redundant, and all three database clusters healthy. It records the
original node, hardware, boot, encrypted EPHEMERAL partition, storage disks and
database identities before an owned cordon and an eviction-based application drain.
The live `block-if-contains-last-replica` policy and target V1 instance-manager
ownership are verified on each health read. Only that ready storage Pod is kept
running during drain; a workload impersonating its selector label fails admission.
Application evictions keep the normal disruption-budget checks. After
drain, known attachment transitions and the unchanged database quorum share up
to 20 minutes to settle. Attaching or detaching volumes must still retain three
healthy copies on ready disks with known engine and replica states. Peer ownership
is checked during this wait. Changed identities, lost quorum, failed runtime
states or other safety evidence stop the wait;
a timeout preserves the cordon without requesting a reboot. Two fresh checks of
complete redundancy, databases and original storage still precede durable
reboot intent. The single reboot request has no reset or disk-wipe selector.
An unfinished or unreadable peer journal, another worker's cordon, or lost proof
of a peer's recorded recovery prevents starting the next worker.

New journals bind all five production PostgreSQL clusters and their full declared
replica counts. The two-instance kagent and Substrate databases must both recover
before the reboot, alongside Coroot, Umami and Wedding. Existing journals preserve
their original identities. A legacy journal that has yet to request a reboot must
bind the current database roster before moving the worker.

Recovery never repeats an uncertain reboot. It requires a changed boot with the
same node, hardware, OS, encrypted partition, registered disks and database system
identities, 1,024 native 2 MiB pages, matching 2 GiB Kubernetes capacity, clear node
pressure and two intact copies outside the target before restoring scheduling.
That intermediate check permits a degraded attached volume only with two live
read-write peer replicas; final acceptance still requires healthy volumes and all three copies.
The verified new boot is journaled as `returning`, and only the owned cordon is
released so Longhorn can recreate its instance manager. The maintenance marker
and unfinished journal remain until full V1 redundancy and database health return;
a timeout at this stage leaves the node schedulable under that retained marker.
Only then is the marker removed and recovery recorded. Both release steps are read back.
Dispatch each worker separately and verify platform recovery before the next one.
An interrupted run keeps its journal and cordon; a new first-attempt main dispatch
may adopt it only after the previous run has ended. This operation enables no V2
storage and removes no iSCSI support.

The `Prepare Longhorn V2 Migration` workflow runs guarded steps towards
platform#5582. It does not migrate a claim, enable V2, disable V1 or remove iSCSI.

This tool lands in stages (platform#5655). It includes `refresh-clients`, `prepare-worker`,
`prepare-unused-temporary-worker`, read-only replica inventories and the benchmarks.
`refresh-clients` holds until the fixed migration workers are Ready and the host
huge-page settings are loaded. Worker preparation is below and is dormant; enabling temporary
capacity follows in its own change.

For the unused temporary-worker route (platform#5787), finish the client refresh and the
cleaned V1 baseline first. Select `prepare-unused-temporary-worker`, `temporary-v1-1` or
`temporary-v1-2`, and the matching confirmation `prepare-unused-longhorn-v2-temporary-v1-1`
or `prepare-unused-longhorn-v2-temporary-v1-2`. Each selection is resolved from the complete,
reviewed two-member V1 pool and one-member V2 pool. The fresh V2 pool has no reset operation.

This operation wipes **only the selected temporary worker's EPHEMERAL partition** and reboots
it. Its separate journal binds the original node UID, provider and hardware identities, boot,
encrypted partition UUID, and native storage commitments. It requires unregistered, fenced
Longhorn storage with no placed replicas, complete native parent-directory and block-resource
inventories, and no allocated raw partition. Only the recognized engine, metadata, log and
socket cache directories may remain; a replica entry, backing images, any file, unknown entry,
incomplete read or changed identity holds the run. The original encrypted partition is checked
before each parent listing. A missing replica-path error is never interpreted as empty storage.
An allocation in `failed` state qualifies only when its previous phase was `waiting` or
`missing`, its type is a partition, every allocation size, location and identity field is
empty, and complete physical discovery contains no raw or other user partition. A failure
after locating, provisioning or opening a device holds the run. Provider error messages do
not establish absence. These checks accompany both parent reads and the unchanged API,
hardware, boot and original-partition bindings; observation-only receipts grant no reset clearance.
All three temporary workers receive owned scale-down protection before the selected worker is
cordoned. Drain uses eviction and respects disruption budgets. Two fresh storage observations
after drain precede the recorded reset intent.

Recovery from `reset-requested` only observes post-state. An uncertain response never causes a
second reset. A new boot and encrypted EPHEMERAL UUID, the reviewed 96 GiB encrypted raw
partition, loaded configuration and huge-page readiness must be observed before releasing the
owned cordon. CPX42 workers retain their larger root capacity; the permanent-worker root-size
cap does not apply to this separate profile. The raw disk remains unregistered and fenced;
storage activation and volume migration follow separately. Original storage replicas remain on
the permanent workers throughout this preparation route.

When a replica-directory observation holds, `inventory-storage-root` inspects the fixed
parent `/var/lib/longhorn` directory. Use the same fixed node selections with the matching
confirmation `inventory-longhorn-storage-root-<selection>`. It brackets two complete native
metadata listings with the same node, hardware, boot, API-inventory and source checks.
The receipt reports counts, known directory-presence flags and a metadata digest; it grants
no reset clearance. A file named `replicas` remains distinguishable from a directory with that
name. Closed native failure categories identify transport, metadata or entry-validation holds
without printing provider errors, file names or file contents. A failed replica listing never
becomes an empty listing, and parent observations do not establish historical recoverability.

Dispatch only the merged workflow on `main`. Run `refresh-clients` promptly after
`talos/workers/enable-longhorn-v2.yaml` deploys. That configuration requests the pages;
the refresh must prove the actual allocation before changing kubelet capacity. Until
capacity is refreshed, kubelet can still count reserved pages as free memory, so an
unrelated kubelet restart could reject Pods.
Select `refresh-clients` with
`refresh-longhorn-v2-clients`. It verifies the live host allocation and that each kernel module is usable, loaded or built in,
restarts only kubelet where capacity is stale, and requires every worker to report
2 GiB of 2 MiB huge pages. Containers and disks are preserved. Kubelet would reject
any Pod that no longer fits once huge pages count, so when a permanent worker's Pods
request too much, it first moves the fewest Pods that make them fit, largest first. Only a
Deployment Pod with another Ready replica qualifies, so its workload keeps serving. It
also needs no data of its own (no claim, scratch or host volume), no node pinning or
required placement rule, and to be outside the cluster, storage and Flux namespaces. When those
Pods are not enough, the vulnerability scanner `kubescape/kubevuln` moves too, even as a single
replica: it serves no traffic and its next scheduled scan repeats the work. The worker
stays cordoned, under a `longhorn-v2-refresh-<worker>` ownership annotation, until kubelet
reports the smaller allocatable; otherwise moved Pods could return. Each move is an
eviction, so disruption budgets decide, and the next Pod leaves only once the workload is
back at its Ready count. It holds before cordoning when the qualifying Pods cannot cover
the difference, no other schedulable worker has the memory, or the worker is a
migration-pool node, which never moves Pods; such a hold stops the run before any later
worker, so reduce requests or move those workloads by a reviewed change first. A move refused after retries, a replacement
that never turns Ready, or a Pod that came back releases the cordon with no restart. An
unproven restart keeps the cordon, and so does an interrupted run, which fails the next
deployment's node readback: dispatch `refresh-clients` again. It adopts only that worker's
own cordon. Once kubelet reports the smaller allocatable it releases that cordon before the
roster and volume checks; otherwise that worker goes first, after those checks, so unhealthy
storage leaves the cordon in place until it recovers. A later run that holds before restarting
releases the earlier cordon.

The restart holds under node pressure, and when Pods that landed since the first measurement
no longer fit. For two minutes after kubelet reports its new capacity, a rejected Pod or a Pod
that was running and has gone fails the run and keeps any cordon. Ordinary Pod churn can trip
this too; check the worker, then dispatch again.

Next select `benchmark-v1` with `benchmark-longhorn-storage`, before enabling any
temporary replica storage. It holds until every permanent worker reports its huge pages,
so run `refresh-clients` first. The pinned fio 3.41 image runs eighteen 4 KiB direct
I/O trials on a disposable volume with three permanent CX43 replicas and client
worker 3: random read, random write and 70:30 mixed I/O, queue depths 1 and 32,
three 30-second trials after a 10-second ramp each. CPU limits, throttling, IOPS
and p99 latency are recorded. The receipt also captures the admitted client CPU
allocation and each permanent worker's actual instance-manager image, CPU
request and limit. Storage health is read every minute during the trials, and a scratch
replica rebuilt at any point is detected from its identity. Three unproven reads in a row,
from failed reads or a volume staying unhealthy, hold the comparison, so a shorter fault on
another volume can go unseen. Other changes during the trials hold it too. fio keeps running
until it finishes or reaches its deadline.

A receipt records these checks, and it does not prove nothing else disturbed the trials: only
rebuilds, volume health and the recorded configuration are observed. Scratch objects are excluded
from backups. Compare receipts knowing that, and avoid dispatching around the 02:17 UTC backup,
whose load on the same disks is not measured. V2's necessary CPU reservation is reported alongside the V1 context;
the comparison does not assume the engines have identical CPU costs.
A completed receipt requires healthy storage and
explicit cleanup of only the owned scratch Pod, claim, retained PV, backend,
class and namespace. An incomplete run, including one that reaches the Pod's 25 minute
deadline, holds with its journal and resources preserved for explicit recovery.

Run `benchmark-v2` only after every volume uses V2 and every replica is back on
the same three permanent workers. It requires the baseline's hardware, OS,
huge-page allocation and fio workload. Compare the three-trial medians and
latency alongside CPU throttling and concurrent platform load. These controlled
measurements do not establish a universal fourfold performance claim.

Each Kubernetes patch tests the observed UID and resource version, and Pod evictions
carry a UID precondition. Talos reads must report the same hardware UUID as the selected
Kubernetes worker before host configuration can be trusted or a host command can run.

## Benchmark retirement safeguards

Recovery builds per-run admission policies from all seven original resource UID receipts. One
protects the retained PV's original claim and backend handle; one keeps the original backend
detached after its Kubernetes status clears naturally; and one keeps the recorded engines,
replicas, frontends and attachment objects stopped under a fresh recovery checkpoint. These
policies permit normal stopped-state and finalizer updates, and reject replacement identities,
new attachments, restarts and changes to the original ownership chain.

The builders return unbound policies. A recovery workflow still needs complete dependency
inventories, activation proof on every serving API server, UID/resource-version checkpoint
writes, and direct instance-manager observations under stable controller and process identities.
The instance-manager registry is an observation of native runtime state, not an independent
disk or kernel audit. The builders and stopped-state checks alone do not authorize backend
retirement. Complete that recovery path before dispatching a benchmark.

## Prepare a permanent worker

`prepare-worker` rebuilds one permanent worker's local disk so it can hold V2 replicas. It wipes
that worker's `EPHEMERAL` partition and requires the separate permanent-worker safeguards. Nothing on the
cluster enables it yet: it holds with no write until the temporary V1 pool accepts replicas, the worker layout is loaded by Talos, the raw-disk defaults are
on the Node, huge pages are ready and Longhorn's V2 engine is on. Those land in later changes
(platform#5655).

Select `prepare-worker`, the worker, and `prepare-longhorn-v2-<worker>`, for example
`prepare-longhorn-v2-prod-worker-1`. The workflow shares the production deployment lock. It writes
a durable journal before requesting evacuation, waits for replica removal, drains through
Kubernetes disruption protections, and makes two complete observations before requesting the reset.
Both attached and retained detached volumes must have three healthy copies on distinct other nodes,
and no unregistered Longhorn data may remain on the worker.

Before the reset, the hardware identity, original `EPHEMERAL` UUID, storage fence and loaded layout
must still match. The only reset selector is `EPHEMERAL`; there is no disk-name wipe and no forced
drain. Recovery requires a changed `EPHEMERAL` UUID and separate, unlocked, state-locked encrypted
partitions of the reviewed sizes.

Every disk-removal retry rereads the owned storage node and requires the same disabled original
disk map and evacuation fence. A changed map or fence stops the procedure; an already empty map
needs no further write. Readiness callbacks and their host/API requests share the recovery
deadline, and a callback cannot establish readiness after that deadline expires.

A held preparation's own cordon does not fail the production deployment's node readback while its
journal names that exact worker and Node UID. The worker must still be Ready and on the desired
image, and any other cordon fails the deployment, so a deploy that may carry the recovery fix is not
evicted.

If a run fails, preserve its `longhorn-v2-prepare-<worker>` ConfigMap in `longhorn-system` and any
owned cordon. Diagnose the hold before dispatching a new first-attempt main run. A new run may adopt
the journal only after GitHub proves its previous workflow attempt is terminal. `reset-requested`
never replays the reset: an uncertain request must produce positive rebuilt state or remain held. If
the reset was never accepted, review a specific recovery rather than deleting the journal or trying
a rerun. A prepared receipt is revalidated against live state.

Prepared means the worker and its encrypted disk layout passed these checks. V2 activation, a
canary, backup and restore, and application acceptance remain separate gates.

Run the local checks with:

```sh
go test -race ./scripts/migrate-longhorn-v2 ./internal/longhornmigration ./internal/longhorninventory
actionlint .github/workflows/prepare-longhorn-v2-node.yaml
zizmor --offline .github/workflows/prepare-longhorn-v2-node.yaml
```

The Go checks make no cluster calls, but a clean module cache needs network access to the
configured Go module proxy and checksum database. This includes the native admission proof's
nested module at `testdata/retirement-fence-proof`; its committed `go.sum` is required. Populate
both modules' dependencies before running in a network-restricted environment.

The workflow's read-only test job runs on PRs and merge groups. Production credentials are
restored only for a confirmed manual dispatch from main. Host configuration and provider
responses are not printed into public logs.

## Read native replica directories

Select `inventory-replicas`, a permanent worker, and
`inventory-longhorn-replicas-<worker>`. This operation reads the fixed historical
V1 path `/var/lib/longhorn/replicas` through Talos's native List API. It does not
create a workload, execute a command on the node, change a storage record or reset
a partition. It shares the protected workflow's source, first-attempt and
production-lock boundaries.

Complete Kubernetes and Longhorn lists bind the selected worker's UID, hardware,
boot identity, address and filesystem disk registration to every placed replica,
including failed, inactive, stopped and terminating replicas. An unresolved
placement fails the observation. Every native directory is retained even when it
has no Replica or Orphan record. Missing expected directories are counted
separately. The historical fixed path is still inspected when its registration is
gone and no remaining API owner can be resolved there.

The operation requires two identical complete native listings, an unchanged API
projection, independently checked hardware and the actual kernel boot ID on both
sides. Transport errors, error envelopes, partial data, duplicate identities,
symlinks, unexpected file types, path escapes and exceeded response bounds fail
closed. The configured TLS context and one explicitly selected endpoint come only
from the workflow's protected in-memory Talos configuration; there is no default
configuration fallback.

Public output contains counts and SHA-256 commitments, with source and workflow
run identity. Directory names, replica identities and host addresses are not
printed. A failed observation reports a fixed stage name, including source,
identity, listing, consistency and close checks; provider error text is withheld.
This is a pair of consistent observations, not an atomic filesystem
snapshot, backup, proof of historical recoverability or permission to wipe. An
unmatched directory must be investigated and preserved before any rebuild. The
existing preparation operation still requires its separate preservation and
recovery gates; this inventory operation does not authorize it.

To inspect unused temporary capacity, select `temporary-v1-1`,
`temporary-v1-2` or `temporary-v2-1` with the same `inventory-replicas`
operation and `inventory-longhorn-replicas-<selection>` confirmation. These
are ordinal selections from the sorted, complete pool roster in the reviewed
`ksail.prod.yaml`, not hostnames. Both observations must retain every pool
member's identity and boot state. A changed roster holds the observation.

Temporary storage must still be fenced, unregistered and without placed
replicas. Its native listing must be empty and accounted for; unexpected
directories hold the observation. This can inform a later reviewed decision
to reuse capacity for V2. It changes no disk, pool, host setting or volume,
and does not authorize preparing a temporary worker through `prepare-worker`.
