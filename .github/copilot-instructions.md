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

# Install KSail (main tool for local development)
brew tap devantler-tech/formulas && brew install ksail
```

### Local Development Cluster

**Primary method (requires KSail + Docker):**
```bash
# NEVER CANCEL: Cluster startup takes 3-5 minutes. Set timeout to 10+ minutes.
ksail cluster create

# Push manifests and trigger Flux reconciliation
ksail workload push
ksail workload reconcile
```

**Access local services** at `https://platform.lan` (requires host entries from the `hosts` file).

**Cleanup:**
```bash
ksail cluster delete
```

### Verification Commands
Always run these commands to ensure your environment is properly set up:
```bash
# Verify all tools are installed
docker --version
ksail --version
kubectl version --client
sops --version
age --version

# Verify Docker is running
docker ps

# Check for existing Talos clusters
ksail cluster list
```

## Repository Structure and Navigation

### Key Directories
- **`k8s/`** - All Kubernetes manifests and GitOps configuration
  - **`k8s/clusters/`** - Environment-specific configurations (local, dev, prod)
  - **`k8s/providers/`** - Provider-specific configs (docker, omni)
  - **`k8s/bases/`** - Shared base configurations
    - **`k8s/bases/infrastructure/`** - Core infrastructure components organized by resource type (e.g. `certificates/`, `gateway/`, `cluster-policies/`, `controllers/`)
    - **`k8s/bases/apps/`** - Application deployments (homepage, whoami, headlamp)
- **`talos-prod/`** - Talos machine config patches for Omni (production) clusters
- **`talos-local/`** - Talos machine config patches for Docker (local) clusters
- **`hetzner/`** - Hetzner Cloud provisioning scripts
- **`.sops.yaml`** - SOPS encryption configuration
- **`ksail.yaml`** - KSail local cluster configuration (Talos + Docker, `kustomizationFile: clusters/local`)
- **`ksail.prod.yaml`** - KSail production cluster configuration (Talos + Omni, `kustomizationFile: clusters/prod`)

### Important Files
- **`README.md`** - Main repository documentation
- **`ksail.yaml`** - Defines local Talos+Docker cluster with Flux, Cilium. Has `spec.workload.kustomizationFile: clusters/local` so Flux uses `k8s/clusters/local/kustomization.yaml` as the entry point.
- **`ksail.prod.yaml`** - Defines production Talos+Omni cluster with Flux, Cilium, GHCR registry. Has `spec.workload.kustomizationFile: clusters/prod` so Flux uses `k8s/clusters/prod/kustomization.yaml` as the entry point.
- **`talos-local/`** - Talos machine config patches for local Docker clusters
- **`talos-prod/`** - Talos machine config patches for Omni clusters (prod)
- **`.github/workflows/`** - CI/CD pipelines for cluster bootstrap and deployment

## Common Tasks and Workflows

### Local Development Workflow
1. **Setup**: Install prerequisites and verify tools
2. **Start**: Run `ksail cluster create` (3-5 minutes, NEVER CANCEL)
3. **Deploy**: Run `ksail workload push` then `ksail workload reconcile`
4. **Develop**: Make changes to YAML files in `k8s/` directory
5. **Test**: Run `ksail workload push` and `ksail workload reconcile` to apply changes
6. **Cleanup**: Run `ksail cluster delete`

### Production Deployment Workflow
Production uses **Talos + Omni** (managed by Sidero Omni SaaS). The cluster is pre-provisioned — only workloads are deployed via KSail.

**How it works:**
1. Push a `v*` tag to trigger the `CD - Deploy` workflow
2. The workflow uses `ksail --config ksail.prod.yaml` to target the committed prod config
3. `ksail.prod.yaml` has `kustomizationFile: clusters/prod`, which tells KSail/Flux to use `k8s/clusters/prod/kustomization.yaml` as the entry point — no root `k8s/kustomization.yaml` or file rewriting is needed
4. `ksail --config ksail.prod.yaml workload push` packages manifests and pushes to GHCR
5. `ksail --config ksail.prod.yaml workload reconcile` triggers Flux to sync from the OCI artifact

**Key differences from local:**
- No `ksail cluster create/delete` — Omni manages cluster lifecycle externally
- OCI artifacts pushed to GHCR (not a local registry)
- Kubeconfig is fetched via `omnictl kubeconfig` in CI/CD workflows (workaround until KSail handles this natively)
- SPIRE mutual auth is enabled (unlike local Docker clusters)
- Omni endpoint: `https://devantler.omni.siderolabs.io:443`

### CI/CD Pipelines
- **`ci.yaml`**: Runs on `pull_request` and `merge_group`. Creates a local Talos+Docker cluster, pushes manifests, reconciles, then cleans up.
- **`cd.yaml`**: Runs on `v*` tags. Deploys to production Omni cluster using `ksail --config ksail.prod.yaml`. The prod config has `kustomizationFile: clusters/prod` so no file rewriting is needed — KSail/Flux automatically uses the correct entry point.

**Required GitHub Secrets:**
- `GHCR_PAT` — long-lived PAT (owner: `devantler`) with `write:packages` scope, used for GHCR push/pull authentication
- `SOPS_AGE_KEY` — Age private key for SOPS secret decryption
- `OMNI_SERVICE_ACCOUNT_KEY` — Omni service account key for cluster access (dev/prod)

