# Supply-chain verification (platform OCI artifact)

This documents the **consumer-side verification** half of the platform's
supply-chain story: making the cluster *reject* a platform OCI artifact that
was not signed by our own CI. It is the planned follow-up that
[#1628](https://github.com/devantler-tech/platform/pull/1628) explicitly left
out of scope, and the reason the kubescape `C-0237` control is still excepted in
[`security-exceptions/image-verification.yaml`](../k8s/bases/infrastructure/security-exceptions/image-verification.yaml).

> **TL;DR.** Signing is done; enforcement is **blocked on KSail**, which owns the
> root `flux-system` `OCIRepository` and doesn't yet expose Flux's `spec.verify`.
> The clean fix is a small KSail feature; the interim in-repo override is
> possible but carries a real prod-GitOps-freeze risk and can't be validated
> anywhere but prod. This doc records the exact config and a safe path so it can
> be executed deliberately rather than rediscovered.

---

## Where we are

| Stage | Status |
| --- | --- |
| **Sign** the platform OCI artifact (tag deploys) | ✅ Done — [#1628](https://github.com/devantler-tech/platform/pull/1628), cosign keyless, Fulcio + Rekor |
| **Sign** it on the merge-queue deploy path too | ✅ Done — [#1694](https://github.com/devantler-tech/platform/pull/1694) |
| SBOM + SLSA provenance attestation (tag deploys) | ✅ Done — part of #1628 |
| **Verify** the signature at pull time (reject unsigned) | ❌ **Not enforced** — this doc |

Today any artifact at `ghcr.io/devantler-tech/platform/manifests:latest` is
pulled and applied by Flux **without checking the signature**. The signature
exists and is publicly verifiable; nothing on the cluster *requires* it. Closing
that means putting a `spec.verify` on the `OCIRepository` that drives the root
Flux Kustomizations.

## The config that would enforce it

Flux's `OCIRepository` supports cosign keyless verification directly:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  # ... url / ref / interval / secretRef exactly as KSail creates them ...
  verify:
    provider: cosign
    matchOIDCIdentity:
      # Issuer: GitHub Actions' OIDC provider.
      - issuer: ^https://token\.actions\.githubusercontent\.com$
        # Subject: this repo's cd.yaml (tag deploys) OR ci.yaml (merge-queue
        # deploys). Tight on repo+workflow, broad on the git ref — cd signs from
        # refs/tags/v*, ci signs from refs/heads/gh-readonly-queue/main/*.
        subject: ^https://github\.com/devantler-tech/platform/\.github/workflows/(cd|ci)\.yaml@refs/.*$
```

`matchOIDCIdentity` is the whole security policy: it says "only trust artifacts
signed by Fulcio certs whose identity is *this repo's* `cd.yaml` or `ci.yaml`
workflow." A signature from any other identity (or no signature) makes the source
`NotReady` and Flux stops applying it.

> Both `issuer` and `subject` are RE2 regexes. The `(cd|ci)` alternation is why
> [#1694](https://github.com/devantler-tech/platform/pull/1694) (sign on the
> merge-queue path) **must** land first — otherwise merge-queue deploys would
> publish artifacts with no matching identity and freeze the cluster.

## Why it isn't a one-line PR — the blocker

The root source is an `OCIRepository` named **`flux-system`**, referenced by all
four Flux Kustomizations
([`bootstrap`](../k8s/bases/cluster/bootstrap-flux-kustomization.yaml),
`infrastructure-controllers`, `infrastructure`, `apps`). Three things make
adding `verify` to it hard:

1. **KSail owns it, and it isn't in this repo.** KSail creates the
   `flux-system` `OCIRepository` during `cluster create` from
   `ksail.prod.yaml`'s `spec.cluster.localRegistry`
   (`…@ghcr.io/devantler-tech/platform/manifests`, tag `latest`). Its full spec
   — `interval`, `layerSelector`, and especially the **pull `secretRef`** KSail
   generates from those registry credentials — lives only in the cluster. A
   hand-written override has to reproduce all of it; get the `secretRef` wrong
   and the source can't pull at all.

2. **`FluxInstance.spec.sync` has no `verify` field.** The in-repo
   [FluxInstance](../k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml)
   override (which pins the distribution and tunes the controllers) is the
   natural place to express this, but the flux-operator `FluxInstance` schema's
   `sync` block exposes only `provider` and `pullSecret` — **no `verify`**. So
   verification can't be configured through the resource we already own.

3. **It's the bootstrap source, and it's prod-only.** A wrong `verify` (bad
   identity regex, unsigned artifact) makes `flux-system` `NotReady`, which
   stops **every** Kustomization — a GitOps freeze the cluster cannot self-heal
   from (cf. the 2026-05-28 worker-2 deadlock). And it can only be exercised in
   **prod**: local/CI artifacts are pushed **unsigned** (only `cd.yaml` /
   `ci.yaml` sign), so there is no local or CI cluster where this can be
   validated first.

## Recommended path

### 1. Primary — fix it in KSail (clean, safe)

Since KSail owns the `OCIRepository`, KSail should own its verification too. The
ask: a `ksail.yaml` field (e.g. `spec.cluster.localRegistry.verify` /
`gitOps.verify`) that makes KSail render `spec.verify.provider: cosign` +
`matchOIDCIdentity` onto the `OCIRepository` it generates. Then this repo sets
two regexes in `ksail.prod.yaml` and KSail handles the rest — no in-repo
override, no spec drift, no ownership fight.

**Action:** file a feature request on `devantler-tech/ksail` (none exists as of
this writing). Reference this doc and #1628.

### 2. Interim — in-repo `OCIRepository` override (only if needed sooner)

If verification is wanted before KSail ships the feature, declare the
`flux-system` `OCIRepository` in `clusters/prod/bootstrap` with the `verify`
block above. Treat it as a deliberate, supervised prod change:

**Prerequisites**

- [#1694](https://github.com/devantler-tech/platform/pull/1694) merged (both
  deploy paths sign).
- Read the live spec first: `kubectl -n flux-system get ocirepository flux-system -o yaml`
  and reproduce `url`, `ref`, `interval`, `layerSelector`, and `secretRef`
  **exactly**, adding only `verify`.

**Rollout**

1. Apply during a window where you can watch
   `kubectl -n flux-system get ocirepository flux-system -w`.
2. Confirm it reconciles `Ready=True` with a `verified` message
   (`cosign verified signature …`), not a verification error.
3. Confirm the four Kustomizations stay `Ready`.

**Rollback**

- Revert the PR (drop `verify`); on the next reconcile the source is unverified
  again and GitOps resumes. Keep the revert one click away.
- If GitOps is already frozen and can't pull the revert, patch the live resource
  directly: `kubectl -n flux-system patch ocirepository flux-system --type=json -p '[{"op":"remove","path":"/spec/verify"}]'`.

**Watch for** KSail re-asserting its own (verify-less) `OCIRepository` on the
next `cluster update` / `workload push` and overwriting the in-repo one — the
ownership fight that makes option 1 the real fix.

## Cleanup once enforced

Remove the kubescape exception that documents this gap —
[`security-exceptions/image-verification.yaml`](../k8s/bases/infrastructure/security-exceptions/image-verification.yaml)
(`C-0237`) — so the control scores green instead of being ignored.

## Scope note

This is about the **platform** artifact only. Tenant apps
([`wedding-app`](../k8s/bases/apps/wedding-app/sync.yaml),
[`ascoachingogvaner`](../k8s/bases/apps/ascoachingogvaner/sync.yaml)) pull their
own `OCIRepository`s from separate repos that this platform doesn't sign;
verifying those is a per-tenant decision, out of scope here.
