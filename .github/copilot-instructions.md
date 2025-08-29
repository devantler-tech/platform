# DevantlerTech Platform - GitHub Copilot Instructions

Always reference these instructions first and fallback to search or bash commands only when you encounter unexpected information that does not match the info here.

## Working Effectively

### Prerequisites and Tool Installation
Install the required tools in this exact order:

```bash
# Install Docker (if not already installed)
curl -fsSL https://get.docker.com -o get-docker.sh && sudo sh get-docker.sh

# Install Age for secret encryption
sudo apt-get update && sudo apt-get install -y age

# Install SOPS for secret management
wget -O /tmp/sops_linux_amd64.deb https://github.com/getsops/sops/releases/download/v3.8.1/sops_3.8.1_amd64.deb
sudo dpkg -i /tmp/sops_linux_amd64.deb

# Install Flux CLI
wget -O /tmp/flux_linux_amd64.tar.gz https://github.com/fluxcd/flux2/releases/download/v2.4.0/flux_2.4.0_linux_amd64.tar.gz
cd /tmp && tar -xzf flux_linux_amd64.tar.gz && sudo mv flux /usr/local/bin/

# Install KSail (main tool for local development)
# Note: Installation may fail in some environments due to module size limitations
wget -O /tmp/ksail https://github.com/devantler-tech/ksail/releases/download/v1.6.0/ksail_linux_amd64
chmod +x /tmp/ksail && sudo mv /tmp/ksail /usr/local/bin/
```

### Local Development Cluster

**Primary method (requires KSail):**
```bash
# NEVER CANCEL: Cluster startup takes 3-5 minutes. Set timeout to 10+ minutes.
ksail up
```

**Alternative method (if KSail unavailable):**
```bash
# Create cluster using Kind directly - takes 45 seconds. NEVER CANCEL.
kind create cluster --config kind.yaml

# Verify cluster creation
kubectl get nodes
kubectl get pods -A

# Clean up when done
kind delete cluster --name local
```

### Verification Commands
Always run these commands to ensure your environment is properly set up:
```bash
# Verify all tools are installed
docker --version
kind --version
kubectl version --client
flux version --client
sops --version
age --version

# Verify Docker is running
docker ps

# Check for existing clusters
kind get clusters

# Test Flux prerequisites
flux check --pre
```

## Repository Structure and Navigation

### Key Directories
- **`k8s/`** - All Kubernetes manifests and GitOps configuration
  - **`k8s/clusters/`** - Environment-specific configurations (local, dev, prod)
  - **`k8s/distributions/`** - Distribution-specific configs (kind, talos)
  - **`k8s/bases/`** - Shared base configurations
    - **`k8s/bases/infrastructure/`** - Core infrastructure components (controllers, certificates, policies)
    - **`k8s/bases/apps/`** - Application deployments (homepage, nextcloud, whoami)
- **`hetzner/`** - Hetzner Cloud provisioning scripts
- **`.sops.yaml`** - SOPS encryption configuration
- **`kind.yaml`** - Local Kind cluster configuration
- **`ksail.yaml`** - KSail cluster configuration

### Important Files
- **`README.md`** - Main repository documentation
- **`ksail.yaml`** - Defines local cluster with Flux, Cilium, Traefik, SOPS
- **`kind.yaml`** - 4-node cluster (1 control-plane, 3 workers) with disabled CNI
- **`.github/workflows/`** - CI/CD pipelines for cluster bootstrap and deployment

## Common Tasks and Workflows

### Local Development Workflow
1. **Setup**: Install prerequisites and verify tools
2. **Start**: Run `ksail up` (3-5 minutes, NEVER CANCEL)
3. **Develop**: Make changes to YAML files in `k8s/` directory
4. **Test**: Apply changes using Flux or kubectl
5. **Cleanup**: Run `ksail down` or `kind delete cluster --name local`

### Working with Secrets
This platform uses SOPS with Age encryption for all secrets:
```bash
# View encrypted secrets (requires proper Age key)
sops -d k8s/clusters/local/infrastructure/some-secret.enc.yaml

# Encrypt new secrets
sops -e --input-type yaml --output-type yaml secret.yaml > secret.enc.yaml
```

**Important**: You CANNOT decrypt existing secrets without the proper Age keys. For local development:
1. Fork the repository
2. Generate your own Age keys: `age-keygen -o key.txt`
3. Update `.sops.yaml` with your public key
4. Re-encrypt all `*.enc.yaml` files with your key

### Validation and Testing
Always run these validation steps after making changes:
```bash
# Validate Kubernetes YAML syntax
kubectl apply --dry-run=client -f k8s/

# Validate Kustomize builds
kustomize build k8s/clusters/local/

# Test Flux kustomizations
flux check
```

### Hetzner Cloud Operations
Scripts for managing Hetzner Cloud infrastructure are in `hetzner/`:
```bash
# Create a server (requires Hetzner API token)
./hetzner/create-server.sh --token <token> --server-name <name> --image-id <id>

# Create snapshot from Talos media
./hetzner/create-snapshot.sh --token <token> --media-path <path>

# Delete a server
./hetzner/delete-server.sh --token <token> --server-name <name>
```

