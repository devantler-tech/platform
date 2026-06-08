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

Tenant secrets come from **OpenBao** via **External Secrets** — never SOPS. No
tenant ships a SOPS-encrypted Secret.

### App secrets (DB creds, API keys, … — only for tenants that need them)

A tenant gets a store + Vault role **only if it needs app secrets** — a static
site gets none. Two halves — the tenant reads, the platform seeds. This is a
GitOps repo: a value is only ever introduced **SOPS-encrypted** and pushed to
OpenBao by a `PushSecret`, **never written out of band** (no OpenBao UI/CLI).

- **Tenant (`deploy/`)** — add only an `ExternalSecret` that references the
  platform-provided namespaced store (`secretStoreRef: { name: openbao, kind:
  SecretStore }`), reads `apps/<tenant>/*`, and materialises a native Secret.
  Never reference the shared `ClusterSecretStore` — the Kyverno policy
  `restrict-tenant-secret-stores` blocks tenant-applied resources from doing so.
  Your `edit` RoleBinding aggregates the `external-secrets-tenant-edit`
  ClusterRole, so you may manage your own `ExternalSecret`/`PushSecret`/`Password`
  generator resources.
- **Platform (one-time per such tenant)**:
  - **Provision the store + isolation** — in
    [`vault-config/job.yaml`](../k8s/bases/infrastructure/vault-config/job.yaml)
    add an `app-<tenant>` policy scoped to `secret/{data,metadata}/apps/<tenant>/*`
    and a dedicated `auth/kubernetes/role/<tenant>` bound to the tenant SA (mirror
    `app-wedding-app` + the `wedding-app` role); drop a `secretstore.yaml`
    (`kind: SecretStore`, named `openbao`) into the registration dir (mirror
    `wedding-app/`). The store can never reach infra or another tenant's path.
  - **Seed the value (GitOps)** — SOPS-encrypt it as a key in
    `variables-cluster-secret.enc.yaml` and add a `PushSecret` in
    [`vault-seed/push-secrets.yaml`](../k8s/bases/infrastructure/vault-seed/push-secrets.yaml)
    that pushes it to `apps/<tenant>/*` via the `openbao` ClusterSecretStore
    (mirror `seed-wedding-app-admin-code`). Randomly-generatable values use a
    `Password` generator + `push-generated-secrets.yaml` instead.

### The GHCR image-pull secret (`ghcr-auth`)

A **platform-managed** credential, not a tenant secret. Every registration dir
ships a `ghcr-auth-externalsecret.yaml` that sources the shared org pull
credential from OpenBao (`infrastructure/ghcr`) via the cluster-scoped `openbao`
**ClusterSecretStore** and materialises the `ghcr-auth` dockerconfigjson the
`OCIRepository` and ServiceAccount consume. It is reconciled by flux-system (not
your tenant SA) — which is why it may use the ClusterSecretStore where your own
resources may not (the Kyverno policy carves out flux-system-applied resources).
The value lives SOPS-encrypted as `ghcr_dockerconfigjson` in the shared
`variables-base-secret.enc.yaml` (the same org token both clusters use); the
`seed-ghcr` PushSecret pushes it to `infrastructure/ghcr` via the `openbao`
ClusterSecretStore.

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

Add `k8s/bases/apps/<tenant>/` — copy `ascoachingogvaner/` (a static tenant with
no app secrets) or `wedding-app/` (a tenant with app secrets + a namespaced
SecretStore) and rename — with:

| File | Purpose |
|---|---|
| `kustomization.yaml` | Kustomize entrypoint listing the resources in this directory |
| `namespace.yaml` | Namespace, `pod-security.kubernetes.io/enforce: restricted` |
| `serviceaccount.yaml` | SA with `automountServiceAccountToken: false` + `imagePullSecrets: [ghcr-auth]` |
| `rolebinding.yaml` | Binds the SA to the `edit` ClusterRole in the namespace |
| `networkpolicy.yaml` | Cilium policy: ingress from the Gateway on the app port; egress DNS (+ CNPG/metrics if needed) |
| `ghcr-auth-externalsecret.yaml` | OpenBao-backed `ExternalSecret` (shared `openbao` ClusterSecretStore, key `infrastructure/ghcr`) producing the `ghcr-auth` pull secret |
| `secretstore.yaml` | *Only if the tenant needs app secrets* — namespaced `SecretStore` (`kind: SecretStore`, name `openbao`) authenticating via the tenant's Vault role (mirror `wedding-app/`) |
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
