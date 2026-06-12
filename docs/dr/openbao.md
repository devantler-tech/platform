# OpenBao Secrets Vault — Disaster Recovery

## Overview

OpenBao runs as a raft (Integrated Storage) cluster. Three artifacts make it
recoverable:

1. **Raft snapshots** — the `vault-snapshot` CronJob saves a
   `bao operator raft snapshot` to the `vault-snapshots` PVC daily (newest 14
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
   exists on the PVC, it temp-initializes, runs
   `bao operator raft snapshot restore -force` with the newest snapshot, and
   unseals with the stored key — no operator action. Only when no snapshot is
   available does it abort and demand explicit data-loss acknowledgement
   (the #1982 guard, unchanged).

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
3. Trigger Flux reconciliation (`ksail workload reconcile`) — the StatefulSet
   scales back up with empty volumes, and the `vault-config` Job detects
   uninitialized-pods + surviving-keys + available-snapshot and restores
   automatically (worst-case RPO: 24 h, the snapshot cadence).
4. ExternalSecrets resume syncing; PushSecrets top up anything newer than the
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
3. Scale OpenBao to 0, delete the fresh `data-openbao-*` PVCs, reconcile —
   the `vault-config` Job now hits the automated restore path (Scenario 2)
   and brings back the pre-incident vault.
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
Re-initialize from scratch:

1. Delete the OpenBao PVC:
   ```bash
   kubectl delete pvc -n openbao data-openbao-0
   ```
2. Restart the StatefulSet:
   ```bash
   kubectl rollout restart statefulset -n openbao openbao
   ```
3. Trigger Flux reconciliation -- the `vault-config` Job re-initializes the vault.
4. ESO Password generators create new random secrets. PushSecrets re-seed
   all secrets (both generated and SOPS-sourced). The Docker provider's
   platform CA key pair is auto-generated by cert-manager and does not
   depend on OpenBao.

## References

- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
- [Velero Backup and Restore](https://velero.io/docs/)
