# Prepare a Longhorn V2 worker

The `Prepare Longhorn V2 Migration` workflow enables protected temporary capacity
or evacuates and rebuilds one permanent storage worker. It is part of platform#5582; it does not migrate a claim, enable
V2, disable V1, remove iSCSI, or establish a performance improvement.

The production worker configuration reserves a 96 GiB encrypted raw partition
on the system disk. EPHEMERAL keeps at least 40 GiB, leaves 104 GiB for the raw
partition and headroom, and retains its existing state-locked LUKS2 encryption.
Existing EPHEMERAL partitions do not shrink when this configuration is applied.
The workflow resets only EPHEMERAL after evacuation, retaining STATE, its
encryption salt, and every CSI-attached disk.

Before either evacuation or reset, the complete measured volume inventory must
fit the 96 GiB raw-disk budget: 4 GiB reserved, 2 GiB metadata margin, one 4 GiB
probe at a time, and the current 25% free-space floor. Missing sizes hold. The
last V1 worker cannot begin rebuilding while any V1 backend remains. Rejoined
workers must also report clear disk, memory and PID pressure conditions.

During a permanent-worker reset, three static workers, three control planes and
the three migration-pool workers must be Ready, with no compute overflow node.
The V1 pool is fixed at two workers, and the V2 pool at one. Each migration node
must carry this procedure's owned `scale-down-disabled` annotation. The temporary
V1 workers preserve three copies of every volume while permanent workers are
rebuilt. Prepare the first two permanent workers before moving data to V2; the
fresh temporary raw-disk worker provides the third V2 target. Keep the remaining permanent V1 worker until every
V1 volume, including retained recovery volumes, has been migrated or explicitly
retired with verified recovery evidence. Then prepare that worker, move replicas
from temporary capacity, and retire temporary workers in a later reviewed step.

Dispatch only the merged workflow on `main`. First select `refresh-clients` with
`refresh-longhorn-v2-clients`. It verifies the live host allocation and modules,
restarts only kubelet where capacity is stale, and requires every worker to report
2 GiB of 2 MiB huge pages. Containers and disks are preserved. A restart holds while the
worker's Pods request more memory than would remain after huge pages, because
kubelet would reject the excess outright; move those workloads off first.

Next select `benchmark-v1` with `benchmark-longhorn-storage`, before enabling any
temporary replica storage. The pinned fio 3.41 image runs eighteen 4 KiB direct
I/O trials on a disposable volume with three permanent CX43 replicas and client
worker 3: random read, random write and 70:30 mixed I/O, queue depths 1 and 32,
three 30-second trials after a 10-second ramp each. CPU limits, throttling, IOPS
and p99 latency are recorded. The receipt also captures the admitted client CPU
allocation and each permanent worker's actual instance-manager image, CPU
request and limit. Missing observations or changes during the trials hold the
comparison. V2's necessary CPU reservation is reported alongside the V1 context;
the comparison does not assume the engines have identical CPU costs.
A completed receipt requires healthy storage and
explicit cleanup of only the owned scratch Pod, claim, retained PV, backend,
class and namespace. An incomplete run holds with its journal and resources
preserved for explicit recovery.

Select `enable-capacity`, a pool,
and `enable-longhorn-migration-capacity` to activate temporary storage. The whole
pool is checked and pinned before any disk is enabled. Its role, server type,
hardware and encryption must match; an existing foreign pin or disk is a hold.
New pool nodes remain excluded by admission until explicitly enabled. The V2 pool also holds until Longhorn's V2 data engine setting is on, because Longhorn refuses a block disk otherwise. Existing
client-readiness checks independently verify the fixed pool source and live pins
before accepting any replica there. Ordinary compute nodes stay excluded.
Activation requires the completed, cleaned baseline from a terminal main
workflow and matching permanent hardware and OS identities.

Run `benchmark-v2` only after every volume uses V2 and every replica is back on
the same three permanent workers. It requires the baseline's hardware, OS,
huge-page allocation and fio workload. Compare the three-trial medians and
latency alongside CPU throttling and concurrent platform load. These controlled
measurements do not establish a universal fourfold performance claim.

For a permanent worker, select `prepare-worker`, its node name, and
`prepare-longhorn-v2-node`. The workflow shares the production deployment
lock. It requires the current main source, the reviewed layout already loaded by
Talos, the reconciled block-disk defaults, every temporary V1 worker enabled for
replicas, and a Ready target without a foreign cordon. It writes a durable journal before requesting evacuation, waits for
replica removal, drains through Kubernetes disruption protections, and makes two
complete observations before requesting the reset. Both attached and retained
detached volumes must have three healthy copies on distinct other nodes.

Each Kubernetes write tests the observed UID and resource version. Talos reads
must report the same hardware UUID as the selected Kubernetes worker before
host configuration can be trusted or a host command can run. Benchmark journals
retain each scratch resource's exact name, namespace and UID as it is created,
including both the PV and backend before I/O is released. A held preparation's own cordon does not fail the production deployment's node
readback while its journal names that exact worker; the worker must still be Ready
and on the desired image, and any other cordon fails the deployment. Interrupted work keeps
those identities for explicit recovery.

Before reset,
the hardware identity, original EPHEMERAL UUID, storage fence and desired layout
must still match. The only reset selector is `EPHEMERAL`; there is no disk-name
wipe and no forced drain. Recovery requires a changed EPHEMERAL UUID and separate,
unlocked, state-locked encrypted partitions of the reviewed sizes.

If a run fails, preserve its `longhorn-v2-prepare-<worker>` ConfigMap in
`longhorn-system` and any owned cordon. Diagnose the hold before dispatching a
new first-attempt main run. A new run may adopt the journal only after GitHub
proves its previous workflow attempt is terminal. Source, hardware and resource
identities are rechecked. `reset-requested` never replays the reset: an uncertain
request must produce positive rebuilt state or remain held. If the reset was
never accepted, review a specific recovery rather than deleting the journal or
trying a rerun. A prepared receipt is revalidated against live state.

Prepared means the worker and encrypted disk layout passed these checks. Every
V2 client node must advertise its required 2 GiB hugepage capacity before activation. V2
instance-manager health, a successful three-replica canary, persistence,
reattachment, backup/restore, application acceptance and comparable V1/V2 I/O
measurements remain separate gates before data migration or iSCSI retirement.

Run offline checks with:

```sh
go test -race ./scripts/migrate-longhorn-v2
go test -race ./internal/longhornmigration ./scripts/maintain-longhorn-clients
bash scripts/tests/test-longhorn-migration-replica-exclusion.sh
actionlint .github/workflows/prepare-longhorn-v2-node.yaml
zizmor --offline .github/workflows/prepare-longhorn-v2-node.yaml
```

The complete Go regression job and the workflow's read-only test job run on PRs
and merge groups. Production credentials are restored only for a confirmed
manual dispatch from main. Host configuration and provider responses are not
printed into public logs.

References: [Longhorn eviction](https://longhorn.io/docs/1.13.0/nodes-and-volumes/nodes/disks-or-nodes-eviction/),
[default disks](https://longhorn.io/docs/1.13.0/nodes-and-volumes/nodes/default-disk-and-node-config/),
[Talos raw volumes](https://docs.siderolabs.com/talos/v1.13/configure-your-talos-cluster/storage-and-disk-management/disk-management/raw).
