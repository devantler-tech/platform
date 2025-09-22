# Data Model: GitOps Configuration and Infrastructure

## Core Entities

### GitOps Configuration Manifest

**Purpose**: Declarative representation of desired cluster state
**Attributes**:

- `apiVersion`: Kubernetes API version
- `kind`: Resource type (Deployment, Service, ConfigMap, etc.)
- `metadata`: Resource identification (name, namespace, labels)
- `spec`: Desired state specification
- `status`: Current state (managed by Kubernetes)

**Relationships**:

- Contains references to ConfigMaps and Secrets
- Groups into Kustomizations for Flux management
- Inherits from base configurations via Kustomize overlays

### Kustomization Configuration

**Purpose**: Flux GitOps reconciliation unit
**Attributes**:

- `source`: Git repository reference
- `path`: Directory path within repository
- `interval`: Reconciliation frequency
- `dependsOn`: Dependency relationships
- `healthChecks`: Validation criteria

**Relationships**:

- Manages multiple GitOps Configuration Manifests
- Dependencies form directed acyclic graph
- Contains environment-specific overlays

### Encrypted Secret

**Purpose**: Secure storage of sensitive configuration data
**Attributes**:

- `encryptedData`: SOPS-encrypted key-value pairs
- `ageRecipients`: Public keys for decryption access
- `creationRules`: SOPS encryption patterns
- `metadata`: Standard Kubernetes secret metadata

**Relationships**:

- Referenced by GitOps Configuration Manifests
- Encrypted with environment-specific Age keys
- Decrypted during Flux reconciliation

### Environment Configuration

**Purpose**: Environment-specific customizations and overlays
**Attributes**:

- `environmentName`: Target environment identifier (local, dev, prod)
- `kustomizePatches`: Environment-specific modifications
- `configMapOverrides`: Environment-specific configuration
- `secretReferences`: Environment-specific secret mappings
- `resourceLimits`: Environment-specific resource constraints

**Relationships**:

- Extends base configurations
- Contains environment-specific secrets
- Applies to specific cluster distributions

### KSail Cluster Definition

**Purpose**: Local development cluster specification
**Attributes**:

- `clusterName`: Local cluster identifier
- `kubernetesVersion`: Target Kubernetes version
- `nodeConfiguration`: Worker node specifications
- `networkConfiguration`: CNI and networking setup
- `toolingConfiguration`: Development tool integrations

**Relationships**:

- Deploys Environment Configurations
- Validates GitOps Configuration Manifests locally
- Bootstraps Flux GitOps controllers

### Policy Rule

**Purpose**: Governance and compliance constraints
**Attributes**:

- `ruleName`: Policy identifier
- `ruleType`: Validation, mutation, or generation
- `targetResources`: Applicable resource types
- `constraints`: Validation criteria
- `enforcement`: Enforce, warn, or audit mode

**Relationships**:

- Applied to GitOps Configuration Manifests
- Enforced during Flux reconciliation
- Validates constitutional compliance

## Configuration Hierarchy

### Base Layer

- Shared infrastructure components
- Common application templates
- Standard policy rules
- Default resource specifications

### Distribution Layer

- Kind-specific configurations
- Talos-specific configurations
- Platform-specific networking
- Distribution-specific tools

### Cluster Layer

- Environment-specific overlays
- Cluster-specific secrets
- Environment resource limits
- Cluster-specific policies

## Data Flow Patterns

### GitOps Reconciliation Flow

```
Git Repository → Flux Controller → Kustomize Build → Kubernetes API → Cluster State
```

### Secret Management Flow

```
Plaintext Secret → SOPS Encryption → Git Storage → Flux Decryption → Kubernetes Secret
```

### Local Development Flow

```
KSail Configuration → Kind Cluster → Flux Bootstrap → Local Validation → Development Feedback
```

### Configuration Inheritance

```
Base Configuration → Distribution Overlay → Environment Overlay → Final Manifest
```

## Validation Rules

### Manifest Validation

- All manifests must pass Kubernetes schema validation
- Resource names must follow naming conventions
- Labels must include required metadata
- Resource limits must be specified

### Secret Validation

- No plaintext secrets in repository
- All secrets must be SOPS encrypted
- Age recipients must be configured per environment
- Secret rotation procedures documented

### Constitutional Compliance

- GitOps-first: All changes via Git commits
- Security by design: SOPS encryption mandatory
- Test-first: Local validation required
- Infrastructure as code: Declarative YAML only
- Observability: Monitoring and policies required

## Storage Considerations

### Git Repository Structure

```
k8s/
├── bases/           # Base configurations (reusable)
├── distributions/   # Platform-specific overlays
└── clusters/        # Environment-specific overlays
```

### Secret Storage

- Encrypted *.enc.yaml files in Git
- Age keys stored securely outside repository
- Environment-specific encryption keys
- Automated decryption during deployment

### State Management

- Git as single source of truth
- Kubernetes cluster as desired state target
- Flux controllers manage reconciliation
- No manual cluster modifications permitted
