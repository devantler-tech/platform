# OpenBao Secrets Vault — Disaster Recovery

## Overview

OpenBao stores secrets in a Raft-backed file storage engine. Backups are taken
daily via a CronJob (`vault-snapshot`) and retained for 3 days in-cluster. The
Velero daily file-level backup also captures the openbao namespace PVCs.

## Recovery Scenarios

### Scenario 1: Single pod restart

**Symptom**: OpenBao pod was evicted or restarted; vault shows `sealed: true`.

**Resolution**: The `postStart` hook auto-unseals using the SOPS-decrypted
`openbao-unseal` Secret. No manual action needed. Verify:

```bash
kubectl exec -n openbao openbao-0 -- bao status
```

### Scenario 2: PVC data corruption

**Symptom**: OpenBao fails to start, Raft errors in logs.

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
4. Re-initialize:
   ```bash
   kubectl exec -n openbao openbao-0 -- bao operator init -key-shares=1 -key-threshold=1
   ```
5. Update the `openbao-unseal-secret.enc.yaml` with new unseal key and root token.
6. Re-run the `vault-config` Job to restore policies and roles.
7. Re-seed secrets from SOPS variables (PushSecrets will re-run on next Flux reconciliation).

### Scenario 3: Full cluster rebuild

**Symptom**: Entire cluster is lost (DR scenario).

**Resolution**: SOPS-encrypted secrets in Git remain the source of truth for
bootstrap. On a fresh cluster:

1. `ksail cluster create` — provisions infrastructure
2. Flux deploys `variables` → SOPS-encrypted Secrets are available
3. Flux deploys `infrastructure-controllers` → OpenBao + ESO start
4. OpenBao initializes fresh (new unseal keys + root token)
5. Update `openbao-unseal-secret.enc.yaml` and push to Git
6. `vault-config` Job runs → creates policies, roles, auth config
7. Flux deploys `infrastructure` → PushSecrets seed vault from SOPS variables
8. ExternalSecrets sync secrets from vault → all consumers get their Secrets

**Key point**: SOPS-encrypted Secrets in Git act as the offline backup. Even
after full migration to OpenBao, the SOPS variables files remain the recovery
source.

### Scenario 4: Lost unseal keys

**Symptom**: The `openbao-unseal-secret.enc.yaml` file is lost or corrupted,
and the vault is sealed.

**Resolution**: If the SOPS Age private key is available, the unseal key can
be recovered from Git history. If both are lost:

1. Delete the OpenBao PVC (all vault data is lost)
2. Re-initialize OpenBao from scratch (Scenario 2, steps 3-7)
3. Re-seed all secrets from SOPS variables

## Raft Snapshot Restore

To restore from a Raft snapshot (if one was captured before data loss):

```bash
# Copy snapshot to pod
kubectl cp snapshot.snap openbao/openbao-0:/tmp/snapshot.snap

# Restore (requires root token)
kubectl exec -n openbao openbao-0 -- bao operator raft snapshot restore /tmp/snapshot.snap
```

## References

- [OpenBao Operator Raft Snapshot](https://openbao.org/docs/commands/operator/raft/snapshot/)
- [OpenBao Seal/Unseal](https://openbao.org/docs/concepts/seal/)