**Required GitHub Variables:**
- `OMNI_ENDPOINT` — Omni API endpoint URL (per-environment)

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
- **ksail cluster create**: 3-5 minutes for full bootstrap - NEVER CANCEL. Set timeout to 10+ minutes.
- **Flux Reconciliation**: 2-5 minutes per kustomization - NEVER CANCEL. Set timeout to 10+ minutes.
- **Tool Installation**: 1-3 minutes total (tested: apt update takes 30+ seconds) - NEVER CANCEL. Set timeout to 5+ minutes.
- **Kustomize Build**: Under 1 second (tested: immediate) - Set timeout to 1+ minute.

## Known Limitations and Workarounds

### macOS Port Exposure
- MetalLB virtual IPs are not accessible from macOS Docker Desktop (Docker VM isolation)
- Port mappings are configured in `ksail.yaml` under `spec.cluster.talos.extraPortMappings` to expose ports 80 and 443 from the Talos Docker container to the host
- Host entries in `hosts` file map `*.platform.lan` to `127.0.0.1`

### SOPS Decryption Requirements
- Cannot decrypt existing secrets without proper Age keys
- **Workaround**: Fork repository and use your own Age keys for development
- Local development requires secret re-encryption with personal keys

### CNI Configuration
- Talos cluster starts with default CNI disabled (via `talos-local/cluster/cni.yaml`)
- Nodes will be NotReady until Cilium is installed by KSail
- This is expected behavior - KSail handles CNI installation automatically

## Platform Architecture

This is a **GitOps-based Kubernetes platform** using:
- **Flux CD** for declarative GitOps from OCI artifacts
- **Cilium** for CNI with SPIRE mutual auth (prod) or without SPIRE (local Docker)
- **Traefik** for ingress controller
- **SOPS + Age** for secret encryption at rest (per-environment Age keys)
- **Kustomize** for configuration templating
- **KSail** for unified cluster and workload management
- **Talos + Docker** for local development clusters (via KSail)
- **Talos + Omni** for production cluster management (Sidero Omni SaaS)
- **GHCR** for OCI artifact storage (production)

### Dual-Provider Model
- **Local/CI**: `ksail cluster create` → Talos + Docker provider → local OCI registry → `ksail workload push/reconcile`
- **Production**: Omni manages cluster lifecycle → `ksail --config ksail.prod.yaml workload push` to GHCR → `ksail --config ksail.prod.yaml workload reconcile`

### Kustomization Flow
The platform uses a hierarchical kustomization structure:
1. **Base configurations** in `k8s/bases/`
2. **Provider-specific** overlays in `k8s/providers/`
3. **Cluster-specific** overlays in `k8s/clusters/`

### Dependency Order
Infrastructure components are deployed in this order:
1. **infrastructure-controllers** (Flux controllers)
2. **infrastructure** (core components like Cilium, SOPS)
3. **apps** (applications and services)

### Infrastructure File Structure Convention
Resources in `k8s/bases/infrastructure/` are organized by **resource type**, not by the component that uses them:
- `certificates/` — Certificate resources (e.g. `gateway-certificate.yaml`)
- `cluster-policies/` — ClusterPolicy resources
- `controllers/` — HelmRelease, HelmRepository, and related controller resources (each in a subdirectory by component name)
- `gateway/` — Gateway and infrastructure-level HTTPRoute resources (e.g. HTTP→HTTPS redirect)

Central gateway resources (Gateway, Certificate for TLS) are deployed to `kube-system` (the Cilium namespace) rather than a dedicated namespace.

## Validation Scenarios

After making any changes, ALWAYS test these scenarios:

### Basic Cluster Functionality
1. **Cluster Creation**: Verify `ksail cluster create` succeeds
2. **Node Status**: Check nodes become Ready after Cilium installation
3. **Pod Deployment**: Verify core pods start successfully

### GitOps Validation
1. **Kustomize Build**: Ensure `kustomize build k8s/clusters/local/` succeeds
2. **YAML Validation**: Run `kubectl apply --dry-run=client` on generated manifests

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
ksail.yaml            - KSail local configuration (kustomizationFile: clusters/local)
ksail.prod.yaml       - KSail production configuration (kustomizationFile: clusters/prod)
talos-prod/           - Talos configs for Omni clusters
talos-local/          - Talos configs for local Docker clusters
```

### Cluster Status (Expected)
```bash
# kubectl get nodes (after Cilium installation)
NAME                  STATUS   ROLES           AGE   VERSION
local-controlplane-1  Ready    control-plane   5m    v1.33.1
local-worker-1        Ready    <none>          4m    v1.33.1
local-worker-2        Ready    <none>          4m    v1.33.1
local-worker-3        Ready    <none>          4m    v1.33.1
```

## Emergency Procedures

### Local Cluster Recovery
```bash
# If local cluster is unresponsive
ksail cluster delete
ksail cluster create

# Then redeploy workloads
ksail workload push
ksail workload reconcile
```

### Production Cluster Recovery
The Omni cluster is managed externally. To redeploy workloads:
```bash
# From a machine with production kubeconfig access
ksail workload push
ksail workload reconcile
```
For cluster-level issues, use the Omni dashboard at `https://devantler.omni.siderolabs.io`.

### Tool Reinstallation
If tools stop working, reinstall in this order:
1. Docker (restart service if needed)
2. KSail (`brew reinstall ksail`)
3. Kubectl (check cluster context)
4. SOPS and Age (check encryption keys)
5. omnictl (`brew install siderolabs/tap/omnictl`) — for Omni cluster management

Remember: **ALWAYS follow these instructions first**. Only use additional search or commands when encountering unexpected issues not covered here.
