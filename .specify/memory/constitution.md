<!--
Sync Impact Report:
- Version change: initial → 1.0.0
- New constitution creation
- Principles established: GitOps-First, Security by Design, Test-First Development, Infrastructure as Code, Observability & Automation
- Added sections: Development Workflow, Deployment Policies
- Templates requiring updates: ✅ all templates align with established principles
- Follow-up TODOs: None
-->

# DevantlerTech Platform Constitution

## Core Principles

### I. GitOps-First Architecture

Everything MUST be declared as code and reconciled via Flux GitOps. All changes to cluster state MUST be made through Git commits to this repository. Direct kubectl apply or manual changes are PROHIBITED except for emergency debugging (and MUST be reverted through Git). Single source of truth maintained in Git with OCI artifact distribution.

**Rationale**: Ensures reproducibility, auditability, and prevents configuration drift across environments.

### II. Security by Design (NON-NEGOTIABLE)

All secrets MUST be encrypted at rest using SOPS with Age encryption before being committed to Git. Each environment (local/dev/prod) MUST use separate Age keys. No plaintext secrets are permitted in the repository. Secret rotation MUST be documented and testable.

**Rationale**: Enables secure storage of sensitive data in version control while maintaining environment isolation.

### III. Test-First Development (NON-NEGOTIABLE)

All infrastructure changes MUST be validated in local KSail cluster before being applied to development or production. CI/CD pipeline MUST verify cluster bootstrapping and application deployment. TDD cycle: Local test → CI validation → Environment promotion.

**Rationale**: Prevents production outages and ensures all changes are properly validated before deployment.

### IV. Infrastructure as Code

All Kubernetes resources MUST be defined using declarative YAML manifests, Kustomize overlays, or Helm charts. No imperative cluster modifications. Configuration MUST follow the hierarchical structure: bases → distributions → clusters. Resource templates MUST be reusable across environments.

**Rationale**: Maintains consistency, enables code review of infrastructure changes, and supports multi-environment deployments.

### V. Observability & Automation

Policy enforcement MUST be automated via Kyverno. Resource management MUST be automated (Reloader for config changes, Goldilocks for resource recommendations). Monitoring and alerting MUST be implemented for all critical services. Automated certificate management and secret rotation required.

**Rationale**: Reduces manual operational overhead and ensures platform reliability through proactive monitoring and policy enforcement.

## Development Workflow

Local development MUST follow this workflow:

1. Install prerequisites: Docker, KSail, kubectl, flux, sops, age
2. Create/modify Kubernetes manifests in appropriate k8s/ directory
3. Validate changes locally: `ksail up` (NEVER CANCEL - takes 3-5 minutes)
4. Test Flux reconciliation and application deployment
5. Create pull request with CI validation
6. Merge only after successful CI and peer review

**Timing Expectations**: Cluster creation (30-45 seconds), KSail bootstrap (3-5 minutes), Flux reconciliation (2-5 minutes). These operations MUST NOT be cancelled prematurely.

## Deployment Policies

**Environment Progression**: All changes MUST follow local → dev → prod promotion path.

**Multi-Cloud Strategy**: Hybrid deployment across Hetzner Cloud and on-premises infrastructure is supported. Talos Omni manages production clusters, Kind manages local development.

**High Availability**: Control plane nodes MUST be distributed across multiple availability zones in production. Workload scheduling on control planes is permitted for homelab use but discouraged for enterprise deployments.

**Resource Management**: All applications MUST define resource requests and limits. Goldilocks recommendations MUST be reviewed and applied appropriately.

## Governance

This constitution supersedes all other development practices. Amendments require:

1. Documented justification for the change
2. Impact assessment on existing workflows
3. Migration plan for affected templates and workflows
4. Approval via pull request review

All pull requests MUST verify constitutional compliance. Complexity MUST be justified against platform simplicity goals. Use `.github/copilot-instructions.md` for runtime development guidance and tool-specific instructions.

**Constitutional violations MUST be addressed before merge**. When in doubt, prioritize security, reproducibility, and GitOps principles.

**Version**: 1.0.0 | **Ratified**: 2025-09-22 | **Last Amended**: 2025-09-22
