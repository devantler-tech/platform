# Prepare Longhorn V2 workers

The `Prepare Longhorn V2 Migration` workflow runs guarded steps towards
platform#5582. It does not migrate a claim, enable V2, disable V1 or remove iSCSI.

This tool lands in stages (platform#5655). Today it has `refresh-clients`, `prepare-worker` and the
benchmarks. `refresh-clients` holds until the V2 migration pool is in
`ksail.prod.yaml`, the fixed migration workers are Ready and the host huge-page
settings are loaded. Worker preparation is below and is dormant; temporary capacity follows in its own change.

Dispatch only the merged workflow on `main`. Select `refresh-clients` with
`refresh-longhorn-v2-clients`. It verifies the live host allocation and modules,
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

## Prepare a permanent worker

`prepare-worker` rebuilds one permanent worker's local disk so it can hold V2 replicas. It wipes
that worker's `EPHEMERAL` partition, so it is the one destructive step in this tool. Nothing on the
cluster enables it yet: it holds with no write until the V2 pool is in `ksail.prod.yaml`, the
temporary V1 pool accepts replicas, the worker layout is loaded by Talos, the raw-disk defaults are
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

Run offline checks with:

```sh
go test -race ./scripts/migrate-longhorn-v2 ./internal/longhornmigration
actionlint .github/workflows/prepare-longhorn-v2-node.yaml
zizmor --offline .github/workflows/prepare-longhorn-v2-node.yaml
```

The workflow's read-only test job runs on PRs and merge groups. Production credentials are
restored only for a confirmed manual dispatch from main. Host configuration and provider
responses are not printed into public logs.
