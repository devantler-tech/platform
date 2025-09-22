# Tasks: GitOps Configuration and Infrastructure

**Input**: Design documents from `/specs/001-gitops-configuration-and/`
**Prerequisites**: plan.md (required), research.md, data-model.md, contracts/

## Execution Flow (main)

```text
1. Load plan.md from feature directory
   → Extract: Flux GitOps v2.x, SOPS+Age, Kustomize, KSail
2. Load design documents:
   → data-model.md: 6 entities → configuration tasks
   → contracts/: 3 files → contract test tasks
   → research.md: GitOps workflow → setup tasks
3. Generate tasks by category:
   → Setup: KSail cluster, Age keys, SOPS configuration
   → Validation: Infrastructure, reconciliation, operational verification
   → Core: Validate existing configurations work properly
   → Applications: Validate application deployment via GitOps
   → End-to-end: Complete workflow validation, documentation
4. Apply task rules:
   → Independent validations = [P] parallel
   → Dependent validations = sequential
   → Validation before enhancement
5. Return: SUCCESS (tasks ready for execution)
```

## Format: `[ID] [P?] Description`

- **[P]**: Can run in parallel (independent configurations, no dependencies)
- Include exact file paths for Kubernetes manifests and test files

## Path Conventions

- **GitOps configs**: `k8s/bases/`, `k8s/distributions/`, `k8s/clusters/`
- **Validation**: Direct kubectl/ksail commands for operational verification
- **Scripts**: Root directory scripts and tools (when needed for automation)

## Phase 3.1: Setup and Validation

- [x] T001 ~~Generate local Age encryption key pair for SOPS secret management~~ **COMPLETED - Age keys already configured in .sops.yaml**
- [x] T002 ~~Configure SOPS rules in .sops.yaml for local development environment~~ **COMPLETED - .sops.yaml fully configured**
- [ ] T003 [P] Validate KSail cluster configuration and test cluster bootstrap with `ksail up`
- [ ] T004 [P] Validate Flux prerequisites and GitOps reconciliation with `flux check --pre`

## Phase 3.2: Infrastructure Validation

- [ ] T005 [P] Validate KSail bootstrap functionality and cluster health
- [ ] T006 [P] Validate Flux reconciliation and GitOps workflow
- [ ] T007 [P] Validate SOPS secret management and decryption
- [ ] T008 [P] Validate end-to-end application deployment via GitOps
- [ ] T009 [P] Validate cluster policies and governance enforcement

## Phase 3.3: Configuration Validation and Enhancement

- [x] T010 ~~Infrastructure controllers Kustomization~~ **COMPLETED - controllers/ directory exists with testkube, flux configs**
- [x] T011 ~~Certificates configuration~~ **COMPLETED - k8s/bases/infrastructure/certificates/ exists**
- [x] T012 ~~Cluster policies~~ **COMPLETED - k8s/bases/infrastructure/cluster-policies/ exists**
- [x] T013 ~~Middlewares configuration~~ **COMPLETED - k8s/bases/infrastructure/middlewares/ exists**
- [x] T014 ~~Base applications Kustomization~~ **COMPLETED - k8s/bases/apps/kustomization.yaml includes homepage, nextcloud, whoami**
- [x] T015 ~~Variables base configuration~~ **COMPLETED - k8s/bases/variables/ with encrypted secrets exists**
- [x] T016 ~~Kind distribution overlay~~ **COMPLETED - k8s/distributions/kind/infrastructure/ exists**
- [x] T017 ~~Kind applications overlay~~ **COMPLETED - k8s/distributions/kind/apps/ exists**
- [x] T018 ~~Local cluster infrastructure overlay~~ **COMPLETED - k8s/clusters/local/infrastructure/ exists**
- [x] T019 ~~Local cluster applications overlay~~ **COMPLETED - k8s/clusters/local/apps/ exists**
- [x] T020 ~~Local cluster variables configuration~~ **COMPLETED - k8s/clusters/local/variables/ with encrypted secrets exists**

## Phase 3.4: Secret Management Validation

