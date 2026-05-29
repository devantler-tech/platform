# Onboarding a GitOps tenant

A **tenant** is an application that runs on the platform but lives in its **own
repository**. The tenant repo builds a container image and publishes its
Kubernetes manifests (`deploy/`) as a **signed OCI artifact** to GHCR; the
platform pulls that artifact with a Flux `OCIRepository` + `Kustomization` and
runs it in a dedicated, locked-down namespace.

There are two halves to onboarding, in two repos:

1. **The tenant repo** — created from the
   [`gitops-tenant-template`](https://github.com/devantler-tech/gitops-tenant-template),
   which ships the shared, framework-agnostic CI/CD plumbing and keeps it current
   via [template-sync](https://github.com/AndreasAugustin/actions-template-sync).
2. **The platform registration** — a small directory in *this* repo under
   `k8s/bases/apps/<tenant>/` that grants the tenant a namespace, identity, RBAC,
   network policy, and the Flux resources that pull its artifact.

Use an existing tenant —
[`k8s/bases/apps/ascoachingogvaner/`](../k8s/bases/apps/ascoachingogvaner) — as
the reference while following the steps below.

## 1. Create the tenant repository

Create the repo from the template with **"Use this template"** (GitHub UI), or:

```sh
gh repo create devantler-tech/<tenant> \
  --template devantler-tech/gitops-tenant-template --private
```

The template gives you the shared plumbing it keeps in sync (`cd.yaml`,
`release.yaml`, `template-sync.yaml`, `CLAUDE.md`, `zizmor.yml`) plus scaffolding
you then customise and own (`AGENTS.md`, the `maintain` skill, `ci.yaml`,
`Dockerfile`, `deploy/`, `.releaserc`, `.gitignore`, `.github/dependabot.yml`).
See the template's `README.md` for the exact owned-vs-synced split.

## 2. Fill in your stack

- **Application code + `Dockerfile`** — your app, building to a container that
  listens on the port your `deploy/` manifests expose.
- **`deploy/`** — your Kubernetes manifests (`kustomization.yaml`,
  `deployment.yaml`, `service.yaml`, `httproute.yaml`, an optional
  CloudNativePG `cluster.yaml`, and — when the app needs secrets — a namespaced
  `SecretStore` + `ExternalSecret` sourcing them from OpenBao; see §3).
  - The `HTTPRoute` attaches to the shared platform Gateway:
    `parentRefs: [{ name: platform, namespace: kube-system, sectionName: https }]`.
  - **The Deployment's container `name` MUST equal the repository name.** `cd.yaml`
    publishes via the platform's signed-publish path and pins the freshly-built
    image digest into the container named after the repo (see §4).
- **`ci.yaml`** — replace the example job with your stack's lint/test/build, kept
  behind the `aggregate-job-checks` required-checks gate.
- **`.templatesyncignore`** — list every file in the repo that *you* own so
  template-sync never overwrites it (your `AGENTS.md`,
  `.claude/skills/maintain/SKILL.md`, `ci.yaml`, `Dockerfile`, `deploy/`,
  `.releaserc`, `.gitignore`, `.github/dependabot.yml`, `README.md`, `LICENSE`,
  and the `.templatesyncignore` itself). Everything the template ships that is
  not ignored is kept in sync.

## 3. Secrets

Tenant app secrets come from **OpenBao** via **External Secrets** — not SOPS.

- **Store the values in OpenBao** (KV v2, mount `secret`) under your tenant
  prefix `apps/<tenant>/*`, out of band (OpenBao UI/CLI).
- **In `deploy/`**, add a **namespaced `SecretStore`** (`kind: SecretStore`) and
  an `ExternalSecret` that reads `apps/<tenant>/*` and materialises a native
  Secret. Tenants must use a *namespaced* store, never the shared
  `ClusterSecretStore` — the Kyverno policy `restrict-tenant-secret-stores`
  enforces this. The template ships ready-to-edit `secretstore.yaml` +
  `externalsecret.yaml`.
- **Platform-side (one-time per tenant)**, in
  [`vault-config/job.yaml`](../k8s/bases/infrastructure/vault-config/job.yaml):
  add an `app-<tenant>-readonly` policy scoped to `secret/data/apps/<tenant>/*`
  (mirror the existing `app-wedding-readonly`) and a Kubernetes auth role binding
  the tenant's ServiceAccount to it. The tenant's `edit` RoleBinding already
  aggregates the `external-secrets-tenant-edit` ClusterRole, so it may manage its
  own `SecretStore`/`ExternalSecret`.
- The release and template-sync workflows mint a **GitHub App token** from the
  org-level `APP_ID` variable and `APP_PRIVATE_KEY` secret — already available to
  every repo in the org, so no per-repo setup is needed.

## 4. How publishing & trust fit together

On every `v*` tag, the tenant's `cd.yaml` calls the
[`publish-app.yaml`](https://github.com/devantler-tech/reusable-workflows/blob/main/.github/workflows/publish-app.yaml)
reusable workflow, which builds and pushes the image, pins its digest into
`deploy/deployment.yaml`, pushes the manifests as an OCI artifact, and
**cosign-signs** both (keyless, via GitHub OIDC). The platform's `OCIRepository`
(§5) **verifies** that signature against the `publish-app.yaml` identity, so only
artifacts produced by that trusted workflow are ever reconciled onto the cluster.

> Tags come from `release.yaml` → semantic-release: merge Conventional-Commit
> PRs to `main` and a `vX.Y.Z` tag (and thus a publish) follows automatically.

## 5. Register the tenant on the platform

Add `k8s/bases/apps/<tenant>/` (copy `ascoachingogvaner/` and rename), with:

| File | Purpose |
|---|---|
| `namespace.yaml` | Namespace, `pod-security.kubernetes.io/enforce: restricted` |
| `serviceaccount.yaml` | SA with `automountServiceAccountToken: false` + `imagePullSecrets: [ghcr-auth]` |
| `rolebinding.yaml` | Binds the SA to the `edit` ClusterRole in the namespace |
| `networkpolicy.yaml` | Cilium policy: ingress from the Gateway on the app port; egress DNS (+ CNPG/metrics if needed) |
| `ghcr-auth-secret.enc.yaml` | SOPS-encrypted GHCR pull secret (`ghcr-auth`) |
| `sync.yaml` | `OCIRepository` (semver `>=1.0.0`, cosign `verify`) + `Kustomization` (prune, `serviceAccountName: <tenant>`) |

In `sync.yaml`, update the `name`/`namespace`/`url`
(`oci://ghcr.io/devantler-tech/<tenant>/manifests`) and keep the `verify` block
pointing at `publish-app.yaml`. No Flux `spec.decryption` is needed — tenant
secrets are delivered by External Secrets from OpenBao (§3), not SOPS-encrypted
inside the artifact.

Finally, add the directory to
[`k8s/bases/apps/kustomization.yaml`](../k8s/bases/apps/kustomization.yaml):

```yaml
resources:
  - <tenant>/
```

Open the change as a PR; once merged, Flux reconciles the new tenant.

## 6. Staying current

template-sync opens a PR in the tenant whenever the template's shared plumbing
changes (a bumped action pin, a workflow fix, an updated convention). Review and
merge it like any dependency update — your owned files are untouched.
