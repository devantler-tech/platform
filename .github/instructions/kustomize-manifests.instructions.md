---
description: "Use when editing Kubernetes manifests, kustomization.yaml files, HelmRelease resources, or GitOps configurations in the k8s/ directory. Covers kustomize overlay conventions, Flux dependency ordering, and resource organization patterns."
applyTo: "k8s/**/*.yaml"
---
# Kustomize & GitOps Manifest Conventions

## Overlay Hierarchy

```
k8s/clusters/<env>/   → per-environment (cluster-meta ConfigMap, bootstrap variables)
k8s/providers/<provider>/ → provider-specific assembly (patches, extra resources)
k8s/bases/             → shared base resources (never modified by overlays in-place)
```

- **Never modify base files** from cluster or provider overlays — use `patches:` in kustomization.yaml instead.
- Cluster overlays reference `../base`, set the `cluster-meta` ConfigMap data, and apply environment-specific patches (e.g. Flux Kustomization timeout overrides).
- Provider overlays import bases via relative `resources:` and add provider-specific patches or extra resources.

## Resource Organization (`k8s/bases/infrastructure/`)

Resources are organized by **resource type**, not by component:
- `controllers/` — HelmRelease + HelmRepository (subdirectory per component)
- `certificates/` — Certificate resources
- `cluster-policies/` — ClusterPolicy resources
- `gateway/` — Gateway and HTTPRoute resources
- `alerts/` — Alert and Provider resources

## Flux Dependency Chain (strict order)

1. `bootstrap` — substitution variables (ConfigMaps + Secrets) and PriorityClasses (no dependencies)
2. `infrastructure-controllers` — Helm controllers (depends on: bootstrap)
3. `infrastructure` — Core infra resources (depends on: infrastructure-controllers)
4. `apps` — Applications (depends on: infrastructure)

All Flux Kustomizations reference `spec.decryption.secretRef.name: sops-age` for SOPS decryption.

## HelmRelease Conventions

- HelmRelease and HelmRepository live together in `controllers/<component>/`
- Provider-specific value overrides use strategic merge patches in `providers/<provider>/infrastructure/controllers/<component>/patches/`
- Chart versions are managed by Renovate (automerge for minor/patch)

## Adding New Components

See [docs/TEMPLATING.md](../../docs/TEMPLATING.md) for the full checklist.
