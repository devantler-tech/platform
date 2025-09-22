# Contract: Flux GitOps Reconciliation

## Purpose
Validate that Flux properly reconciles Git repository changes to cluster state

## Contract Definition

### Input
- Git repository with Kubernetes manifests
- Flux Kustomization configuration
- Target Kubernetes cluster

### Expected Behavior
1. **Change Detection**: Flux detects Git commits within 1 minute
2. **Kustomize Build**: Manifests built successfully with overlays
3. **Validation**: Dry-run validation passes before apply
4. **Application**: Resources created/updated in cluster
5. **Health Check**: Resources reach desired state within 5 minutes

### Success Criteria
```bash
# Kustomization shows latest commit
kubectl get kustomization -A -o custom-columns=NAME:.metadata.name,REVISION:.status.lastAppliedRevision,READY:.status.conditions[0].status

# No reconciliation errors
kubectl get kustomization -A -o json | jq '.items[].status.conditions[] | select(.type=="Ready" and .status!="True")'

# Resources match Git repository
kubectl diff -f <(kustomize build k8s/clusters/local)
```

### Test Scenarios

#### Scenario 1: New Resource Deployment
```yaml
# Add new ConfigMap to k8s/clusters/local/apps/
apiVersion: v1
kind: ConfigMap
metadata:
  name: test-config
  namespace: default
data:
  key: value
```

Expected: ConfigMap appears in cluster within 5 minutes

#### Scenario 2: Resource Update
```yaml
# Modify existing resource
data:
  key: updated-value
```

Expected: Resource updated in cluster, no downtime

#### Scenario 3: Resource Deletion
```yaml
# Remove resource from Git
```

Expected: Resource removed from cluster (with proper finalizers)

### Performance Requirements
- Change detection: <1 minute from Git commit
- Reconciliation completion: <5 minutes for standard workloads
- Resource validation: <30 seconds per manifest
- Rollback capability: <2 minutes to previous state

### Health Monitoring
```bash
# Flux controller health
kubectl get pods -n flux-system

# Reconciliation status
flux get kustomizations

# Source controller status
flux get sources git
```

### Failure Recovery
- Automatic retry on transient failures
- Rollback to last known good state
- Alert on persistent failures
- Manual intervention capability

### Dependencies
- Flux controllers running
- Git repository accessible
- Kubernetes cluster healthy
- RBAC permissions configured
