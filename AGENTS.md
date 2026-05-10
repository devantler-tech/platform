# AGENTS.md

This file provides project-specific conventions for AI agents (including [Repo Assist](https://github.com/githubnext/agentics/blob/main/docs/repo-assist.md)) working on this repository.

## Project Overview

This is a **GitOps-based Kubernetes platform** — not a traditional code repository. All "code" consists of Kubernetes YAML manifests managed with Kustomize overlays and deployed via Flux CD.

### Technology Stack

- **Flux CD** — GitOps engine reconciling from OCI artifacts
- **Kustomize** — manifest templating and overlays
- **Cilium** — CNI and Gateway API
- **Talos Linux** — immutable Kubernetes OS
- **KSail** — cluster lifecycle management (Docker for local, Hetzner for prod)
- **SOPS + Age** — secret encryption at rest

## Repository Structure

```
k8s/                  # All Kubernetes manifests
  bases/              # Shared base resources (never modify directly from overlays)
    infrastructure/   # Organized by resource type: controllers/, certificates/, gateway/, etc.
    apps/             # Application deployments
  providers/          # Provider-specific overlays (docker, hetzner)
  clusters/           # Per-environment overlays (local, prod)
talos-local/          # Talos machine config patches for Docker (local)
talos/                # Talos machine config patches for Hetzner (prod)
ksail.yaml            # KSail local cluster config
ksail.prod.yaml       # KSail production cluster config
```

See `.github/instructions/` for detailed conventions on:
- `kustomize-manifests.instructions.md` — Kustomize overlays, Flux dependency ordering, HelmRelease conventions
- `talos-patches.instructions.md` — Talos machine config patch structure
- `sops-secrets.instructions.md` — SOPS encryption workflow and key rules

## Validation

There is no traditional build/test/lint pipeline. Validate changes with:

```bash
# Validate Kustomize builds
kustomize build k8s/clusters/local/
kustomize build k8s/clusters/prod/

# Validate YAML syntax
kubectl apply --dry-run=client -f <file>
```

CI runs a full Talos+Docker cluster system test on PRs — this takes 3-5 minutes and cannot be run locally without Docker and KSail.

## Protected Files — Do Not Modify

- `*.enc.yaml` — SOPS-encrypted secrets (cannot be decrypted without the Age private key)
- `ksail.prod.yaml` — production cluster config (changes affect live infrastructure)
- `.sops.yaml` — encryption rules and Age public keys

## Conventions

- **Semantic commits** — Use conventional commit messages (e.g., `feat:`, `fix:`, `chore:`)
- **Draft PRs** — Always create PRs as drafts
- **Small, focused changes** — One concern per PR
- **Never commit plaintext secrets** — All secrets must be SOPS-encrypted with `.enc.yaml` suffix
- **Base files are immutable** — Use Kustomize `patches:` in overlays, never edit `k8s/bases/` directly from cluster or provider overlays
- **Flux dependency order** — `variables` → `infrastructure-controllers` → `infrastructure` → `apps`

## What's Useful for Repo Assist

- **Issue labelling and triage** — Very helpful
- **Issue investigation** — Investigate manifest misconfigurations, Helm chart issues, Flux sync problems
- **Engineering investments** — Helm chart version bumps (via HelmRelease `spec.chart.spec.version`), GitHub Actions updates
- **Manifest improvements** — Kustomize structure cleanup, documentation gaps, dead resource removal
- **Stale PR nudges** — Helpful for contributor PRs

## What's Less Applicable

- **Performance improvements** — Limited scope (K8s manifests, not application code)
- **Testing improvements** — No test suite; CI is a full cluster system test
- **Code refactoring** — Manifests are declarative YAML, not imperative code
