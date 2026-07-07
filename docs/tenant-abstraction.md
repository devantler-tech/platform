# ADR: The Tenant abstraction is KRO

- **Status:** Accepted — records a decision already made and implemented.
- **Decision:** **KRO** (`ResourceGraphDefinition` + typed CRs) is the platform's Tenant/WebApp
  abstraction. The `Tenant` and `WebApp` RGDs are live; the remaining work under #1932 is to
  **enhance those KRO CRs to cover all tenant and platform needs**, not to switch technology.
- **Tracks:** [#1932](https://github.com/devantler-tech/platform/issues/1932) (AC #1). Follows the
  incremental DRY pass (#1927–#1931).

## Context

Every tenant used to duplicate a near-identical control-plane skeleton under `k8s/bases/apps/<tenant>/`
(namespace, Flux-impersonated `edit` ServiceAccount + RoleBinding, a cosign-verified `OCIRepository`
+ Flux `Kustomization`, a `ghcr-auth` `ExternalSecret`, and a `NetworkPolicy`), plus a provider patch,
with optional per-tenant branches (external-dns RBAC, SOPS-decrypted secrets). Onboarding a tenant was
a ~7-file copy, and the skeletons had begun to drift.

The platform has **already adopted KRO** to collapse that copy into one small typed declaration:

- `k8s/bases/infrastructure/controllers/kro/` installs the KRO controller.
- `k8s/bases/infrastructure/resource-graph-definitions/tenant/` defines the `Tenant` RGD — the core
  skeleton plus `externalDns` and `sops` `includeWhen` toggles.
- `k8s/bases/infrastructure/resource-graph-definitions/webapp/` defines the `WebApp` RGD (Deployment +
  Service + HTTPRoute, with replicas/probe/resource inputs) that composes onto a `Tenant` namespace.
- `k8s/providers/docker/apps/tenant-ascoachingogvaner.yaml` and `.../web-app-wedding-app.yaml` are the
  live tenant/app instances.

Onboarding is now a ~5-line CR:

```yaml
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata: { name: ascoachingogvaner }
spec:
  name: ascoachingogvaner
  externalDns: true
```

This ADR records **why KRO** (over the alternatives that were weighed) and frames the remaining #1932
work as enhancing the KRO CRs. It does **not** reopen the technology choice.

Any enhancement must preserve the isolation posture verbatim: cosign-verified OCI, the
Flux-impersonated namespaced `tenant-edit` SA (aggregated `edit`-minus-`pods/exec`, so tenants do not
trip Kubescape C-0002), the Kyverno-restricted namespaced Vault `SecretStore`, the default-deny
network floor, and ESO/SOPS-sourced secrets (never inlined into a Tenant spec). It must satisfy
`enforce-flux-best-practices` (Enforce) and stay validatable by `ksail workload validate`.

## Options considered (historical — the decision is KRO)

### KRO `Tenant`/`WebApp` RGDs (typed, build) — **adopted**

A KRO `ResourceGraphDefinition` is a typed, `kubectl`-native abstraction and matches the platform's
standing preference for **CRD + controller over Helm** for in-cluster composition. It expands a small
typed CR into the full resource graph, with `includeWhen` toggles for the optional branches — no Kustomize
overlay/component stacking. The pre-1.0 (`v1alpha1`) maturity is a known trade-off, managed by keeping
the RGDs render-diffable and the controller in the `infrastructure-controllers` layer with explicit
CRD-before-CR ordering (the `infrastructure` Kustomization `dependsOn` the controllers layer).

### Capsule (buy) — CNCF multi-tenancy operator — **rejected**

Capsule's `Tenant` CRD groups namespaces under an owner and auto-inherits ResourceQuota, LimitRange,
NetworkPolicy, and RBAC. It targets **owner-based, multi-namespace, self-service** tenancy; our tenants
are **single-namespace, machine-onboarded, Flux-impersonation-driven**. Capsule does not manage Flux
sources, so the two resources that carry most of the archetype's value *and* its security posture — the
cosign-verified `OCIRepository` and the impersonating Flux `Kustomization` — would stay hand-written
regardless, while Capsule adds a second controller and a policy model overlapping the existing Kyverno
`restrict-tenant-secret-stores` policy and the OpenBao role. Wrong model fit.

### Helm library chart (build, simplest) — **considered, not chosen**

A `tenant` Helm library chart templating the same resources behind one `HelmRelease` per tenant was a
lower-ceremony option that stays inside the Flux→HelmRelease model with no new controller. It was **not
chosen**: the platform has already committed to KRO as the typed abstraction, and a library chart would
be a technology switch *away* from that already-implemented choice — trading the typed
CRD/`kubectl`-native model (and the CRD OpenAPI + admission validation it enables) for JSON-schema on a
chart's `values`. KRO is the intended end-state and it is already live, so there is no reason to detour
through Helm.

## Decision

**KRO is the Tenant/WebApp abstraction.** The `Tenant` and `WebApp` RGDs are the chosen, implemented
approach. The remaining #1932 work is to **enhance the KRO CRs to cover all tenant and platform
needs**, not to reconsider the technology.

## Consequences

**Positive** — onboarding a tenant is one small typed CR; the drift is gone; the abstraction is typed
and `kubectl`-native with CRD-schema validation; no Helm/overlay stacking; the isolation posture is
templated verbatim.

**Trade-offs** — KRO is pre-1.0 (`v1alpha1`), so the RGDs stay render-diffable and the controller sits
in the platform chokepoint with explicit ordering; a CR of an RGD-defined kind cannot share a Flux
Kustomization with the controller that installs its CRD (already handled by the layer split).

**Follow-ups — enhance the KRO CRs (subsequent children of #1932).** Candidate gaps to close so the
`Tenant`/`WebApp` archetypes cover *all* tenant and platform needs (each to be filed and prioritised as
its own issue, behaviour-preservingly, render-diffed vs the current instances):

- **Arbitrary per-tenant `ExternalSecret`s beyond `ghcr-auth`** — e.g. `wedding-app`'s object-store /
  db-backup secrets — via a typed `secrets` list on the `Tenant` schema, so no tenant hand-writes an
  ExternalSecret.
- **Namespace `ResourceQuota` / `LimitRange`** — a standard multi-tenancy guardrail the RGD does not yet
  emit; add as an optional typed input.
- **Stateful `WebApp` shapes** — CNPG-backed apps (e.g. `wedding-db`) so the `WebApp` archetype covers
  database-backed workloads, not only stateless Deployments.
- **Onboarding docs** — keep [`TENANTS.md`](./TENANTS.md) in sync with the ~5-line CR onboarding as the
  schema grows.
