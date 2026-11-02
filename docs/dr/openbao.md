# OpenBao Secrets Vault — Disaster Recovery

## Overview

OpenBao runs as a raft (Integrated Storage) cluster. Three artifacts make it
recoverable:

1. **Raft snapshots** — the `vault-snapshot` CronJob saves a
   `bao operator raft snapshot` daily (newest 14
   kept) and mirrors them to the S3 backup target under `openbao-snapshots/`
   (Cloudflare R2 in prod, MinIO locally). The mirror exists because Velero's
   file-system backup only captures volumes mounted by *running* pods —
   nothing mounts this PVC outside the CronJob's brief run, so Velero alone
   would never carry the snapshots off-cluster.
2. **`openbao-unseal` Secret** — unseal key + root token, captured by Velero's
   daily resource backup. A snapshot is only usable together with the keys
   that were current when it was taken.
3. **The `vault-config` Job** — auto-initializes OpenBao on fresh clusters,
   auto-unseals on restarts, and **auto-restores**: when no pod reports an
   initialized barrier but `openbao-unseal` still holds keys AND a snapshot
   is fetched from the off-cluster mirror into its scratch volume, it
   temp-initializes, runs
   `bao operator raft snapshot restore -force` with the newest snapshot, and
   unseals with the stored key — no operator action. Only when no snapshot is
   available does it abort and demand explicit data-loss acknowledgement
   (the #1982 guard, unchanged).

The snapshot fetcher uses a pinned MinIO client and pinned static shell tools
installed before it starts. It selects the newest `.snap` object by name and
downloads only that object. Missing projected credentials or an empty or
unreachable mirror listing leave the scratch volume empty. When recovery needs
a snapshot and none is available, the restore guard refuses re-initialization.

## Snapshot storage and verification

Production's initial and scheduled snapshot jobs use the retained
`vault-snapshots-recovery` claim. The original `vault-snapshots` claim remains
protected and unmodified. Local deployments keep their original claim. Active
claims must bind; the temporary preparation marker no longer bypasses readiness.

The snapshot container completes before the mirror starts. The mirror copies
without remote removal, then downloads each nonempty snapshot and compares its
SHA-256 with the local bytes. A failed or partial GET fails the run, including a
GET that delivers a complete body and then returns an error. Production requires
the projected backup credentials; an empty source also fails. Fresh local
bootstrap can report `VAULT_SNAPSHOT_REMOTE=UNSEEDED` until credentials arrive.

The fixed `VAULT_SNAPSHOT_REMOTE=VERIFIED count=N` verdict requires matching
downloaded bytes for every snapshot and successful age-based retention. Retention
starts after upload and readback; the mirror never uses `--remove`. Credentials,
client configuration and copied static tools use bounded memory-backed volumes.
The existing concurrency exclusion, execution deadlines and snapshot resources
remain in place.

Only the snapshot init container receives the explicitly projected Kubernetes
token used for its existing OpenBao login. Automatic token mounting is disabled;
the static-tool installer and remote mirror receive no Kubernetes identity.

Complete recovery acceptance with an isolated restore of the new off-cluster
snapshot and verification of its credential records using the matching recovery
keys. Keep #5376 open until that native restore proof is observed, before moving
Email credentials or retiring legacy custody.

## Recovery Scenarios

### Scenario 1: Single pod restart

**Symptom**: OpenBao pod was evicted or restarted; vault shows `sealed: true`.

**Resolution**: The `postStart` hook auto-unseals using the `openbao-unseal`
Secret (created by the `vault-config` Job on first init, backed up by Velero).
No manual action needed. Verify:

```bash
kubectl exec -n openbao openbao-0 -- bao status
```

### Scenario 2: Raft data corruption or loss (Secret + snapshots intact)

**Symptom**: OpenBao fails to start with storage errors, or all pods report
`initialized: false` while `openbao-unseal` still exists (the 2026-06-10
incident shape).

**Resolution** — automated. Reset the data volumes and let the `vault-config`
Job restore the newest snapshot:

1. Scale down the StatefulSet:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=0
   ```
2. Delete the corrupted data PVCs (NOT `vault-snapshots`, and do NOT delete
   the `openbao-unseal` Secret — both are the restore inputs):
   ```bash
   kubectl delete pvc -n openbao data-openbao-0 data-openbao-1 data-openbao-2
   ```
3. Scale the StatefulSet back up to its three servers, which start with empty
   volumes:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=3
   ```
   Flux does not restore the count: its drift detection ignores StatefulSet
   replica counts, so a reconcile leaves the vault at zero servers.
4. Trigger Flux reconciliation (`ksail workload reconcile`) — the
   `vault-config` Job detects uninitialized-pods + surviving-keys +
   available-snapshot and restores automatically (worst-case RPO: 24 h, the
   snapshot cadence).
5. ExternalSecrets resume syncing; PushSecrets top up anything newer than the
   snapshot on their next refresh.

Only if **no snapshot exists** (PVC also lost and the R2 mirror is empty) does
the Job abort with the data-loss guard; acknowledge the loss explicitly with
`kubectl delete secret openbao-unseal -n openbao` and the next run
re-initializes from scratch (Scenario 4 then re-seeds the KV).

### Scenario 3: Full cluster rebuild (backups available)

**Symptom**: Entire cluster is lost (DR scenario); the R2 snapshot mirror
and/or Velero backups are available.

**Sequencing caveat**: on a rebuilt cluster Flux stands OpenBao up *before*
any restore can run, so the `vault-config` Job auto-initializes a **fresh**
vault first (no `openbao-unseal` exists yet → the guard does not trigger).
Recovering the old data means deliberately resetting that fresh vault into
the Scenario 2 shape:

1. `ksail cluster create` + `workload push`/`reconcile` — the platform
   converges with a fresh, empty vault (PushSecrets re-seed SOPS-sourced
   values, so the cluster is functional but generated secrets are new).
2. Restore the old `openbao-unseal` Secret (from the Velero backup) over the
   fresh one, and copy the newest snapshot from the R2 `openbao-snapshots/`
   mirror onto the `vault-snapshots` PVC.
3. Scale OpenBao to 0, delete the fresh `data-openbao-*` PVCs, scale it back
   to 3 and reconcile, as in Scenario 2 — the `vault-config` Job now hits the
   automated restore path and brings back the pre-incident vault.
4. ExternalSecrets resume syncing from the restored data; consumers pick up
   the old (matching) credentials.

**Key point**: a snapshot and the `openbao-unseal` Secret must come from the
same generation (same backup day) — keys from one era cannot unseal a
snapshot from another.

### Scenario 4: Full cluster rebuild (no Velero backup)

**Symptom**: Cluster and backups are lost. Starting from scratch.

**Resolution**: SOPS-encrypted Secrets in Git remain the source of truth for
bootstrap. On a fresh cluster:

1. `ksail cluster create` -- provisions infrastructure
2. Flux deploys `bootstrap` -> SOPS-encrypted Secrets are available
3. Flux deploys `infrastructure-controllers` -> OpenBao + ESO start.
   Controllers with placeholder Secrets (Dex, OAuth2-proxy) start with
   dummy values.
4. Flux deploys `infrastructure` -> `vault-config` Job auto-initializes:
   - Detects vault is uninitialized -> runs `bao operator init`
   - Stores unseal key + root token in `openbao-unseal` K8s Secret
   - Unseals and configures policies/roles
5. ESO Password generators create random secrets (DB passwords, OIDC
   client secrets). PushSecrets seed the vault from both generators and
   SOPS variables.
6. ExternalSecrets sync secrets to consumer namespaces, overwriting
   placeholder Secrets.
7. Reloader restarts Dex, OAuth2-proxy with real secrets.

No manual steps required.

> **Note**: The Docker provider's platform CA key pair is not stored in
> OpenBao. cert-manager auto-generates it via a self-signed CA Certificate
> resource, so PushSecrets are not involved for that secret.

### Scenario 5: Lost unseal key (Secret deleted, no Velero backup)

**Symptom**: The `openbao-unseal` Secret was deleted and no backup exists.
The vault is sealed and cannot be unsealed.

**Resolution**: The vault data is unrecoverable without the unseal key.
Re-initialize from scratch by removing every server's Raft data, the same
reset Scenario 2 uses:

1. Scale down the StatefulSet:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=0
   ```
2. Delete the data PVC of **every** server (NOT `vault-snapshots`):
   ```bash
   kubectl delete pvc -n openbao data-openbao-0 data-openbao-1 data-openbao-2
   ```
   All three are needed. A server that keeps its data still reports
   `initialized: true`, and the `vault-config` Job then stops with
   `vault is initialized but no unseal key is available` instead of
   initializing a fresh vault.
3. Scale the StatefulSet back up to its three servers, which start with empty
   volumes:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=3
   ```
   Flux does not restore the count: its drift detection ignores StatefulSet
   replica counts, so a reconcile leaves the vault at zero servers.
4. Trigger Flux reconciliation (`ksail workload reconcile`). The `vault-config`
   Job finds no initialized server and no `openbao-unseal` Secret, so it runs
   `bao operator init` on `openbao-0`, stores the new unseal key and root
   token in a new `openbao-unseal` Secret, and unseals every server.
5. ESO Password generators create new random secrets. PushSecrets re-seed
   all secrets (both generated and SOPS-sourced). The Docker provider's
   platform CA key pair is auto-generated by cert-manager and does not
   depend on OpenBao.

Deleting the claims is what removes the old data. Scaling to zero only
stops the servers, and the StatefulSet keeps their claims by default; it
comes first so the claims can be deleted and then recreated empty. A
`kubectl rollout restart` cannot stand in for these steps: the production
StatefulSet uses the `OnDelete` update strategy, so a restart changes the
pod template but recreates no pod.

## Prove recovery before moving Email credentials

After the snapshot repair has deployed and its replacement-storage job has
completed, dispatch **Verify OpenBao Backup Recovery** from current `main`.
The confirmation input must be exactly `verify-openbao-backup-recovery`.
It uses the protected production environment and the shared deployment lock.

The workflow downloads a fresh, complete snapshot from the existing independent
backup store and binds that download to the listed object. It reads the three
original Email credential records, then ends its production connection before
restoring in a disposable, non-root, network-disabled server. It verifies all
record fields and a fresh unseal with the retained recovery key. Production
storage, credentials and backup objects are never changed by this verification.
Private inputs and diagnostic logs stay in temporary RAM storage; the workflow
publishes only a fixed verdict and removes its owned runtime resources.

Only `OPENBAO_BACKUP_RESTORE=PASS records=3 version=2.6.3` establishes this
three-record proof. A missing, stale, truncated, changed or mismatched backup
refuses success. This proof must be expanded before retiring additional records;
it does not by itself authorize credential migration or legacy-copy removal.

## References

- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
- [Velero Backup and Restore](https://velero.io/docs/)
