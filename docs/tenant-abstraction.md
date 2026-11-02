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

### Tenant-owned WebApps: opt-in permission and ownership boundary

`Tenant.spec.webApp` defaults to `false`. Setting it to `true` adds a namespaced
RoleBinding to the dedicated, nonaggregated `tenant-webapp-edit` ClusterRole. It
grants CRUD on `kro.run/webapps` only; it does not grant status, Tenant/RGD, RBAC,
impersonation or exec access. Existing tenants are not opted in by this change.

Tenant-owned WebApps must use their namespace as both parent name and
`spec.name`, which selects the generated workload namespace. That target is
immutable. Only KRO may establish/change reserved KRO/ApplySet metadata; its
parent writes may maintain metadata/status but not change application spec.
Tenant writers must preserve KRO's cleanup finalizer as well as reserved labels
and annotations; they cannot force orphaning by removing that finalizer.
The existing platform-owned `flux-system/wedding-app` pilot is an exact
exception, not a wildcard namespace exemption.

KRO 0.9.2 identifies children through ApplySet and `kro.run/instance-*` labels,
not WebApp owner references. Child admission therefore checks their complete
placement/identity and refuses updates that would adopt an unlabelled or
foreign-owned Deployment, Service or HTTPRoute. Non-controller writers must
preserve reserved ownership labels. Controller updates may change provenance
such as `kro.run/kro-version`, while instance/node/ApplySet identity remains fixed.
Existing hostname, secret-store and
network admission remain in force; KRO has no hostname bypass.

KRO's pinned deletion path does not itself verify child ownership. Deletion
admission additionally requires complete old child ownership, a read of the
labelled parent's matching UID, and that parent's deletion timestamp. This
prevents failed collision/rollback from deleting an unowned workload or another
active parent's child. The admission controller gains only `get` on WebApps for
that check; missing/refused reads fail closed. Non-controller writers may delete
ordinary unowned workloads, while owned child cleanup remains controller-only.
The current graph has three
unconditional children; future pruning under an active parent requires a
separately reviewed authorization change.

`scripts/tests/test-tenant-webapp-boundary.sh` tests the actual policies with
the pinned Kyverno compiler/engine. Its request adapter supplies distinct
old/new objects and explicit status subresources: CLI 1.19.1's CEL adapter
copies the new object into the old one on UPDATE and drops subresources, so
CLI UPDATE fixtures alone cannot establish these protections. The regular
Kyverno fixture census is retained for CREATE/DELETE selection and denial
coverage. Cross-resource deletion controls feed fictional parent snapshots to
the upstream engine; they do not prove live API authorization or convergence.

