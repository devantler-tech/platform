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
| Tenant (ns, SA/RBAC, netpol, GitRepository, Terraform CR, ExternalSecret) | `k8s/providers/hetzner/apps/unifi/` |
| OpenBao read + seed-write policies (`infra-unifi-readonly`, `vault-seed-write`) | `k8s/bases/infrastructure/vault-config/job.yaml` |
| Declarative seed (PushSecret + SOPS placeholder) | `k8s/providers/hetzner/infrastructure/vault-seed/seed-unifi.yaml` + `clusters/prod/bootstrap/variables-cluster-secret.enc.yaml` |
| Registered in the apps overlay | `k8s/providers/hetzner/apps/kustomization.yaml` |

It is a **regular tenant in the apps layer**, reconciled by the shared `apps`
Flux Kustomization like wedding-app/ascoachingogvaner. It lives in the **hetzner
apps overlay** (not `bases/apps/`) because it is prod-only: both tofu-controller
and the controller API it reaches are Hetzner-only, so the Terraform CRD is absent
on local/CI clusters. Because the apps layer is `wait: true`, **seed the OpenBao
secret before merging** so the tenant is healthy on first apply (an unseeded
ExternalSecret would hold the apps layer NotReady). With the observe-first empty
config, the plan is a no-op, so once the secret exists the Terraform CR is Ready.

The tenant repo is **public** and holds **no secrets** — the `GitRepository`
needs no auth. The only sensitive value (the UniFi API key) is seeded into
**OpenBao** declaratively (GitOps) and pulled into the cluster by an
`ExternalSecret` (the `openbao` ClusterSecretStore).

## Credentials flow (GitOps) + the one gate

```
variables-cluster-secret.enc.yaml (SOPS)   ← maintainer sets the value here
   │  seed-unifi PushSecret (vault-seed/)
   ▼
OpenBao  secret/infrastructure/unifi/controller  {api_url, api_key}
   │  ExternalSecret (infra-unifi-readonly policy)
   ▼
unifi-credentials Secret → Terraform CR (varsFrom)
```

The repo ships **placeholders** (`unifi_api_url=https://unifi.example.invalid`,
`unifi_api_key=PLACEHOLDER…`) so the chain is healthy on merge. **Gate:** set the
real key by editing the SOPS file and committing:

```sh
sops k8s/clusters/prod/bootstrap/variables-cluster-secret.enc.yaml
# set unifi_api_url (controller base URL, WITHOUT the /api path) and
# unifi_api_key (Limited Admin, Local Access Only; UniFi OS >= 9.0.108)
```

The `seed-unifi` PushSecret re-pushes hourly, so the new value reaches OpenBao
and the `ExternalSecret` re-syncs automatically — no `bao kv put`, no out-of-band
writes. With the observe-first empty config the plan is a no-op even on the
placeholder, so the apps layer stays green throughout.

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
