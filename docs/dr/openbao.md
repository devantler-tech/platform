# OpenBao Secrets Vault — Disaster Recovery

## Overview

OpenBao stores secrets in file-based storage. The Velero daily backup captures
the openbao namespace PVCs and the `openbao-unseal` Secret (which contains the
unseal key and root token). The `vault-config` Job auto-initializes OpenBao on
fresh clusters and auto-unseals on restarts.

## Recovery Scenarios

### Scenario 1: Single pod restart

**Symptom**: OpenBao pod was evicted or restarted; vault shows `sealed: true`.

**Resolution**: The `postStart` hook auto-unseals using the `openbao-unseal`
Secret (created by the `vault-config` Job on first init, backed up by Velero).
No manual action needed. Verify:

```bash
kubectl exec -n openbao openbao-0 -- bao status
```

### Scenario 2: PVC data corruption

**Symptom**: OpenBao fails to start, storage errors in logs.

**Resolution**:

1. Scale down the StatefulSet:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=0
   ```
2. Delete the corrupted PVC:
   ```bash
   kubectl delete pvc -n openbao data-openbao-0
   ```
3. Scale back up:
   ```bash
   kubectl scale statefulset -n openbao openbao --replicas=1
   ```
4. Delete the stale `openbao-unseal` Secret (old keys are for the old storage):
   ```bash
   kubectl delete secret -n openbao openbao-unseal
   ```
5. Trigger Flux reconciliation (`ksail workload reconcile`) — the `vault-config`
   Job re-runs, auto-initializes with fresh keys, and configures policies/roles.
6. PushSecrets re-seed the vault from SOPS variables on next reconciliation.

### Scenario 3: Full cluster rebuild (Velero restore)

**Symptom**: Entire cluster is lost (DR scenario), Velero backup available.

**Resolution**:

1. `ksail cluster create` — provisions infrastructure
2. Deploy Velero and restore from backup — this restores:
   - OpenBao PVC (vault data)
   - `openbao-unseal` Secret (unseal key + root token)
3. Flux deploys `infrastructure-controllers` → OpenBao starts
4. The `postStart` hook reads the restored `openbao-unseal` Secret → auto-unseals
5. `vault-config` Job runs → detects vault is already initialized → skips init →
   converges policies/roles
6. ExternalSecrets resume syncing from the restored vault data

**Key point**: Velero backs up both the PVC (vault data) and the `openbao-unseal`
Secret (unseal credentials). Both are needed for a complete restore.

### Scenario 4: Full cluster rebuild (no Velero backup)

**Symptom**: Cluster and backups are lost. Starting from scratch.

**Resolution**: SOPS-encrypted Secrets in Git remain the source of truth for
bootstrap. On a fresh cluster:

1. `ksail cluster create` — provisions infrastructure
2. Flux deploys `variables` → SOPS-encrypted Secrets are available
3. Flux deploys `infrastructure-controllers` → OpenBao + ESO start
4. Flux deploys `infrastructure` → `vault-config` Job auto-initializes:
   - Detects vault is uninitialized → runs `bao operator init`
   - Stores unseal key + root token in `openbao-unseal` K8s Secret
   - Unseals and configures policies/roles
5. PushSecrets seed the vault from SOPS variables
6. ExternalSecrets sync secrets to consumer namespaces

No manual steps required.

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
3. Trigger Flux reconciliation — the `vault-config` Job re-initializes the vault.
4. PushSecrets re-seed all secrets from SOPS variables.

## Raft Snapshot Restore

To restore from a Raft snapshot (if one was captured before data loss):

```bash
# Copy snapshot to pod
kubectl cp snapshot.snap openbao/openbao-0:/tmp/snapshot.snap

# Restore (requires root token from openbao-unseal Secret)
ROOT_TOKEN=$(kubectl get secret -n openbao openbao-unseal -o jsonpath='{.data.root-token}' | base64 -d)
kubectl exec -n openbao openbao-0 -- env BAO_TOKEN=$ROOT_TOKEN bao operator raft snapshot restore /tmp/snapshot.snap
```

## References

- [OpenBao Operator Raft Snapshot](https://openbao.org/docs/commands/operator/raft/snapshot/)
- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