These are source/admission controls, not proof that a signed tenant artifact
has reconciled through a live controller. Issue #4897 retains that runtime
acceptance. No business-site Tenant, portal activation, database, hostname or
customer-facing routing is introduced by this foundation. Private-input
projection (#4854) and the business repository's signed manifest (#102 there)
remain separate prerequisites.

### Confined public runtime inputs

WebApp accepts opt-in literal arguments (at most 32, each at most 256 UTF-8
bytes), a non-root UID and primary GID (1–2147483647), a read-only-root option,
and at most one same-namespace Secret projection. The projection selects 1–8
unique keys and unique single-component destinations. Its directory is fixed
at `/run/webapp-inputs`, with required input, mode `0440` and read-only mounting;
there is no path, namespace, executable, environment or arbitrary volume override.
Arguments reject NUL and Kubernetes `$(...)` expansion; they must contain no
credentials. Secret values remain outside source, images, arguments and receipts.
The WebApp writer chooses the same-namespace Secret reference. This is workload
authority: its chosen image can consume selected keys even without Secret API
`get` permission. Grant WebApp editing only to trusted tenant workload writers;
do not place higher-privilege shared secrets in that tenant namespace. The fixed
directory and selected keys are confinement, not a new secret-authorization boundary.

Omitted inputs preserve empty argument/mount/volume lists, UID/GID 1000 and the
existing writable root. The Pod now explicitly derives its file group from its
primary GID, including that compatibility default. Current pilots have no graph
volumes and the normal platform mutation already supplies group 1000, but this
is not a byte-identical Pod template: existing instances may roll on reconciliation.
Do not generalize that permissions are unchanged for injected or future volumes.
The restricted business image opts into UID/GID 65532 and read-only root, so a
root-owned projected file is readable by exactly its non-root application group.
The public disclosure is an immutable startup snapshot: rotation requires a restart.

The platform source scanner refuses unknown or malformed WebApp spec fields
before sealing committed-instance evidence. The existing strict JSON parser
rejects duplicate members before jq can keep a last value; the maintained YAML
decoder also checks the original source before conversion. Complete inputs are
bounded to 1 MiB, with at most 1024 raw YAML documents. This is the platform's committed
source gate, not proof of a future tenant publisher's input validation. The actual
KRO 0.9.2 compiler/generated API tests admission, defaults, literal arguments and
resolved children. Resolved deployments are separately scanned with Trivy 0.74.0;
the writable-root and low-ID compatibility findings remain explicit rather than
disappearing behind expressions. Both resolved states pass the actual container
security policy, whose missing/false/string controls remain denied.

The required hosted fixture uses a disposable kind node (Kubernetes 1.36.4,
not the compiler fixture's 1.36.2 API server), one pinned KRO controller and the
signed source-owned public business image. It tests controller input transitions,
real process identity, kubelet atomic projection permissions, fictional EN/DA
disclosure and closed private routes. It has no platform credentials, gateway,
production policies, database or identity provider. Passing it is not production
ingress, tenant authorization, portal activation or recovery proof. The script
refuses execution outside an isolated GitHub-hosted Linux runner.
Removing required disclosure changes the template but does not revoke an already
healthy Pod: the zero-unavailable rolling strategy can retain it while replacement
startup fails. The fixture explicitly scales to zero before cleanup; this is not
a new production revocation mechanism.

**Positive** — onboarding a tenant is one small typed CR; the drift is gone; the abstraction is typed
and `kubectl`-native with CRD-schema validation; no Helm/overlay stacking; the isolation posture is
templated verbatim.

### Public business-site staging hold

The platform installs `business-site-staging-hold` before enabling the public
business application. Tenant writers cannot create, update or delete that named
Cilium policy in `business-site`, including through reconciliation or pruning.
The existing trusted platform-controller carve-outs remain; a controller name
in a tenant namespace does not grant the same authority. Other policy names and
the same name in other namespaces retain their ordinary admission rules.

Reserved policy names do not inherit an exception for an already noncompliant
old object: platform creation is not permission for a tenant's later update.
This ownership boundary is separate from the hold's actual traffic effect.
Routing, projection and live hold/serve/isolate/recover acceptance remain under
#5247 before exposure; the source-only baseline does not enable an application.

### Public business-site configuration identity

The maintained `vault-config` Job declares the `business-site` Kubernetes auth
role for the same-named service account in only that namespace. Its sole token
policy reads the exact KV-v2 disclosure entry used by the source-owned
SecretStore; it grants no value writes, metadata listing or other application
paths. The only other permissions are lookup and revocation of the token itself,
which the pinned ESO client uses when reusing and closing its authenticated
connection. These do not authorize lookup or revocation of another token. The
default token policy is excluded and the lifetime is capped at one hour.
The role uses the source store's existing Kubernetes token audience behavior;
it does not guess a separate audience absent from that store. The pinned ESO
2.13.0 Kubernetes connector passes the store's service-account audiences through
to TokenRequest, without the `vault` default of its separate JWT connector.

The tenant owner materializes and maintains the confirmed disclosure privately
through the supported operator entry path. This role creates no value, enables
no WebApp and grants no customer identity or payment access. Missing/unsafe
projection refusal, actual ESO authentication and rotation, HTTPS staging and
private-route closure remain live acceptance under #5247. Normal production
reconciliation owns installation; a suspended deployment is not manually bypassed.

`bash scripts/tests/test-business-disclosure-reader.sh` checks the actual rendered
role/policy payload and twenty-two invalid-payload controls without authenticating or
reading any value. OpenBao supports the policy's native
[JSON syntax](https://openbao.org/docs/concepts/policies/); the
[Kubernetes role API](https://openbao.org/docs/api/auth/kubernetes/) defines the
typed identity and token fields. The bounded payload checker reuses the platform's
strict JSON parser to refuse duplicate members and additional documents before
decoding; its unit controls run with `go test ./scripts/check-business-disclosure-reader`.
Static checks are not live authorization proof.

**Trade-offs** — KRO is pre-1.0 (`v1alpha1`), so the RGDs stay render-diffable and the controller sits
in the platform chokepoint with explicit ordering; a CR of an RGD-defined kind cannot share a Flux
Kustomization with the controller that installs its CRD (already handled by the layer split).

**Follow-ups — enhance the KRO CRs (subsequent children of #1932).** Candidate gaps to close so the
`Tenant`/`WebApp` archetypes cover *all* tenant and platform needs (each to be filed and prioritised as
its own issue, behaviour-preservingly, render-diffed vs the current instances):

- **Arbitrary per-tenant `ExternalSecret`s beyond `ghcr-auth`** — e.g. `wedding-app`'s object-store /
  db-backup secrets — via a typed `secrets` list on the `Tenant` schema, so no tenant hand-writes an
  ExternalSecret. Needs kro's `forEach` collections (one `ExternalSecret` per declared entry) plus a
  nested CEL `.map()` to build each one's `data:` list — kro has no offline CEL validator, so this
  needs a live-cluster verification pass before it ships (parked on #2521).
- ~~**Namespace `ResourceQuota` / `LimitRange`**~~ — **done** (#2522): optional `resourceQuota` /
  `limitRange` inputs, each with its own `enabled` toggle (mirrors the existing `externalDns`/`sops`
  boolean pattern — no `forEach`/CEL `.map()` needed, so this one *is* safely static-validatable).
  Default `enabled: false` on both ⇒ no behaviour change for existing tenants.
- **Stateful `WebApp` shapes** — CNPG-backed apps (e.g. `wedding-db`) so the `WebApp` archetype covers
  database-backed workloads, not only stateless Deployments (#2523).
- **Onboarding docs** — keep [`TENANTS.md`](./TENANTS.md) in sync with the ~5-line CR onboarding as the
  schema grows. `TENANTS.md` still documents only the pre-KRO hand-copy flow — reconciling it with the
  KRO `Tenant`/`WebApp` CR path (this ADR) is itself an open follow-up.