- [x] T021 ~~Create test secret template~~ **COMPLETED - secrets already exist in variables/**
- [x] T022 ~~Encrypt test secret for local environment~~ **COMPLETED - variables-cluster-secret.enc.yaml exists**
- [x] T023 ~~Create Nextcloud secret template~~ **COMPLETED - nextcloud/secret.yaml exists**
- [x] T024 ~~Encrypt Nextcloud secret for local~~ **COMPLETED - managed by existing structure**
- [x] T025 ~~Create variables base secret~~ **COMPLETED - variables-base-secret.enc.yaml exists**

## Phase 3.5: Application Configuration Validation

- [x] T026 ~~Homepage application configuration~~ **COMPLETED - k8s/bases/apps/homepage/ exists and included in kustomization**
- [x] T027 ~~Whoami test application~~ **COMPLETED - k8s/bases/apps/whoami/ exists and included**
- [x] T028 ~~Nextcloud application configuration~~ **COMPLETED - k8s/bases/apps/nextcloud/ exists and included**
- [ ] T029 Validate local cluster apps overlay includes all applications and works with KSail

## Phase 3.6: End-to-End Validation

- [ ] T030 [P] End-to-end GitOps workflow validation via KSail cluster operations
- [ ] T031 [P] Performance validation for cluster bootstrap (<10 minutes) via KSail timing
- [ ] T032 [P] Operational validation of secret encryption/decryption via kubectl
- [ ] T033 Update quickstart.md with validation results and operational notes
- [ ] T034 Create troubleshooting guide in docs/troubleshooting.md for common KSail/GitOps issues

## Dependencies

- Setup validation (T003-T004) before infrastructure validation
- Infrastructure validation (T005-T009) before application validation
- Application validation (T029) before end-to-end validation
- Most infrastructure is already complete - focus on operational verification
- Use KSail for all SOPS operations and cluster management

## Parallel Example

```bash
# Bootstrap and validate cluster (T005-T009):
ksail up    # This will bootstrap cluster with all configs

# Validate infrastructure components in parallel:
kubectl get nodes                          # Cluster health
kubectl get kustomizations -A             # Flux reconciliation
kubectl get secrets -A | grep sops        # SOPS decryption
kubectl get pods -A                       # Application deployment

# Use KSail for all cluster operations:
ksail down  # Clean shutdown when validation complete

# KSail handles SOPS operations automatically based on .sops.yaml
```

## Notes

- **Major Discovery**: Most infrastructure is already implemented and configured!
- [P] tasks = independent validation tasks, no shared dependencies
- **Use KSail for everything**: Cluster management, SOPS operations, Flux bootstrapping
- Focus on operational validation rather than test creation
- Age keys and SOPS configuration already complete in .sops.yaml
- TestKube available for future testing infrastructure when needed
- Validate functionality through actual cluster operations
- Commit after each validation phase completion
- Follow GitOps principles: all changes via Git commits (infrastructure already follows this)
- Constitutional compliance already built into existing structure

## Task Generation Rules

Applied during main() execution

1. **From Contracts**:
   - flux-reconciliation.md → T006 Flux reconciliation validation
   - ksail-bootstrap.md → T005 KSail bootstrap validation
   - secret-management.md → T007 Secret management validation

2. **From Data Model**:
   - GitOps Configuration Manifest → Most configurations already exist (marked COMPLETED)
   - Encrypted Secret → Secret infrastructure already exists (marked COMPLETED)
   - Environment Configuration → Environment overlays already exist (marked COMPLETED)
   - KSail Cluster Definition → T003 KSail operational validation
   - Policy Rule → **DEFERRED - Will be handled in separate policy enhancement specification**

3. **From Quickstart Scenarios**:
   - Cluster bootstrap → T005 KSail bootstrap validation
   - GitOps workflow → T008 End-to-end operational validation
   - Secret encryption → T007 SOPS operational validation

4. **Ordering**:
   - Setup validation → Infrastructure validation → Application validation → End-to-end validation
   - Focus on operational verification of existing infrastructure
   - Policy enhancements deferred to separate specification
   - TestKube available for future testing when infrastructure testing is needed

## Validation Checklist

GATE: Checked before execution

- [x] All contracts have corresponding validation tasks (T005-T007)
- [x] All entities have operational verification (most already exist)
- [x] All validation flows logically (Setup → Infrastructure → Applications → End-to-end)
- [x] Parallel tasks truly independent (different validation areas)
- [x] Each task specifies operational verification approach
- [x] No task conflicts with existing infrastructure
- [x] Constitutional compliance verified (GitOps-first, Security by Design, Infrastructure as Code, Observability)
- [x] Policy enhancement scope properly deferred to separate specification
- [x] Focus maintained on operational validation of existing GitOps solution
