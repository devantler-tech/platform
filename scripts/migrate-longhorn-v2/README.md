# Prepare Longhorn V2 workers

First-attempt manual runs on main join the ordinary production queue before
validation and keep that turn through the selected operation. Other events, refs
and attempts use separate groups and keep their existing operation refusals.
All boundary tests still run before production credentials
are restored. The original dispatch source must remain current: the production
lock does not lock Git refs, and a stale source still refuses the operation.

`inventory-v1-runtime` is a protected, target-free read-only inventory. Its exact
confirmation is `inventory-v1-runtime`, with empty `node` and `database`.
It observes the complete runtime inventory twice between current-main checks.
`declaredOverlap=FIT` means the observed CPU and memory requests fit one
additional V1 manager per storage node. CPU uses a conservative sum of init
requests and the actual per-node override or rounded global V1 percentage.
Memory uses the scheduler's peak init/sidecar accounting and observed namespace
default request. Complete LimitRange and ResourceQuota lists participate in the
same epoch. Unqualified defaults, quota or resource quantities leave the
calculation `UNVERIFIED`; an insufficient conservative budget reports `HOLD`.
Effective memory requests use exact Kubernetes quantities and whole-byte Pod
rounding. The budget also counts larger allocated or enacted Pod aggregate
requests, adding overhead once, and binds those values into the inventory epoch.
Container-level status must belong to its declared container and fit within its
declared request. A larger container allocation, active resize, malformed status
or node-resource claim leaves the calculation `UNVERIFIED`. The shared host-memory
admission uses the same conservative memory budget.
These are declared request budgets. Resident-memory measurements, reservations,
admission, serving health and rollout remain separate checks. The receipt keeps
`adoption=UNVERIFIED headroom=UNVERIFIED` and authorizes no storage operation.

