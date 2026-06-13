# UniFi network management

The UniFi network is managed declaratively as a platform tenant. The desired
state lives in its own repository,
[`devantler-tech/unifi`](https://github.com/devantler-tech/unifi) (plain
OpenTofu/Terraform using the
[`filipowm/unifi`](https://github.com/filipowm/terraform-provider-unifi)
provider). On the cluster, **tofu-controller** continuously reconciles it: a
`Terraform` custom resource pulls the repo as a Flux source and runs
`tofu plan`/`apply` against the controller API.

```
devantler-tech/unifi (OpenTofu, filipowm/unifi)
   │  Flux GitRepository (namespace: unifi)
   ▼
Terraform CR (tofu-controller, infra.contrib.fluxcd.io/v1alpha2)
   │  tf-runner pod runs OpenTofu, state in a Secret in the unifi namespace
   ▼
UniFi Controller API   (reached from Hetzner)
```

This is the pragmatic interim. The steady-state goal is a real-CRD Crossplane
provider (`provider-upjet-unifi`) — tracked in the monorepo issues.

## Where it lives

| Piece | Path |
| --- | --- |
| tofu-controller install (prod) | `k8s/providers/hetzner/infrastructure/controllers/tofu-controller/` |
| Tenant (ns, SA/RBAC, netpol, GitRepository, Terraform CR, secrets) | `k8s/providers/hetzner/unifi/` |
| Dedicated, isolated Flux Kustomization (prod) | `k8s/clusters/prod/unifi-flux-kustomization.yaml` |

Prod-only: the controller API is only reachable from Hetzner. The reconcile runs
in its own Flux Kustomization so a UniFi stall never blocks app deploys.

## Gates (maintainer)

Until these are in place the `unifi` Flux Kustomization is **red by design**.

1. **UniFi API key** — generate a Limited Admin, Local Access Only API key on the
   controller (UniFi OS ≥ 9.0.108). Add to
   `k8s/clusters/prod/bootstrap/variables-cluster-secret.enc.yaml` (SOPS):
   - `unifi_api_url` — controller base URL, **without** the `/api` path
   - `unifi_api_key` — the API key
   - (`unifi_site` — optional, only for a non-default site)
2. **Git read token** — the tenant repo is private. Add `unifi_git_token` (a
   fine-grained PAT with **Contents: read** on `devantler-tech/unifi`) to the same
   SOPS file. (A repo-scoped deploy key is the tighter alternative — switch the
   `GitRepository` to `ssh://` + an identity/known_hosts secret.)

These are substituted into the tenant Secrets by the Kustomization's
`postBuild.substituteFrom` — values are never committed in plaintext.

## Observe-first adoption (do not skip)

The `Terraform` CR ships with `approvePlan: ""` — **plan only, it never applies**.
Bring the live network under management without risk:

1. Let it reconcile and read the plan: `kubectl -n unifi describe terraform unifi`.
2. In the repo, write resources to match what already exists and `import` them so
   the plan becomes a **no-op** (see the repo README).
3. Approve a reviewed plan by setting `approvePlan` to its id (e.g.
   `plan-main-<sha>`), or flip to `auto` only once steady-state drift-reconcile is
   trusted.

`destroyResourcesOnDeletion: false` — deleting the CR never tears down the network.

## Validation notes (confirm on the live cluster)

- **Runner ↔ controller mTLS.** Cluster-wide `require-mutual-auth` (SPIRE) applies
  to the tofu-controller ↔ tf-runner gRPC (:30000). Cilium auto-issues SPIFFE
  identities to pods, so the L3/L4 allow in `unifi/networkpolicy.yaml` should
  suffice — verify the runner connects.
- **Runner PSS.** The namespace enforces `restricted`. If the upstream tf-runner
  image cannot run non-root, either rely on the platform's Kyverno securityContext
  mutation or relax the namespace to `baseline`.
- **Egress.** `networkpolicy.yaml` allows `world:443,8443`; tighten to the
  controller address + the OpenTofu registry FQDNs once known.
