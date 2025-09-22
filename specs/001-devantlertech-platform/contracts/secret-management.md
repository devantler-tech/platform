# Contract: GitOps Secret Management

## Purpose
Validate that secrets are properly encrypted with SOPS and decrypted during deployment

## Contract Definition

### Input
- Plaintext Kubernetes secret
- SOPS configuration file (.sops.yaml)
- Age encryption keys for target environment

### Expected Behavior
1. **Encryption**: `sops -e secret.yaml > secret.enc.yaml` encrypts all secret data
2. **Git Storage**: Encrypted file can be safely committed to repository
3. **Decryption**: Flux automatically decrypts during reconciliation
4. **Application Access**: Pods can access decrypted secret data
5. **Key Rotation**: New Age keys can be added without service disruption

### Success Criteria
```bash
# Secret encrypted in repository
grep -q "sops:" k8s/clusters/local/*/secret.enc.yaml

# Secret decrypted in cluster
kubectl get secret test-secret -o yaml | grep -v "sops:"

# Secret data accessible to pods
kubectl exec test-pod -- cat /etc/secret/password
```

### Test Secret
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: test-secret
  namespace: default
type: Opaque
data:
  username: dGVzdA==  # 'test' base64 encoded
  password: cGFzc3dvcmQ=  # 'password' base64 encoded
```

### Encryption Validation
- No plaintext secrets in Git repository
- All *.enc.yaml files contain `sops:` metadata
- Age recipients match environment configuration
- Encrypted data differs from plaintext

### Security Requirements
- Age keys stored outside repository
- Environment-specific encryption keys
- No shared keys between environments
- Regular key rotation capability

### Failure Scenarios
- Missing Age keys during decryption
- Invalid SOPS configuration
- Corrupted encrypted data
- Network issues during key retrieval
