# Declarative GitHub management

The devantler-tech GitHub org is managed declaratively from this repo:
GitHub repositories (and, over time, rulesets, labels, branch protection, …)
are **Crossplane managed resources** reconciled by
[provider-upjet-github](https://github.com/crossplane-contrib/provider-upjet-github)
against the GitHub API. Change a manifest, merge, and Flux + Crossplane
converge GitHub to it — including reverting out-of-band UI edits.

## Architecture

| Piece | Where | Notes |
|---|---|---|
| Crossplane core | `k8s/providers/hetzner/infrastructure/controllers/crossplane/` | HelmRelease; prod-only (external reconcilers must run on exactly one always-on cluster). `provider.defaultActivations: []` keeps unused provider CRDs inactive. |
| Provider + org state | `k8s/providers/hetzner/github/` | Provider package, `ManagedResourceActivationPolicy`, `ProviderConfig`, credentials wiring, and `repositories/` (one file per repo). |
| Flux wiring | `k8s/clusters/prod/github-flux-kustomization.yaml` | Dedicated `github` Flux Kustomization, `dependsOn: infrastructure`. Isolated so a GitHub-side stall never holds `infrastructure`/`apps` red. |
| Credentials | OpenBao `infrastructure/github/app` | GitHub App (`app_id`, `installation_id`, `pem`), seeded from the SOPS `variables-cluster` secret by a PushSecret, materialized into `crossplane-system/github-app-credentials` by an ExternalSecret that renders the provider's JSON credential format. |

The layering follows the repo's Flagger/Coroot convention: the controller
(HelmRelease) ships one layer earlier than the CRs that need its CRDs.

## Credential setup (one-time, maintainer)

1. Create a **GitHub App** on the devantler-tech org (Settings → Developer
   settings → GitHub Apps): permissions *Repository: Administration
   (read/write), Contents (read)* and *Organization: Administration
   (read/write)* to start — widen as more resource kinds come under
   management. Install it on **all repositories** of the org. No webhook.
2. Add three keys to the **prod** `variables-cluster` SOPS secret
   (`k8s/clusters/prod/bootstrap/variables-cluster-secret.enc.yaml`):
   `github_app_id`, `github_app_installation_id`, and `github_app_pem`
   (the private key, as a normal multiline PEM block).
3. Merge. The PushSecret seeds OpenBao, the ExternalSecret renders the
   provider credential, and the `github` Flux Kustomization turns Ready.
   Until step 2 lands, that Kustomization sits red — expected, and isolated.

## Adopting an existing repository (Observe-first)

Never let Crossplane *create* what already exists — **bind** to it:

1. Add `repositories/<name>.yaml` copying an existing file: external-name
   annotation = the repo name, `managementPolicies: ["Observe"]`,
   `deletionPolicy: Orphan`, empty `forProvider`.
2. Merge; wait for the MR to become Ready and inspect what Crossplane
   observed: `kubectl get repository.repo.github.upbound.io <name> -o yaml`
   → `status.atProvider` is the live GitHub state.
3. Backfill `spec.forProvider` from `status.atProvider` (only the fields you
   want to pin — unset fields are left alone by the provider's late
   initialization, but be explicit about the ones that matter:
   `visibility`, `hasIssues`, `deleteBranchOnMerge`, …).
4. Flip `managementPolicies` to `["*"]`. From now on GitHub follows the
   manifest, drift included. Keep `deletionPolicy: Orphan` — deleting the CR
   (or a Flux prune) must orphan, never delete, the real repository.

Roll the org over incrementally: one or two repos per PR, watch them
reconcile, then batch the rest.

## Adding more GitHub resource kinds

1. Activate the MRD in
   `k8s/providers/hetzner/github/managed-resource-activation-policy.yaml`
   (plural.group form, e.g. `repositoryrulesets.repo.github.upbound.io`).
2. Add the CRs — same Observe-first flow where an external object already
   exists. Each active MRD costs the apiserver ~3 MiB; activate only what is
   used.

## Safety rails

- **`deletionPolicy: Orphan` on every repository MR, always** — even fully
  managed ones. CR deletion must never cascade to GitHub.
- **Observe-first adoption** — a new MR for an existing object never starts
  with write policies.
- The GitHub App credential is org-scoped and short-lived per request; the
  provider pod's egress is FQDN-pinned to `api.github.com` by the
  `crossplane-system` CiliumNetworkPolicy.
