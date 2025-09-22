# Contract: KSail Local Cluster Bootstrap

## Purpose
Validate that KSail can successfully bootstrap a local Kubernetes cluster with GitOps capabilities

## Contract Definition

### Input
- KSail configuration file (ksail.yaml)
- Git repository with k8s/ directory structure
- Age keys for secret decryption

### Expected Behavior
1. **Cluster Creation**: `ksail up` creates Kind cluster within 10 minutes
2. **Flux Installation**: GitOps controllers deployed automatically
3. **Secret Decryption**: SOPS secrets successfully decrypted with local Age key
4. **Reconciliation**: All Kustomizations reach Ready state within 5 minutes
5. **Health Checks**: Core infrastructure pods reach Running state

### Success Criteria
```bash
# Cluster ready
kubectl get nodes --no-headers | grep Ready

# Flux controllers running
kubectl get pods -n flux-system --no-headers | grep Running

# Kustomizations reconciled
kubectl get kustomizations -A --no-headers | grep True

# No failed reconciliations
kubectl get kustomizations -A -o json | jq '.items[].status.conditions[] | select(.type=="Ready" and .status!="True")'
```

### Failure Scenarios
- Cluster creation timeout (>10 minutes)
- Flux installation failure
- Secret decryption errors due to missing/invalid Age keys
- Kustomization reconciliation failures
- Infrastructure pod startup failures

### Test Data
```yaml
# Minimal test manifest
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test-app
  template:
    metadata:
      labels:
        app: test-app
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - containerPort: 80
```

### Performance Requirements
- Total bootstrap time: <10 minutes
- GitOps reconciliation: <5 minutes after Git commit
- Pod startup time: <2 minutes for standard workloads

### Dependencies
- Docker runtime available
- Git repository accessible
- Age encryption keys properly configured
- Network connectivity for image pulls
