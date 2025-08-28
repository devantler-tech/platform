# Network Policy Deployment and Validation Guide

This guide provides step-by-step instructions for deploying and validating the least-privilege network policies.

## Pre-Deployment Checklist

Before deploying the network policies, ensure:

1. ✅ Cilium is installed and running (version 1.18.1+)
2. ✅ All applications are deployed and running
3. ✅ Testkube is available for running tests
4. ✅ You have cluster admin privileges

## Deployment Steps

### 1. Verify Current State

```bash
# Check Cilium status
kubectl get pods -n kube-system -l k8s-app=cilium

# Check existing network policies (should be empty initially)
kubectl get cnp,ccnp -A

# Verify all application pods are running
kubectl get pods -A
```

### 2. Deploy Network Policies

The network policies are automatically deployed via Flux GitOps when this branch is merged.

To manually deploy for testing:

```bash
# Build and apply network policies
cd k8s/bases/infrastructure/network-policies
kustomize build . | kubectl apply -f -
```

### 3. Immediate Verification

After deployment, verify policies are created:

```bash
# Check that policies are created
kubectl get cnp,ccnp -A

# Expected output should show:
# - 1 CiliumClusterwideNetworkPolicy (default-deny-all)
# - Multiple CiliumNetworkPolicy resources in each namespace
```

## Validation Process

### Automated Testing

Run the comprehensive test suite:

```bash
# Run all network policy tests
kubectl testkube run test network-policy-external-connectivity
kubectl testkube run test network-policy-internal-connectivity  
kubectl testkube run test network-policy-denial-verification
kubectl testkube run test network-policy-system-connectivity

# Check test results
kubectl testkube get executions
```

### Manual Validation

Use the provided validation script:

```bash
# Run the manual validation script
./k8s/bases/infrastructure/network-policies/validation/validate-network-policies.sh
```

### Step-by-Step Manual Tests

#### 1. Test External Connectivity (Should Work)

```bash
# Homepage should reach Cloudflare API
kubectl run test-homepage-external --rm -i --restart=Never \
  --namespace=homepage \
  --image=curlimages/curl:latest \
  -- curl -s https://api.cloudflare.com/client/v4/user/tokens/verify

# Nextcloud should reach external services
kubectl run test-nextcloud-external --rm -i --restart=Never \
  --namespace=nextcloud \
  --image=curlimages/curl:latest \
  -- curl -s -I https://download.nextcloud.com
```

#### 2. Test Internal Connectivity (Should Work)

```bash
# DNS should work from all namespaces
kubectl run test-dns --rm -i --restart=Never \
  --namespace=homepage \
  --image=busybox:latest \
  -- nslookup kubernetes.default.svc.cluster.local

# Testkube should reach whoami service
kubectl run test-internal --rm -i --restart=Never \
  --namespace=testkube \
  --image=curlimages/curl:latest \
  -- curl -s http://whoami.whoami.svc.cluster.local
```

#### 3. Test Traffic Denial (Should Fail)

```bash
# Whoami should NOT reach external sites
kubectl run test-blocked-external --rm -i --restart=Never \
  --namespace=whoami \
  --image=curlimages/curl:latest \
  -- timeout 5 curl -s https://www.google.com
# This should timeout/fail

# Cross-namespace access should be blocked
kubectl run test-blocked-cross --rm -i --restart=Never \
  --namespace=nextcloud \
  --image=curlimages/curl:latest \
  -- timeout 5 curl -s http://homepage.homepage.svc.cluster.local:3000
# This should timeout/fail
```

## Monitoring and Troubleshooting

### Cilium Policy Monitoring

```bash
# View policy status
cilium policy get

# Monitor traffic with Hubble
hubble observe --namespace homepage
hubble observe --verdict DROPPED
```

### Common Issues and Solutions

#### 1. Service Unreachable After Policy Deployment

**Symptoms:** Applications cannot reach services they need

**Diagnosis:**
```bash
# Check for dropped packets
hubble observe --verdict DROPPED --namespace <namespace>

# Check policy rules
kubectl describe cnp <policy-name> -n <namespace>
```

**Solution:** Update the network policy to include the required traffic pattern

#### 2. External Connectivity Blocked

**Symptoms:** Applications cannot reach external APIs

**Diagnosis:**
```bash
# Check if external traffic is being dropped
hubble observe --verdict DROPPED | grep <external-domain>
```

**Solution:** Add the domain to the `toFQDNs` rules in the appropriate policy

#### 3. Database Connection Issues

**Symptoms:** Applications cannot connect to databases

**Diagnosis:**
```bash
# Check database connectivity
kubectl run db-test --rm -i --restart=Never \
  --namespace=nextcloud \
  --image=postgres:15 \
  -- pg_isready -h db-rw.nextcloud.svc.cluster.local
```

**Solution:** Ensure both database and application are in the same namespace

### Emergency Rollback

If network policies cause critical issues:

```bash
# Remove all network policies immediately
kubectl delete cnp,ccnp -A

# Or remove just the global deny policy
kubectl delete ccnp default-deny-all
```

## Policy Maintenance

### Adding New Services

When adding new services:

1. Identify required network patterns
2. Create appropriate network policy
3. Add to kustomization.yaml
4. Create corresponding tests
5. Validate connectivity

### Updating Existing Policies

When modifying policies:

1. Update the policy YAML file
2. Update tests if needed
3. Deploy and validate
4. Monitor for any issues

### Security Auditing

Regular security reviews:

```bash
# Review all network policies
kubectl get cnp,ccnp -A -o yaml

# Check for any pods without network policies
# (All pods should be covered by at least the global deny policy)

# Monitor denied traffic patterns
hubble observe --verdict DROPPED --since 1h
```

## Best Practices

1. **Always test before deploying to production**
2. **Monitor traffic patterns after deployment**
3. **Keep policies as restrictive as possible**
4. **Document any exceptions or broad rules**
5. **Regularly review and audit policies**
6. **Use descriptive policy names and descriptions**
7. **Test both positive and negative cases**
8. **Keep emergency rollback procedures ready**

## Success Criteria

The network policies are successfully deployed when:

✅ All applications remain functional  
✅ External connectivity works for allowed services  
✅ Internal service communication works as expected  
✅ Unauthorized traffic is properly blocked  
✅ DNS resolution works from all namespaces  
✅ System components can access the API server  
✅ All automated tests pass  
✅ No legitimate traffic is being dropped