# ADR: Generalize the Tenant abstraction — Capsule vs KRO vs Helm library chart

- **Status:** Proposed
- **Decision:** Adopt a **Helm library chart** as the Tenant abstraction now; keep a
  **KRO `Tenant` RGD** as the typed end-state to revisit at KRO GA; **reject Capsule**.
- **Tracks:** [#1932](https://github.com/devantler-tech/platform/issues/1932) (AC #1). Follows the
  incremental DRY pass (#1927–#1931).

## Context

Every tenant duplicates a near-identical control-plane skeleton under `k8s/bases/apps/<tenant>/`
(namespace, Flux-impersonated `edit` ServiceAccount + RoleBinding, a cosign-verified `OCIRepository`
+ Flux `Kustomization`, a `ghcr-auth` `ExternalSecret`, and a `NetworkPolicy`), plus a provider
patch — `wedding-app` adds a `SecretStore` + SOPS block. The skeletons differ only by
name/namespace/OCI URL (and the SOPS block) and have already begun to drift (inconsistent
`app.kubernetes.io/managed-by` labels). Onboarding a tenant is a ~7-file copy.

The goal is **one declaration (~5 lines) per tenant** instead of the copy. Two hard constraints shape
the choice:

1. **No new Kustomize overlay/component stacking.** "Overlays on overlays" is the readability problem
   we are trying to escape — the reason we reach for an archetype at all.
2. **The `infrastructure-controllers` layer is the platform's chokepoint.** Our incident history
   (CRD-before-CR ordering, controller-availability wedges, merge-queue deploy-prod stalls) is
   concentrated there. Anything that adds a controller/CRD lands in exactly that layer.

Any solution must also preserve the isolation posture verbatim: cosign-verified OCI, the
Flux-impersonated namespaced `edit` SA, the Kyverno-restricted namespaced Vault `SecretStore`, the
default-deny network floor, and ESO/SOPS-sourced secrets (never inlined into a tenant spec). It must
satisfy `enforce-flux-best-practices` (Enforce) — any templated `HelmRelease`/`Kustomization` carries
`interval`/`timeout`/retries — and stay validatable by `ksail workload validate` (which builds every
kustomization standalone).

## Options considered

### 1. Capsule (buy) — CNCF multi-tenancy operator — **rejected**

Capsule's `Tenant` CRD groups namespaces under an owner and auto-inherits ResourceQuota, LimitRange,
NetworkPolicy, and RBAC, with namespace self-provisioning. It is mature and purpose-built.

It is the wrong model fit here. Capsule targets **owner-based, multi-namespace, self-service**
tenancy; our tenants are **single-namespace, machine-onboarded, Flux-impersonation-driven**. Capsule
does not manage Flux sources, so the two resources that carry most of the archetype's value *and* its
security posture — the **cosign-verified `OCIRepository`** and the impersonating Flux `Kustomization` —
stay hand-written regardless. Capsule would own only namespace + RBAC + netpol + quota (a minority of
the ~7 files), while **adding a controller to the `infrastructure-controllers` chokepoint** and a
second policy model that overlaps the existing Kyverno `restrict-tenant-secret-stores` policy and the
OpenBao role. Cost outweighs benefit.

### 2. KRO `Tenant` RGD (build, typed) — **deferred to KRO GA**

A KRO `ResourceGraphDefinition` is the ideal typed, `kubectl`-native abstraction and matches the
platform's standing preference for **CRD + controller over Helm** for in-cluster composition. But KRO
is **pre-1.0 (`v1alpha1`)**: adopting it means a new alpha controller plus dynamic CRDs in the exact
`infrastructure-controllers` layer that keeps wedging. The maturity/risk is not justified for the
first cut of a foundational archetype. This remains the intended end-state — **revisit once KRO ships
a stable (≥ v1 / GA) release**.

### 3. Helm library chart (build, simplest) — **adopted**

A `tenant` [library chart](https://helm.sh/docs/topics/library_charts/) templates the ~7 resources
behind one `HelmRelease` per tenant, driven by a ~5-line values block
(name / namespace / OCI URL / optional SOPS). It:

- stays **100% inside the existing Flux → HelmRelease reconcile/prune model** — **no new controller**
  in the chokepoint;
- adds **no** Kustomize overlay/component stacking — it *replaces* the per-tenant kustomize copy with
  one HelmRelease (constraint 1 satisfied);
- **preserves the isolation posture verbatim** (cosign OCI, Flux impersonation, Vault scoping,
  default-deny netpol, ESO/SOPS secrets) because it templates the same resources unchanged;
- satisfies `enforce-flux-best-practices` (the templated resources carry interval/timeout/retries);
- is **reversible** and **forward-compatible with KRO** — the same declarative inputs map onto an RGD
  schema later, so choosing it now does not foreclose the typed end-state.

## Decision

**Adopt the Helm library chart** as the Tenant abstraction. It is the lowest-risk path that meets the
goal and both hard constraints today, while keeping the door open to the typed KRO end-state.

## Consequences

**Positive** — onboarding a tenant becomes one small `HelmRelease`; the drift disappears; no new
controller or CRD enters the chokepoint; the full isolation posture is preserved; the change is
reversible; and the inputs stay KRO-compatible.

**Trade-offs** — a library chart is less "typed" than a CRD (input validation is JSON-schema on the
chart's `values.schema.json`, not a CRD OpenAPI + admission); `ksail workload validate` must render the
templated `HelmRelease` (it already validates HelmReleases); and the sibling **WebApp** archetype
should *compose with* this chart, not duplicate it.

**Follow-ups (subsequent children of #1932)** — (a) build the `tenant` library chart + its
`values.schema.json`; (b) pilot-migrate one tenant (`ascoachingogvaner`, no SOPS) behavior-preservingly
(render-diff + `ksail workload validate` unchanged vs baseline) — AC #2; (c) migrate `wedding-app`
(adds the SOPS block); (d) document the ~5-line onboarding in [`TENANTS.md`](./TENANTS.md).
