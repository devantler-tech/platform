# UniFi network management

The UniFi network is managed declaratively as a platform tenant. The desired
state lives in its own repository,
[`devantler-tech/unifi`](https://github.com/devantler-tech/unifi), as Crossplane
**Managed Resources** (`Client`, `TrafficRoute`, `Record`, … from the
[`provider-upjet-unifi`](https://github.com/devantler-tech/provider-upjet-unifi)
provider). On the cluster, a Flux `Kustomization` pulls the repo and applies the
resources into the `unifi` namespace, and the Crossplane provider continuously
reconciles each one against the controller API. The Managed Resource *is* the
state (observed status in `.status.atProvider`); there is no Terraform and no
separate state store.

```
devantler-tech/unifi (Crossplane Managed Resources)
   │  Flux GitRepository + Kustomization (namespace: unifi, runs as the unifi SA)
   ▼
provider-upjet-unifi (Crossplane, crossplane-system)
   │  reconciles Client / TrafficRoute / Record against the controller API
   ▼
UniFi Controller API   (reached from Hetzner)
```

This is the steady-state Crossplane model; it replaced an interim OpenTofu +
tofu-controller setup. Broadening coverage (VLANs/WLANs/firewall) and a
cross-resource reference for `TrafficRoute.networkId` are tracked in the
`provider-upjet-unifi` issues.

## Where it lives

| Piece | Path |
| --- | --- |
| Provider install (prod): `Provider` + `DeploymentRuntimeConfig` + `ManagedResourceActivationPolicy` | `k8s/providers/hetzner/infrastructure/crossplane/` |
| Tenant (ns, SA/RBAC, netpol, namespaced `SecretStore`, `ProviderConfig`, credential + WireGuard `ExternalSecret`s, `GitRepository`, Flux `Kustomization`) | `k8s/providers/hetzner/apps/unifi/` |
| OpenBao read policy (`infra-unifi-readonly`) + dedicated `unifi` auth role + placeholder seeding (Job) | `k8s/bases/infrastructure/vault-config/job.yaml` |
| Registered in the apps overlay | `k8s/providers/hetzner/apps/kustomization.yaml` |

It is a **regular tenant in the apps layer**, reconciled by the shared `apps`
Flux Kustomization like wedding-app/github-config; the provider itself installs in
the **infrastructure** layer (a `pkg.crossplane.io` `Provider` needs the Crossplane
CRDs first — the same controller-then-CR ordering as `provider-upjet-github`). It
lives in the **hetzner** overlay because it is prod-only: both the provider and the
controller API it reaches are Hetzner-only, so the UniFi CRDs are absent on
local/CI clusters. The provider declares **SafeStart**, so its
ManagedResourceDefinitions install inactive; the
`managed-resource-activation-policy-unifi.yaml` activates only the namespaced
(`*.unifi.m.crossplane.io`) `Client`/`TrafficRoute`/`Record` MRDs actually used.

The tenant's Flux `Kustomization` runs **as the namespace-scoped `unifi`
ServiceAccount** (a plain `Role` over the UniFi MR API groups — no cluster RBAC),
so the Managed Resources are applied with least privilege. The tenant repo is
**public** and holds **no secrets** — the `GitRepository` needs no auth.

## Credentials flow + the one gate

The controller API key and the gateway WireGuard keys live in **OpenBao** and are
pulled into the `unifi` namespace by `ExternalSecret`s through the tenant's own
**namespaced** `openbao` `SecretStore` (not the shared cluster store — the
`restrict-tenant-secret-stores` Kyverno policy blocks tenants from it), authorised
by the dedicated `unifi` OpenBao auth role (only `infra-unifi-readonly`).

```
vault-config Job  ── seeds PLACEHOLDERS (only if absent) ──▶
OpenBao  secret/infrastructure/unifi/controller   {api_url, api_key}
         secret/infrastructure/unifi/wireguard    {private_key, peer_public_key}
   │  ExternalSecrets via the namespaced `openbao` SecretStore (unifi role)
   ▼
unifi-controller-credentials Secret (a JSON blob)  ◄─ ProviderConfig.spec.credentials.secretRef
cluster-wireguard Secret {private-key, peer-public-key}  ◄─ VPN Client's *SecretRef
   │
   ▼
provider-upjet-unifi (crossplane-system)  ──▶  controller API
```

The `ProviderConfig` credentials Secret holds one JSON document the provider
forwards to the underlying SDK: `{"api_url":"…","api_key":"…","site":"default",
"allow_insecure":"false"}` (the `ExternalSecret` template builds it from the two
OpenBao keys). Seeding follows the same pattern as the `provider-upjet-github` App
credentials: the Job writes placeholders **only when the path is absent**, so a
later Job re-run never clobbers the real value. No SOPS, no PushSecret.

**Gate:** overwrite the placeholders in place via the **OpenBao UI** (or CLI):
- `secret/infrastructure/unifi/controller` — `api_url` (controller base URL,
  **without** the `/api` path) and `api_key` (Limited Admin, Local Access Only;
  UniFi OS ≥ 9.0.108).
- `secret/infrastructure/unifi/wireguard` — the gateway's `private_key` and the
  Talos server's `peer_public_key` (needed only once the WireGuard tunnel is set
  up — see [`wireguard-vpn-access.md`](wireguard-vpn-access.md)).

The `ExternalSecret`s re-sync automatically (1h refresh). OpenBao is backed up
(Raft snapshots → R2 via Velero), so the manually-set values are durable.

## Adopt-first adoption (do not skip)

A Managed Resource reconciles toward the controller continuously, so bring live
network config under management **adopt-first** — never let the first reconcile
create a duplicate:

1. Write the Managed Resource to match what already exists on the controller.
2. Annotate it `crossplane.io/external-name: <unifi-id>` (the controller's internal
   `_id`) so Crossplane binds the live object instead of creating a new one.
3. **(Safest) observe first:** set `spec.managementPolicies: ["Observe"]` and let it
   reconcile; confirm `kubectl -n unifi get <kind> <name> -o yaml` shows
   `Synced=True`, `Ready=True`, and a `.status.atProvider` matching the live object.
   **Only then** widen back to the default `["*"]` to manage it.
4. Only now edit fields to change the network, in a follow-up commit.

A genuinely **new** object (nothing to adopt — everything in the repo today) needs
no annotation; Crossplane creates it. See the tenant repo's
[`docs/runbook.md`](https://github.com/devantler-tech/unifi/blob/main/docs/runbook.md)
for the full procedure and how to find a live object's `_id`.

> **Deletion:** the Managed Resources use the default `deletionPolicy: Delete`, and
> the tenant's Flux `Kustomization` has `prune: true` — removing a resource from the
> repo deletes the live object. Set `deletionPolicy: Orphan` on a resource if it must
> survive removal from Git.

## Validation notes (confirm on the live cluster)

- **Provider health.** `kubectl get providers,providerconfigs.unifi.m.crossplane.io -A`
  — the `Provider` should be Installed+Healthy and the `ProviderConfig default` present
  in the `unifi` namespace. The provider needs its package image published
  (`ghcr.io/devantler-tech/provider-upjet-unifi`).
- **No tenant pods.** The provider runs in `crossplane-system`; the `unifi` namespace
  holds only Managed Resources, the `ProviderConfig`, and credential Secrets — hence
  the `default-deny` NetworkPolicy (no per-workload allow needed, unlike the former
  tofu-controller runner).
- **Egress to the controller.** The provider pod (crossplane-system) reaches the
  UniFi controller API over the Hetzner network; confirm reachability if a `Client`/
  `Record` stays `Synced=False` with a dial error.
