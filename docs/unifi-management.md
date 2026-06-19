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
| OpenBao read policy (`infra-unifi-readonly`) + placeholder seeding (Job) | `k8s/bases/infrastructure/vault-config/job.yaml` |
| Registered in the apps overlay | `k8s/providers/hetzner/apps/kustomization.yaml` |

It is a **regular tenant in the apps layer**, reconciled by the shared `apps`
Flux Kustomization like wedding-app/ascoachingogvaner. It lives in the **hetzner
apps overlay** (not `bases/apps/`) because it is prod-only: both tofu-controller
and the controller API it reaches are Hetzner-only, so the Terraform CRD is absent
on local/CI clusters. The apps layer is `wait: true`, but the credential chain is
healthy on first apply without any manual step: the vault-config Job (infrastructure
layer, reconciles before apps) seeds a **placeholder** at
`secret/infrastructure/unifi/controller`, so the `ExternalSecret` syncs and — with
the observe-first empty config (a no-op plan even on the placeholder) — the
Terraform CR goes Ready. The apps layer stays green throughout.

The tenant repo is **public** and holds **no secrets** — the `GitRepository` needs
no auth. The only sensitive value (the UniFi API key) lives in **OpenBao** and is
pulled into the cluster by an `ExternalSecret` (the `openbao` ClusterSecretStore).

## Credentials flow + the one gate

```
vault-config Job  ── seeds PLACEHOLDER (only if absent) ──▶
OpenBao  secret/infrastructure/unifi/controller  {api_url, api_key}
   │  ExternalSecret (infra-unifi-readonly policy)
   ▼
unifi-credentials Secret → Terraform CR (varsFrom)
```

Seeding follows the same pattern as the provider-upjet-github App credentials: the
Job writes `api_url=https://REPLACE_WITH_UNIFI_CONTROLLER_URL`,
`api_key=REPLACE_WITH_UNIFI_API_KEY` **only when the path is absent**, so a later
Job re-run never clobbers the real value. No SOPS, no PushSecret.

**Gate:** overwrite the placeholder in place via the **OpenBao UI** (or CLI) —
`secret/infrastructure/unifi/controller`, set `api_url` (controller base URL,
**without** the `/api` path) and `api_key` (Limited Admin, Local Access Only; UniFi
OS ≥ 9.0.108). The `ExternalSecret` re-syncs automatically (1h refresh). OpenBao is
backed up (Raft snapshots → R2 via Velero), so the manually-set value is durable.

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
