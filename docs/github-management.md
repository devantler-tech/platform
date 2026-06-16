# Declarative GitHub management

The devantler-tech GitHub org is managed declaratively as GitOps: GitHub
repositories (and, over time, rulesets, labels, branch protection, …) are
**Crossplane managed resources** reconciled by
[provider-upjet-github](https://github.com/crossplane-contrib/provider-upjet-github)
against the GitHub API. Change a manifest, merge, and Flux + Crossplane converge
GitHub to it — including reverting out-of-band UI edits.

The desired state does **not** live in this repo. It lives in
[`devantler-tech/.github`](https://github.com/devantler-tech/.github) under
`deploy/`, onboarded here as a **normal app** (`github-config`) using the same
pattern as every other tenant (cosign-verified OCI artifact → namespace-scoped
`Kustomization`) — only the artifact carries managed resources instead of a
workload. There is **no** bespoke Flux Kustomization for GitHub; it rides the
existing `apps` layer.

## Why an app, and why namespaced

GitHub's managed resources ship in two flavours: cluster-scoped
(`repo.github.upbound.io`) and **namespaced** (`repo.github.m.upbound.io`,
Crossplane v2). We use the **namespaced** variants. That choice is what lets
`github-config` be a *genuinely isolated* app: a namespaced managed resource can
be applied by a namespace-scoped `ServiceAccount` with a plain `Role` — no
`ClusterRoleBinding`, no cluster RBAC. The app's authority to touch GitHub is
confined to its own namespace and is **not** aggregated into the built-in `edit`
ClusterRole, so no other tenant inherits it.

## Architecture

Three tiers, each in its conventional place — controller in `controllers/`, its
CRs one tier later in `infrastructure/`, the workload in `apps/`:

| Piece | Where | Notes |
|---|---|---|
| Crossplane core | `k8s/providers/hetzner/infrastructure/controllers/crossplane/` | HelmRelease; prod-only. `provider.defaultActivations: []` keeps unused provider CRDs inactive. Installs the pkg.crossplane.io CRDs. |
| Provider + activation policy | `k8s/providers/hetzner/infrastructure/crossplane/` | The `Provider` package + `DeploymentRuntimeConfig` + `ManagedResourceActivationPolicy` (namespaced MRDs only). One tier after the controller (needs its CRDs), like Coroot/Flagger CRs. Establishes the namespaced github CRDs. |
| The `github-config` app | `k8s/bases/apps/github-config/` | Standard app onboarding: namespace, SA, least-privilege `Role`, GitHub App + ghcr credential `ExternalSecret`s, the credential seed `PushSecret`, the namespaced `ProviderConfig`, and the cosign-verified `OCIRepository` + `Kustomization`. Applied by the existing `apps` Flux Kustomization. Prod-only in practice (the docker provider deploys no apps). |
| Desired state | [`devantler-tech/.github`](https://github.com/devantler-tech/.github) `deploy/` | The actual `Repository`/ruleset/label managed resources. Published as a cosign-signed OCI artifact to `ghcr.io/devantler-tech/github-config/manifests` on `v*` tags by the shared `publish-manifests` reusable workflow. |
| Credentials | OpenBao `infrastructure/github/app` | GitHub App (`app_id`, `installation_id`, `pem`), seeded from the SOPS `variables-cluster` secret by the app's `PushSecret`, materialized into `github-config/github-app-credentials` by an `ExternalSecret` that renders the provider's JSON credential format. |

Ordering is the normal chain: `apps` `dependsOn` `infrastructure`, so by the
time the app reconciles, the provider (infrastructure tier) has installed the
namespaced github CRDs the `ProviderConfig` and managed resources need. On a
fresh install the app's `Kustomization` retries benignly until those CRDs exist.

The credential seed `PushSecret` lives **with the app** (apps layer), not in
`infrastructure/vault-seed/`: until the GitHub App keys are in `variables-cluster`
it stalls, and the apps layer is the leaf of the dependency chain — so a stall
can never hold `infrastructure` (and through it every deploy) red.

## Credential setup (one-time, maintainer)

1. Create a **GitHub App** on the devantler-tech org (Settings → Developer
   settings → GitHub Apps): permissions *Repository: Administration
   (read/write), Contents (read)* and *Organization: Administration
   (read/write)* to start — widen as more resource kinds come under
   management. Install it on **all repositories** of the org. No webhook.
2. Add three keys to the **prod** `variables-cluster` SOPS secret
   (`k8s/clusters/prod/bootstrap/variables-cluster-secret.enc.yaml`):
   `github_app_id`, `github_app_installation_id`, and `github_app_pem`
   (the private key, as a normal multiline PEM block). Do this *before* the
   first rollout so the seed `PushSecret` doesn't sit red.
3. Merge. The PushSecret seeds OpenBao, the ExternalSecret renders the provider
   credential into `github-config`, and the app reconciles once `.github` has
   published its first `v*` artifact.

## Adopting an existing repository (Observe-first)

Desired-state manifests live in `devantler-tech/.github` under `deploy/`. Never
let Crossplane *create* what already exists — **bind** to it:

1. In `.github`, add `deploy/repositories/<name>.yaml`: a namespaced
   `Repository` (`apiVersion: repo.github.m.upbound.io/v1alpha1`), external-name
   annotation = the repo name, `managementPolicies: ["Observe"]`,
   `providerConfigRef: {kind: ProviderConfig, name: default}`. (Namespaced MRs
   have no `deletionPolicy`; "never delete" is expressed by omitting `Delete`
   from `managementPolicies`.)
2. Tag a `v*` release of `.github`; wait for the managed resource to become
   Ready and inspect what Crossplane observed:
   `kubectl -n github-config get repository <name> -o yaml` →
   `status.atProvider` is the live GitHub state.
3. Backfill `spec.forProvider` from `status.atProvider` (the fields you want to
   pin: `visibility`, `hasIssues`, `deleteBranchOnMerge`, …).
4. Promote `managementPolicies` to the full set **except `Delete`**
   (`["Observe", "Create", "Update", "LateInitialize"]`). From now on GitHub
   follows the manifest, drift included, but deleting the CR never deletes the
   real repository.

Roll the org over incrementally: one or two repos per release, watch them
reconcile, then batch the rest.

## Adding more GitHub resource kinds

1. Activate the namespaced MRD in
   `k8s/providers/hetzner/infrastructure/crossplane/managed-resource-activation-policy.yaml`
   (plural.group form, e.g. `repositoryrulesets.repo.github.m.upbound.io`); add
   its API group to the app `Role` in `k8s/bases/apps/github-config/rbac.yaml`
   if it is a new group.
2. Add the managed resources in `.github`'s `deploy/` — same Observe-first flow
   where an external object already exists. Each active MRD costs the apiserver
   ~3 MiB; activate only what is used.

## Safety rails

- **Never `Delete`** — managed resources omit `Delete` from
  `managementPolicies`, so neither CR deletion nor a Flux prune can ever delete
  a real GitHub repository.
- **Observe-first adoption** — a new managed resource for an existing object
  always starts `managementPolicies: ["Observe"]` (read-only).
- **Least privilege** — the app SA's authority is a namespaced `Role` over the
  GitHub MR API groups only, never aggregated into `edit`.
- The GitHub App credential is org-scoped and short-lived per request; the
  provider pod's egress is FQDN-pinned to `api.github.com` by the
  `crossplane-system` CiliumNetworkPolicy.
