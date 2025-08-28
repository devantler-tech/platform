# Network Policies

This directory contains Cilium Network Policies that implement least-privilege security for the entire cluster.

## Overview

The network policies follow a default-deny approach with explicit allow rules for necessary traffic. This ensures that all network communication is intentional and documented.

## Structure

### Global Policies
- **default-deny.yaml**: Cluster-wide default deny policy that blocks all traffic by default

### System Policies
- **kube-system.yaml**: Allows necessary traffic for Kubernetes system components
- **traefik.yaml**: Allows ingress controller traffic and external access
- **goldilocks.yaml**: Allows VPA recommendation system to access API server
- **testkube.yaml**: Allows testing framework to validate network connectivity

### Application Policies
- **homepage.yaml**: Allows dashboard to access external APIs for widgets
- **nextcloud.yaml**: Allows file sharing platform and database communication
- **whoami.yaml**: Allows simple test application for connectivity validation

## Security Model

### Default Behavior
- All traffic is denied by default (global default-deny policy)
- Only explicitly allowed traffic is permitted
- Policies are applied at the namespace level

### Allowed Traffic Patterns

#### DNS Resolution
All namespaces are allowed to query DNS in kube-system:
- Port 53 UDP/TCP to kube-dns

#### API Server Access
System components and some applications can access the Kubernetes API:
- Port 6443/443 TCP to kube-system

#### Ingress Traffic
Applications can receive traffic from Traefik ingress controller:
- HTTP/HTTPS traffic from traefik namespace

#### External Access
Only specific namespaces can access external services:
- **homepage**: Cloudflare API, Unsplash, etc. for dashboard widgets
- **nextcloud**: Nextcloud.com services for app store and updates
- **testkube**: General external access for connectivity testing

#### Database Access
Database connections are restricted to the same namespace:
- **nextcloud**: PostgreSQL communication within nextcloud namespace

## Testing

Comprehensive network connectivity tests are included to validate:

1. **External Connectivity**: Verifies allowed external access works
2. **Internal Connectivity**: Validates service-to-service communication
3. **Denial Verification**: Confirms unauthorized access is blocked
4. **System Connectivity**: Ensures system components can communicate

### Running Tests

Tests are automatically deployed with the network policies and can be executed using Testkube:

```bash
# Run all network policy tests
kubectl testkube run test network-policy-external-connectivity
kubectl testkube run test network-policy-internal-connectivity
kubectl testkube run test network-policy-denial-verification
kubectl testkube run test network-policy-system-connectivity
```

## Troubleshooting

### Common Issues

1. **Service Unreachable**: Check if the appropriate ingress/egress rules exist
2. **External Access Blocked**: Verify FQDN patterns in toFQDNs rules
3. **Database Connection Failed**: Ensure both source and destination are in the same namespace

### Debugging

To debug network policy issues:

1. Check Cilium logs: `kubectl logs -n kube-system -l k8s-app=cilium`
2. Use Hubble for traffic monitoring: `hubble observe --namespace <namespace>`
3. Validate policy syntax: `cilium policy validate <policy-file>`

### Policy Updates

When adding new services or modifying traffic patterns:

1. Update the appropriate network policy YAML file
2. Add corresponding test cases
3. Validate connectivity after deployment
4. Monitor for any blocked legitimate traffic

## Security Considerations

- Policies implement least-privilege access
- External access is limited to necessary services only
- Cross-namespace communication is restricted
- Database access is isolated to application namespaces
- System components have required cluster access
- All policies are version-controlled and auditable