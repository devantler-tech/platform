# Backstage (internal developer portal)

The platform runs **Backstage** as its internal developer portal — a software
catalog and the seed of golden-path scaffolding/docs. It is deployed via the
official `backstage` Helm chart in `k8s/bases/apps/backstage/`, backed by a
CloudNativePG database and fronted by the platform's SSO.

> **Status: scaffold.** What ships today is the full *platform integration*
> (database, SSO, ingress, network policy, registration) running the upstream
> **demo image**. Productionising it requires a **custom-built image** — see
> [Productionisation](#productionisation) below. This page is the contract for
> that next step.

## What's deployed

| Piece | How |
|---|---|
| App | `backstage` Helm chart (`backstage.github.io/charts`), image `ghcr.io/backstage/backstage:1.52.0` (demo), 1 replica (single-pod — Backstage runs DB migrations on startup) |
| Database | CloudNativePG `backstage-db` (2 instances, longhorn, synchronous). Backstage uses the generated `backstage-db-app` Secret and one DB with a schema per plugin (`pluginDivisionMode: schema`) |
| Backend secret | Generated once by an External Secrets `Password` generator (`backend-secret.yaml`), never rotated |
| SSO | **oauth2-proxy forward-auth** (Dex) — the route backends to oauth2-proxy, which hands authenticated requests to the auth-proxy (Traefik) → the `backstage` Service. The demo image has no native OIDC |
| Ingress | `HTTPRoute` for `backstage.${domain}` on the shared Gateway |
| Isolation | default-deny + `allow-backstage` CiliumNetworkPolicy; restricted PSS |

Reachable at `https://backstage.${domain}` once SSO is cleared (prod only — like
umami and the tenants, it needs CNPG + longhorn, so it is excluded from the
local/docker overlay).

## Why the demo image is a placeholder

Backstage's frontend, backend and **installed plugins are compiled into the image
at build time**. App-config (and therefore Helm `values`) can only change
*runtime* settings — URLs, tokens, the DB connection. It **cannot add a plugin
that isn't already in the image**. The upstream demo image ships only the example
plugin set and an example catalog, so the genuinely useful capabilities below all
require an own-built image:

- **Native OIDC sign-in** (`@backstage/plugin-auth-backend-module-oidc-provider`)
  — so identity flows *into* Backstage (catalog ownership, `whoami`), not just a
  proxy gate in front of it.
- **Kubernetes plugin** (`@backstage/plugin-kubernetes`) — show this platform's
  workloads/health in the portal (read-only ServiceAccount).
- **GitHub catalog discovery** (`@backstage/plugin-catalog-backend-module-github`)
  — auto-ingest the real devantler-tech components from `catalog-info.yaml` files
  instead of the example catalog.

## Productionisation

The recommended path mirrors how every other first-party app on this platform is
built — as a **tenant repo** (see [TENANTS.md](TENANTS.md)):

1. **Scaffold an own Backstage app** (`npx @backstage/create-app`) in a new
   `devantler-tech/backstage` repo created from `gitops-tenant-template`; add the
   OIDC, Kubernetes and GitHub-discovery plugins to `packages/backend`.
2. **Build + publish** the image (and signed manifests) via the standard
   `publish-app.yaml` pipeline — pin the digest, never `latest`.
3. **Point this HelmRelease** at the own-built image (swap `image.repository` /
   `image.tag`), and replace the forward-auth route with **native OIDC**:
   - add a Dex static client + redirect URI
     `https://backstage.${domain}/api/auth/oidc/handler/frame` (the dex
     HelmRelease's `staticClients`), secret via an OpenBao `ExternalSecret`;
   - set `auth.providers.oidc` + a sign-in resolver in app-config.
4. **Kubernetes plugin**: add a read-only ServiceAccount + ClusterRole and feed
   its token; **GitHub discovery**: add an `api.github.com` egress to
   `networkpolicy.yaml` and a GitHub App token via OpenBao.

Each step is independent and incremental — the scaffold here is designed so none
of it requires re-plumbing the database, ingress or SSO transport.

## Operations

- **Validate**: `ksail --config ksail.prod.yaml workload validate` renders the
  chart with these values (Backstage is in the prod tree).
- **DB**: CNPG owns failover/replication; Velero backs the `backstage` namespace
  up daily. The catalog is re-derivable from Git, so no Barman/PITR is wired
  (add it like `umami-db` if the portal accumulates irreplaceable state).
- **Logs/health**: Coroot (`observability.${domain}`) sees the pod like any other
  workload via the eBPF node-agent.
