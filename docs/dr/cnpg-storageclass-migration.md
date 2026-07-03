# Runbook: migrate a CNPG cluster to the WaitForFirstConsumer StorageClass

Moves a CloudNativePG (CNPG) cluster's storage from the legacy `longhorn` StorageClass
(`volumeBindingMode: Immediate`) to `longhorn-wffc` (`WaitForFirstConsumer`) with **no data
loss**. This is Phase 2 of the node-right-sizing work: `Immediate` binding provisions a
volume before its pod is scheduled, so CNPG replicas get stranded on whichever node the
volume landed on (including autoscaler overflow nodes), blocking scale-down. With
`WaitForFirstConsumer` the scheduler places the pod first (honouring the
restrict-storage-to-baseline-workers policy) and the volume follows.

## Why this is safe

CNPG keeps a full copy of the database on **every** instance (streaming replication). We
never touch data on the primary. We recreate one **replica** at a time: delete its PVC +
pod, and CNPG re-provisions that instance from scratch, re-cloning from the primary onto a
fresh `longhorn-wffc` volume. Only after every replica is migrated do we switch the primary
over and recreate the old primary the same way. Because a recreated replica's data is a
re-clone of the primary, the `reclaimPolicy: Delete` on the discarded old PVC is intended
and harmless — we are deliberately throwing away a redundant copy, never the last copy.

## Hard prerequisites (do not start until ALL are true)

1. **`longhorn-wffc` exists and is the default StorageClass** (platform#2421 promoted, merged,
   and deployed): `kubectl get sc` shows `longhorn-wffc (default)` and `longhorn` still present
   (non-default — CNPG clusters reference it by name until migrated).
2. **The cluster is fully healthy**: `readyInstances == instances`, all instances in
   `status.instancesStatus.healthy`, replication lag ~0.
3. **A fresh backup exists** (see `velero-cnpg.md` / `restore-drill.md`) taken within the last
   24h, and you have verified a restore drill at least once. This is the ultimate safety net.
4. Do this **one cluster at a time**, in ascending criticality:
   `umami-db` → `backstage-db` → `coroot-db` → `wedding-db` (user-facing — do last, ideally
   with the maintainer watching).

## Per-cluster procedure

Variables: `NS` = namespace, `CL` = cluster name (e.g. `NS=umami CL=umami-db`).

### 0. Pre-flight
```sh
kubectl -n $NS get cluster $CL -o jsonpath='{.status.readyInstances}/{.spec.instances} primary={.status.currentPrimary}{"\n"}'
kubectl -n $NS get cluster $CL -o jsonpath='{.status.instancesStatus}{"\n"}'   # all names under "healthy"
kubectl -n $NS get pods -l cnpg.io/cluster=$CL -o wide                          # note node + role of each
kubectl -n $NS get pvc -l cnpg.io/cluster=$CL -o custom-columns=PVC:.metadata.name,SC:.spec.storageClassName
```
Record the current primary. Confirm a recent backup. Abort if anything is unhealthy.

### 1. Point new PVCs at longhorn-wffc (declarative)
Edit the cluster manifest in git — set `spec.storage.storageClass: longhorn-wffc` (and
`spec.walStorage.storageClass` if the cluster defines separate WAL storage) — open a PR, and
let Flux deploy it. This changes **only newly-created** PVCs; existing instances are untouched
and stay up. Verify the spec is applied:
```sh
kubectl -n $NS get cluster $CL -o jsonpath='{.spec.storage.storageClass}{"\n"}'   # longhorn-wffc
```

### 2. Migrate each REPLICA, one at a time
Repeat for every instance that is **not** the current primary. Set `inst` to that replica's
name (e.g. `inst=umami-db-8`) so the block below is copy-pasteable:
```sh
inst=<replica-name>   # a NON-primary instance, e.g. umami-db-8

# a) delete the replica's PVC and pod together — CNPG recreates the instance on longhorn-wffc
kubectl -n "$NS" delete pvc "$inst" --wait=false
kubectl -n "$NS" delete pod "$inst"

# b) WAIT for CNPG to recreate and fully re-sync it before touching the next one
kubectl -n "$NS" get pods -l cnpg.io/cluster="$CL" -w                              # "$inst" returns, Running 1/1
kubectl -n "$NS" get pvc "$inst" -o jsonpath='{.spec.storageClassName}{"\n"}'      # MUST read longhorn-wffc
kubectl -n "$NS" get cluster "$CL" -o jsonpath='{.status.instancesStatus}{"\n"}'  # "$inst" back under "healthy"
kubectl -n "$NS" get pod "$inst" -o wide                                           # confirm a BASELINE worker
```
**Gate:** do not proceed to the next replica until `readyInstances == instances` again and the
recreated replica shows zero replication lag. If a replica fails to re-clone, the primary and
the other replicas are untouched — investigate or restore before continuing.

### 3. Migrate the primary last
Only after **every** replica is a healthy `longhorn-wffc` instance, move the primary off
`longhorn` by triggering a controlled switchover onto an already-migrated replica:
```sh
# Preferred — graceful switchover (requires the cnpg kubectl plugin: `kubectl krew install cnpg`):
kubectl cnpg promote "$CL" -n "$NS" <migrated-replica>

# Plugin-free alternative — delete the primary pod; CNPG promotes the most-advanced (already
# -migrated) replica. Expect a brief (seconds) write failover:
kubectl -n "$NS" delete pod <current-primary>

# Then confirm the new primary is a longhorn-wffc instance and recreate the OLD primary
# (now a replica) exactly as in step 2:
kubectl -n "$NS" get cluster "$CL" -o jsonpath='{.status.currentPrimary}{"\n"}'
```
CNPG flushes WAL and promotes an in-sync replica, so no committed data is lost. Only once the
primary is a `longhorn-wffc` instance do you recreate the former primary's PVC+pod.

### 4. Post-checks
```sh
kubectl -n $NS get pvc -l cnpg.io/cluster=$CL -o custom-columns=PVC:.metadata.name,SC:.spec.storageClassName
#   → every PVC now longhorn-wffc; no longhorn PVCs remain
kubectl -n $NS get pods -l cnpg.io/cluster=$CL -o wide     # none on autoscale-* nodes
kubectl -n $NS get cluster $CL -o jsonpath='{.status.readyInstances}/{.spec.instances}{"\n"}'
```
Run an application smoke test (write + read) against the DB. Only then move to the next cluster.

## Rollback

At any point before step 3, the primary is original and intact — revert the git `storageClass`
change and delete/recreate any half-migrated replica to restore it from the primary. The fresh
backup from the prerequisites is the last-resort recovery path (`restore-drill.md`).

## Notes

- `kubectl cnpg` plugin is **not** installed on the ops host; the raw `kubectl delete pvc/pod`
  flow above is the plugin-free equivalent of `kubectl cnpg destroy`.
- Longhorn volumes are replicated across storage nodes and are **not** Hetzner servers, so this
  migration does not consume hcloud server-quota slots.
- Non-CNPG longhorn PVCs (`kubescape-storage`, `headlamp`, `actual-budget`) can be migrated with
  the same delete-PVC-after-backup pattern **only** where the workload can tolerate recreation
  from a backup — they have no built-in replica to re-clone from, so back up first and treat each
  individually.