The shared production publisher brackets KSail's unchanged source inputs before
staging and before promotion. Its separate read-only publication guard inspects
the effective production layers and refuses indirect or unknown runtime-image
selection. The first selection of the reviewed retirement image requires a fresh
stable native epoch with complete declared overlap capacity, and an isolated
protected-queue change adding only the reviewed image/rolling-strategy component
to the production controllers root and nothing else. The Longhorn release has no
authorization-ledger entry (#6270), so the activation changes no approval.
An unchanged already-selected image needs a stable native epoch rather than a
second stock-image overlap calculation. Initial selection from manual CD or a
recovery checkout is held because those checkouts do not prove an isolated
deployed baseline. A source rollback to the stock profile remains publishable.

Initialized disaster recovery may have complete empty Volume, Engine and Replica
lists. Those are valid observations; failed reads and missing or partial lists
remain held. Positive node, runtime and process inventories are still required.
The guard proves producer-input consistency, not an independent OCI layer readback
or runtime adoption. Serving-image adoption, resident memory, rollout health and
benchmark/scratch retirement remain separate acceptance checks.

`observe-database` is a protected, read-only PostgreSQL observation for
platform#5842. Select one of the eight fixed databases with `database`, leave
`node` empty, and confirm with `observe-longhorn-v2-database-<database>`.
It checks all expected healthy, owned instances, the stable primary, and the
operator health conditions. The preview database has one expected instance.
Fixed SQL reads physical database identity, timeline and WAL positions. Each
standby must have replayed the primary's captured flush position. The
client uses the Pod's fixed local socket with a cleared connection environment
and fixed executable search path, rather than inheriting libpq overrides.
It qualifies PostgreSQL built-ins explicitly and fixes catalog lookup and
read-only transaction defaults for the connection. Persisted role or database
settings cannot redirect this physical-state query through a shadow function.
Complete namespace Pod inventories and process identities bracket the reads,
including a final whole-cluster read after the last SQL command. The operation
prints only a success summary, with no application rows, credentials or physical
identities. Changed source, cluster, process or replication evidence holds the
operation. This observation neither proves backup recovery nor authorizes
replacement, promotion, storage retirement or application cutover.

`repair-v1-reservation` selects the fixed original worker and requires
`repair-longhorn-v1-reservation` on a first-attempt main dispatch. It admits the
two identified missing V1 copies only when both surviving copies, the complete
disk allocation ledger and the physical free-space floor agree. The only write
adjusts the filesystem disk reservation by the required logical capacity.
Success requires all volumes to regain three healthy copies with the original
affected volume and disk identities.

`reserve-v1-scratch` selects the same worker and requires
`reserve-longhorn-v1-scratch` on a first-attempt main dispatch. Run it after
the original volumes have three healthy copies. It qualifies one 4 GiB V1
scratch claim across the three permanent disks under the existing scheduling
settings and keeps an additional 4 GiB above the physical free-space floor.
It adjusts the limiting disk's reservation by only the remaining logical
shortfall and reads back the original disks, volume identities and available
capacity. An uncertain write stops
the run. Check the live reservation and storage health before a new dispatch.
Use the qualified capacity for one recovery or benchmark claim at a time.

`reserve-v1-backup-scratch` requires `reserve-longhorn-v1-backup-scratch` on a
first-attempt main dispatch with no worker or database selection. Run it after
the active backup and its staging claims have cleared. The operation qualifies
10 GiB for a temporary backup replica on each original V1 disk under the
existing physical free-space floor and a further 4 GiB margin. It changes only
the measured reservation shortfall and verifies the disks and original
allocations after each write. Backup acceptance requires a successful upload
and a representative restore.

`inventory-storage-layout` adds read-only native block metadata to the storage-root
observation for a fixed temporary worker. Confirm it with
`inventory-longhorn-storage-layout-<selection>`. Two complete, bounded native volume
status and physical-discovery collections must agree inside the unchanged node,
hardware and boot bracket. Its schema-2 observation-only receipt reports sizes,
phases, encryption properties and counts, while withholding device paths and
identities. It grants no reset clearance and never treats a failed read as absence.
The original root-only mode keeps its existing scope and schema.

`inventory-provider-volume-footprint` reads the current provider-backed claims
and native Talos filesystem usage from their running mounts. Confirm with
`inventory-provider-volume-footprint` on a first-attempt main dispatch.
It binds each running CSI mount to its provider disk and checks the claim,
volume, disk and worker identities on both sides of the read. The receipt reports aggregate GiB rounded up, with
unmounted claims accounted for at their full requested capacity. Use the result
as an input to capacity planning and validate each restore before cutover.

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
volumes fully redundant, and all seven database clusters healthy. It records the
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

New version-3 journals bind all seven production PostgreSQL clusters and their
full declared replica counts, including the two-instance identity and portal
databases. Every cluster must recover before a reboot. Version-1 and version-2
journals remain readable with their original three-owner and five-owner scopes.
A record that has yet to request a reboot must freshly bind the current roster
before moving the worker, preserving each original identity. Post-request
recovery retains the original receipt's scope.

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
Each release PATCH uses a fresh Node identity and resource version after the
native safety checks. A failed write response is confirmed only by the exact
intended state on the same Node and journal identities; the write is never
repeated. A deleting journal cannot authorize recovery. Fixed stage diagnostics
identify the failed boundary without publishing private command replies.
Dispatch each worker separately and verify platform recovery before the next one.
An interrupted run keeps its journal and cordon; a new first-attempt main dispatch
may adopt it only after the previous run has ended. This operation enables no V2
storage and removes no iSCSI support.

The `Prepare Longhorn V2 Migration` workflow runs guarded steps towards
platform#5582. Application cutover, recovery probes, V1 retirement and iSCSI
removal have separate acceptance gates.

This tool lands in stages (platform#5655). It includes `refresh-clients`, `prepare-worker`,
`prepare-unused-temporary-worker`, read-only replica inventories and the benchmarks.
`refresh-clients` holds until the fixed migration workers are Ready and the host
huge-page settings are loaded. Host recovery and a cleaned V1 baseline precede
temporary-disk preparation. Permanent-worker preparation is a dormant fallback
(platform#5582); the chosen route keeps temporary V1 replica capacity disabled.
V2 activation requires completed preparation on the temporary workers.

## Temporary V2 activation

The activation manifests retain V1 and the existing default StorageClass, enable
V2, and add the opt-in `longhorn-v2` Retain class. Eight-CPU workers reserve two
cores for polling SPDK reactors with mask `0x3`, CPU isolation, and 2048 MiB of
huge pages. The CPU-only LimitRange rises to four cores so management and network
work have headroom above those reactors; it adds no memory limit. The reviewed
Longhorn 1.13.0 instance-manager image and the admitted client resources and
arguments must match before registration. Enabling the setting proves neither
raw-disk readiness nor application recovery.

After all three temporary workers have completed their independently confirmed
raw-layout preparation, select `register-temporary-v2-storage` and confirm
`register-longhorn-v2-temporary-storage`. This operation always selects all three
fixed temporary pool members. It requires intact original V1 replicas, healthy
database quorums, the cleaned baseline, reviewed storage capacity, no competing
maintenance, and ready V2 clients on all six workers before its first write.
Every selected worker must present its own 144 GiB encrypted raw partition. The
complete volume inventory must fit on each disk with the working reserves and
free-space margin available.

The journal `longhorn-v2-temporary-activation` binds each temporary selection to
the node, provider, hardware, boot, encrypted EPHEMERAL and raw GPT partition
identities. Complete native reads bracket the partition observations. Each rebuilt
worker also binds its completed preparation journal UID and the successful
first-attempt main workflow that produced it. The raw GPT identity must match
the one recorded during preparation. For older V1-worker preparation journals
that lack this field, supply `legacy_preparation_receipts` once during initial
registration. Use a comma-separated list of `selection:digest` values, with
each SHA-256 computed from five newline-separated fields with no final newline.
Use the selected profile, original preparation source SHA, successful run ID,
original native-empty digest, and lowercase raw GPT UUID from the independently
retained post-preparation readback. Re-reading the disk immediately before
activation cannot establish that historical identity. The registration journal
preserves the accepted binding for later verification. The third worker needs
a completed preservation receipt and a new
encrypted staging partition with a GPT identity distinct from its original raw
partition. Each conditional Longhorn Node update registers the selected worker's
reviewed mapper and adds the journal UID as its ownership annotation. Matching
unowned disks are refused.
An uncertain response stops the run. A new first-attempt main dispatch may resume
an owned partial registration only after the previous workflow is terminal, and
it revalidates all three native bindings and every remaining prerequisite.
Completion requires three independent ready raw disks and all qualified clients;
it grants no application cutover or original-disk reset authority.

The two rebuilt workers retain their original `migration-pool=v1` allocation
label. Production's replica-exclusion policy keeps other autoscaled workers
fenced. Before the V2 restore canary, the protected migration check verifies
the three raw disks against the completed activation journal, its ownership UID
and the current Kubernetes, provider, hardware and Longhorn node identities.

The journal retains the original boot and OS observations for audit. Later
registration recovery and acceptance compare its durable worker, partition and
preparation identities, while each native read still requires an unchanged live
boot bracket. A normal reboot does not adopt replacement hardware or storage.

Filesystem and CSI recovery probes follow separately under platform#5809.
They must verify restored bytes, persistence after reattachment and owned scratch
cleanup before any application cutover. This activation operation has no data
movement or probe entrypoint. All original application volumes and historical
recovery states remain protected.

For the unused temporary-worker route (platform#5787), finish the client refresh and the
cleaned V1 baseline first. Select `prepare-unused-temporary-worker`, `temporary-v1-1` or
`temporary-v1-2`, and the matching confirmation `prepare-unused-longhorn-v2-temporary-v1-1`
or `prepare-unused-longhorn-v2-temporary-v1-2`. Each selection is resolved from the complete,
reviewed two-member V1 pool and one-member V2 pool. This preparation operation
admits only the two V1-born workers.

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

For new preparations, the native original partition must show at least 200 GiB. After drain
and a fresh empty-storage read, the workflow applies the 144 GiB raw profile without rebooting
the selected worker. It checks the loaded configuration and repeats the native empty-storage
read before recording reset intent. The profile retains a 40 GiB minimum for EPHEMERAL and
keeps both encryption keys locked to STATE. Existing journals retain their recorded geometry.

An uncertain reset response never causes a second reset. A new boot and encrypted EPHEMERAL UUID,
the exact encrypted raw size bound by the journal, loaded configuration and huge-page readiness
must be observed before releasing the owned cordon. If a deployment reapplied the standard
configuration, recovery can restore the staging profile in place only after verifying the
rebuilt disk, the owned cordon and capacity pin, and current storage safety. Recovery reads
the restored configuration before releasing the worker. CPX42 workers use a separate profile
with more space for the root filesystem. The raw disk remains unregistered and fenced;
storage activation and volume migration follow separately. Original storage replicas remain on
the permanent workers throughout this preparation route.

For the V2-born worker, use `prepare-retained-temporary-worker`,
`temporary-v2-1` and `prepare-retained-longhorn-v2-temporary-v2-1`.
The protected operation verifies the original 96 GiB raw partition and STATE
extents before draining. It accepts either the original profile with no
staging declaration or the complete distinct 144 GiB staging profile and a
40–44 GiB EPHEMERAL range. After the owned drain, it loads the reviewed
staging profile without a reboot if needed. A partial profile or an
unconfirmed write stops before the original mapper is closed. On resumption
it reads back the layout and retained extents before proceeding.
It records reset intent only after complete native reads show the old raw
configuration, volume status and partition holders cleared while the physical
extents remain.

After the EPHEMERAL-only reset, the worker must report a new boot, a replaced
encrypted EPHEMERAL partition and a separate encrypted staging partition.
The workflow reopens the original mapper only after rechecking the retained
extents. It requires the same extents and an open original mapper before
recording preparation complete. A lost reset response permits observation
of recovery only. The original mapper remains outside Longhorn scheduling.

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

Dispatch a refresh while the platform merge queue is empty, and hold the agent lanes' own
platform enqueues until the run ends. Prefer 00:00–06:00 UTC outside the 02:17 UTC backup.
The refresh checks current main before cordoning, every eviction attempt and the kubelet
restart. If main moves before a restart attempt, the refresh releases only its own cordon.
An unproven restart keeps the cordon for a new first-attempt main run to adopt after the
earlier run finishes. A proven restart follows the normal release path.

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
A completed receipt requires healthy storage and explicit cleanup of the recorded
scratch Pod, network policy, claim, retained PV, backend and class. Both phases use
the GitOps-managed `longhorn-benchmark` namespace. The workflow records its UID and
leaves the namespace in place. An incomplete run, including one that reaches the
Pod's 25-minute deadline, holds with its journal and resources preserved for explicit recovery.

Run `benchmark-v2` only after every volume uses V2 and every replica is back on
the same three permanent workers. It requires the baseline's hardware, OS,
huge-page allocation and fio workload. Compare the three-trial medians and
latency alongside CPU throttling and concurrent platform load. These controlled
measurements do not establish a universal fourfold performance claim.

Each Kubernetes patch tests the observed UID and resource version, and Pod evictions
carry a UID precondition. Talos reads must report the same hardware UUID as the selected
Kubernetes worker before host configuration can be trusted or a host command can run.

## Benchmark retirement safeguards

The V1 benchmark requires the exact reviewed retirement image, including its
release tag and immutable digest. The default setting and each selected manager
declaration must agree with that image; a matching tag shape, older publication
or digest-only declaration does not qualify. Containerd may report the approved
running image by repository and digest without its tag. Publication and admission
rules alone do not prove that this runtime has been adopted.

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

`recover-unstarted-v1-benchmark` recovers a stopped workflow whose fio Pod
never scheduled and whose claim never bound. Leave `node` and `database`
empty and confirm `recover-unstarted-longhorn-v1-benchmark`. The procedure
verifies the latest original workflow attempt is terminal, reads back the
original objects and refuses any PV, backend or attachment. It uses the
observed resource versions to remove only the recorded scratch assets.
The original journal is archived in an immutable ConfigMap. The protected
workspace remains available for a fresh benchmark.

`recover-bound-v1-benchmark` handles an unmeasured V1 run whose original
fio Pod failed after its retained scratch claim bound. Leave `node` and
`database` empty and confirm `recover-bound-longhorn-v1-benchmark:<run-id>`.
The workflow verifies the original workflow is terminal, archives its
unchanged run evidence, and removes only its recorded scratch objects. It
waits for native detach and PV release and uses the same per-API admission
fences and instance-manager seals as normal V1 cleanup. A missing asset
requires a matching deletion intent. An ambiguous fence creation or a changed
runtime epoch holds recovery. A cleaned failure is never a usable baseline.

New V1 benchmarks use the first permanent worker and check its free requested
memory before creating scratch resources. The V2 comparison uses the worker
recorded in its completed V1 baseline. Existing baselines retain their original
third-worker placement.

## Disposable recovery source

`prepare-native-v1-seed`, confirmed with `prepare-longhorn-native-v1-seed`,
prepares only the source half of a filesystem recovery canary. Leave `node` and
`database` empty. It requires the cleaned V1 baseline, three healthy copies of
every Longhorn volume, the original approved retirement-manager epochs, and an
already available four-GiB scratch budget. The source Pod's memory request must
fit on the first permanent worker before scratch creation. The procedure changes
no reservation.

The fixed `longhorn-benchmark` workspace must be empty. The daily Velero schedule
explicitly excludes it. Other schedules may instead select only named, unrelated
namespaces with cluster-scoped resources disabled and no resource policy. The source Pod and
claim opt in only their `data` filesystem volume for this run's separately
selected Backup. The procedure writes and verifies a 128-MiB payload, sentinel
and checksum. It refuses restored, cloned, encrypted or differently bound
source storage.

The existing default Velero destination must be Available, ReadWrite and recently
validated. No Longhorn backup target or credentials are added. A seven-day Backup
selects only this run's Pod and claim, with CSI snapshots, data movement and
cluster resources disabled. Native completion, exactly one complete Kopia
transfer, its source Pod UID, snapshot ID and the original Ready repository UID
and spec are required. A completed Backup by itself does not establish data
transfer. The same evidence is checked again before source retirement and after
local cleanup.

`longhorn-v2-native-seed` retains the original resource identities and each
conditional phase transition. Uncertain creates or journal writes stop without
retrying or adopting objects by name. Cleanup uses the V1 benchmark's original
runtime, serving-API fences and physical retirement proof, then removes the run's
replica tags. It leaves the remote backup and workspace intact. An interrupted
run preserves its journal and assets for a separately reviewed recovery path.

`SOURCE_CLEANED` confirms the retained V1 backup and retirement of its source.
After temporary V2 registration completes, run `restore-native-v2-seed` with
the confirmation `restore-longhorn-native-v2-seed`. Leave `node` and
`database` empty. The first-attempt main workflow binds the three registered
raw disks to their original preparation and activation records and provisions
a fresh V2 claim.

Velero restores the original Kopia filesystem snapshot to that claim. The
workflow verifies the original payload checksum, writes a fresh token, and
detaches the first consumer. It reattaches the same claim on a second qualified
worker and verifies both payloads. It retains the backup, journal and canary
resources for readback. An uncertain write holds the run for explicit recovery.
Application cutover and performance measurement have their own acceptance steps.

When a restore stops at `restore-owned` after a complete Velero transfer, use
`resume-native-v2-seed-restore` with `resume-longhorn-native-v2-seed:<original run ID>`
and leave `node` and `database` empty. The protected main workflow requires the
original failed first attempt, the retained backup, and the same healthy V2 claim
and first consumer. It records one continuation on the original journal, then
verifies the payload and reattachment. Preserve both journals and the canary
if the continuation stops.

If the original Pod reached its deadline after the completed transfer, the
same continuation can replace that disposable Pod after proving the original
V2 volume is detached. It retains the original Velero transfer and Pod
identities and verifies the recovered bytes on the replacement before moving
the same claim to a different worker.

The workflow checks memory headroom when choosing the reattachment worker.
If the recorded second Pod remains unassigned because of insufficient memory
and its first workflow attempt fails, use `resume-native-v2-reattach` with
`resume-longhorn-native-v2-reattach:<original run ID>:<failed continuation run ID>`.
Leave `node` and `database` empty. The protected operation verifies the failed
executor and the never-started Pod, replaces that test Pod and checks the
original payload and previously written marker on the remaining qualified worker.

After independently verifying the successful restore and reattachment, select
`reclaim-native-v2-seed` and confirm it with
`reclaim-longhorn-native-v2-seed:<original restore run ID>`. Leave the node and
database fields empty. The operation binds the original off-cluster seed and
the successful first attempt of the restore or its recorded continuation. It
waits for the canary consumer to leave and for the backend to detach. It verifies
admission on every serving API before changing the bound disposable PV's reclaim
policy.

The original claim is removed only after the CSI provisioner has installed its
deletion finalizer. Verify the PV, backend, replicas and attachment references
have retired. The operation removes the canary's policies, class and workload
tags and verifies the seed backup again. The restore and reclaim journals and
the object-store restore logs provide the audit record. If any action cannot be
confirmed, inspect the reclaim journal and the exact recorded resources before
recovering it.

## Prepare a permanent worker

`prepare-worker` rebuilds one permanent worker's local disk so it can hold V2 replicas. It wipes
that worker's `EPHEMERAL` partition and requires the separate permanent-worker safeguards. It is the
fallback route (platform#5582) and nothing on the cluster enables it: it holds with no write until the temporary V1 pool accepts replicas, the worker layout is loaded by Talos, the raw-disk defaults are
on the Node, huge pages are ready and Longhorn's V2 engine is on. The chosen route reuses the unused
temporary workers instead (platform#5787, platform#5809) and never enables V1 replicas on them.

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
go test -race ./scripts/migrate-longhorn-v2 ./internal/longhornmigration ./internal/longhorninventory ./internal/namespaceinventory
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

## Retain one V1 source before replacing its claim

Use `retain-v1-source` with no worker selection as part of an application
migration. Read the bound application PVC, its PV and the V1 Longhorn volume.
Compute a SHA-256 digest, with no trailing newline, over the colon-separated
values `namespace/claim:claim-uid:pv-name:pv-uid:volume-name:volume-uid`. Supply
`retain-longhorn-v1-source:<namespace>/<claim>:<digest>` as the confirmation.
The digest lets the protected workflow recheck the same identities without
passing UIDs to GitHub Actions.

The workflow changes only the confirmed PV's reclaim policy from `Delete` to
`Retain` and reads the three original identities back. It excludes backup movers,
migration scratch and unbound claims. A lost write response is checked without
repeating the patch. This operation neither replaces claims nor changes storage
classes. Keep the original PV and backend until the application's recovery and
cutover checks pass.

## Retain one Hetzner source before replacing its claim

Select `retain-provider-source` with no worker. Read the original bound
Filesystem PVC and PV plus their `hcloud` StorageClass. Compute a SHA-256
digest without a trailing newline over
`csi.hetzner.cloud:namespace/claim:claim-uid:pv-name:pv-uid:volume-handle:pv-capacity-bytes`.
Supply `retain-hetzner-source:<namespace>/<claim>:<digest>` as confirmation.

The fixed migration namespaces are `kube-system`, `observability`, `openbao`,
`customer-identity` and `business-site`. The provider footprint reader uses the
same namespace set, including the identity and portal databases. Each retention
request still binds one original claim, PV and provider handle; a digest from a
different namespace cannot authorize it.

The workflow verifies the original binding and provider handle, changes
only that PV's reclaim policy, and reads the same binding back. Keep the
retained provider volume until the workload has passed its V2 recovery and
application checks.