## Timing Expectations and Warnings

**CRITICAL: NEVER CANCEL BUILDS OR LONG-RUNNING COMMANDS**

- **Cluster Creation**: 30-45 seconds (tested: 30-43 seconds) - NEVER CANCEL. Set timeout to 5+ minutes.
- **Cluster Deletion**: 1-2 seconds (tested: 1.2 seconds) - NEVER CANCEL. Set timeout to 2+ minutes.
- **KSail Up**: 3-5 minutes for full bootstrap - NEVER CANCEL. Set timeout to 10+ minutes.
- **Flux Reconciliation**: 2-5 minutes per kustomization - NEVER CANCEL. Set timeout to 10+ minutes.
- **Tool Installation**: 1-3 minutes total (tested: apt update takes 30+ seconds) - NEVER CANCEL. Set timeout to 5+ minutes.
- **Kustomize Build**: Under 1 second (tested: immediate) - Set timeout to 1+ minute.

## Known Limitations and Workarounds

### KSail Installation Issues
- KSail installation may fail due to large module size
- **Workaround**: Use Kind directly with the provided `kind.yaml` configuration
- Manual cluster setup is supported but requires additional Flux bootstrap steps

### SOPS Decryption Requirements
- Cannot decrypt existing secrets without proper Age keys
- **Workaround**: Fork repository and use your own Age keys for development
- Local development requires secret re-encryption with personal keys

### CNI Configuration
- Kind cluster starts with `disableDefaultCNI: true`
- Nodes will be NotReady until Cilium is installed via Flux
- This is expected behavior - the GitOps process handles CNI installation

## Platform Architecture

This is a **GitOps-based Kubernetes platform** using:
- **Flux CD** for declarative GitOps
- **Cilium** for Container Network Interface (CNI)
- **Traefik** for ingress controller
- **SOPS + Age** for secret encryption at rest
- **Kustomize** for configuration templating
- **Kind** for local development clusters
- **Talos Omni** for production cluster management

### Kustomization Flow
The platform uses a hierarchical kustomization structure:
1. **Base configurations** in `k8s/bases/`
2. **Distribution-specific** overlays in `k8s/distributions/`
3. **Cluster-specific** overlays in `k8s/clusters/`

### Dependency Order
Infrastructure components are deployed in this order:
1. **infrastructure-controllers** (Flux controllers)
2. **infrastructure** (core components like Cilium, SOPS)
3. **apps** (applications and services)

## Validation Scenarios

After making any changes, ALWAYS test these scenarios:

### Basic Cluster Functionality
1. **Cluster Creation**: Verify `kind create cluster --config kind.yaml` succeeds
2. **Node Status**: Check nodes become Ready after CNI installation
3. **Pod Deployment**: Verify core pods start successfully

### GitOps Validation
1. **Kustomize Build**: Ensure `kustomize build k8s/clusters/local/` succeeds
2. **Flux Check**: Verify `flux check --pre` passes
3. **YAML Validation**: Run `kubectl apply --dry-run=client` on generated manifests

### Manual Functional Testing
If cluster is fully operational:
1. **Access Applications**: Test ingress routes (if configured)
2. **Secret Handling**: Verify SOPS integration works
3. **Network Connectivity**: Check pod-to-pod communication

## Common Output References

### Repository Root Listing
```
.cspell.json          - Spell check configuration
.git/                 - Git repository data
.github/              - GitHub workflows and configurations
.gitignore            - Git ignore patterns
.policyignore         - Policy check ignore patterns
.releaserc            - Semantic release configuration
.sops.yaml            - SOPS encryption rules
.vscode/              - VSCode settings
README.md             - Main documentation
docs/                 - Additional documentation
hetzner/              - Hetzner Cloud scripts
hosts                 - Host configurations
k8s/                  - Kubernetes manifests
kind.yaml             - Kind cluster configuration
ksail.yaml            - KSail configuration
talos/                - Talos configurations
```

### Cluster Status (Expected)
```bash
# kubectl get nodes (after CNI installation)
NAME                  STATUS   ROLES           AGE   VERSION
local-control-plane   Ready    control-plane   5m    v1.33.1
local-worker          Ready    <none>          4m    v1.33.1
local-worker2         Ready    <none>          4m    v1.33.1
local-worker3         Ready    <none>          4m    v1.33.1
```

## Emergency Procedures

### Cluster Recovery
```bash
# If cluster is unresponsive
kind delete cluster --name local
kind create cluster --config kind.yaml

# If KSail cluster is corrupted
ksail down
ksail up
```

### Tool Reinstallation
If tools stop working, reinstall in this order:
1. Docker (restart service if needed)
2. Kind (reinstall from GitHub releases)
3. Kubectl (check cluster context)
4. Flux CLI (verify version compatibility)
5. SOPS and Age (check encryption keys)

Remember: **ALWAYS follow these instructions first**. Only use additional search or commands when encountering unexpected issues not covered here.