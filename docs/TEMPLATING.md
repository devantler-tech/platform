# Templating guide

For a new platform, start with
[platform-template](https://github.com/devantler-tech/platform-template) and its
[bootstrap guide](https://github.com/devantler-tech/platform-template/blob/main/docs/BOOTSTRAP.md).
This repository is Devantler Tech's live reference instance for business
services and personal applications.

The sections below describe this instance's customization points if you choose
to adapt it directly. Supply your own settings and credentials; its committed
encrypted secrets cannot be decrypted with your keys.

## Template inputs (edit these)

### 1. ksail configs — one per environment

Files: `ksail.yaml` (local), `ksail.prod.yaml`.

Only these fields genuinely vary per instance:

| Field | local | prod |
| --- | --- | --- |
| `metadata.name` | cluster short name (e.g. `local`) | `prod` |
| `spec.cluster.connection.context` | kubeconfig context | kubeconfig context |
| `spec.cluster.localRegistry.registry` | n/a | OCI registry URL for the manifest artefact |
| `spec.provider.hetzner.location` | n/a | primary Hetzner location (`fsn1`, `nbg1`, `hel1`, …) |
| `spec.provider.hetzner.{controlPlane,worker}ServerType` | n/a | Hetzner server types (default `cx33`) |
| `spec.provider.hetzner.networkCidr` | n/a | private network CIDR for the cluster |
| `spec.cluster.autoscaler.node.pools` | n/a | node pool definitions (name, serverType, location, min, max) |
| `spec.cluster.autoscaler.node.maxNodesTotal` | n/a | hard ceiling on total cluster nodes |
| `spec.workload.kustomizationFile` | `clusters/local` | `clusters/prod` |

Everything else (distribution, provider, CNI, GitOps engine, timeouts,
`certManager`/`metricsServer`/`policyEngine`, Talos control-plane count,
`sourceDirectory`, tag) should match across all Hetzner-backed instances.

### 2. Talos machine-config directories

- `talos-local/` — Docker-provider patches.
- `talos/` — Hetzner-provider patches. Used by prod.
  Split into `cluster/`, `control-planes/`, and `workers/` as ksail expects.

Edit the YAML patches inside if your DNS, OIDC issuer, or networking differs.

### 3. Per-cluster overlay

Each `k8s/clusters/<env>/kustomization.yaml` carries two template inputs in a
local-config `cluster-meta` ConfigMap:

```yaml
data:
  cluster_name: <env>          # drives spec.path: clusters/<env>/bootstrap
  provider: <docker|hetzner>   # drives spec.path: providers/<provider>/...
```

Replacements in the same file rewrite the sentinel placeholders
(`__CLUSTER__`, `__PROVIDER__`) that come from `k8s/clusters/base/`. Adding a
new environment is "copy an existing overlay directory, change these two
values, point ksail at it".

### 4. Per-cluster bootstrap

Each `k8s/clusters/<env>/bootstrap/` directory contains the only resources
Flux reads that are genuinely per-cluster:

- `config-map.yaml` — non-secret values (hostnames, URLs,
  feature flags, Hetzner LB location and type, etc).
- `secret.enc.yaml` — SOPS-encrypted secrets. Re-encrypt
  these with your own Age key (update `.sops.yaml`, then `sops -e` each file).

### 5. SOPS configuration

`.sops.yaml` lists the Age public keys authorised to decrypt secrets. Replace
with your own public key and re-encrypt every `*.enc.yaml` file in the repo.

### 6. CI/CD secrets and variables

GitHub Actions expect:

- Secrets: `GHCR_TOKEN`, `SOPS_AGE_KEY`, `HCLOUD_TOKEN`, `KUBE_CONFIG`,
  `TALOS_CONFIG`, `WG_SERVER_PRIVATE_KEY` (plus the optional, DR-only
  `DR_GH_ADMIN_TOKEN`), and `APP_PRIVATE_KEY` for the organization's GitHub App
- Variables: `APP_CLIENT_ID`, the same GitHub App's client ID

In devantler-tech the GitHub App pair comes from the organization. A copy
outside it has to create both.

See `.github/workflows/` for the exact names.

## Template body (do not edit when instantiating)

- `k8s/clusters/base/` — shared Flux Kustomizations with sentinel paths.
- `k8s/bases/infrastructure/` — Cilium, cert-manager, Kyverno, alerting configs,
  OpenBao vault, External Secrets Operator, ClusterSecretStore, vault-config Job,
  vault-seed PushSecrets, vault-backup CronJob.
- `k8s/bases/apps/` — reference applications (homepage, whoami).
- `k8s/providers/{docker,hetzner}/` — provider-specific assembly of bases.

Changes here are "platform changes" — upstream them instead of forking them.

## Secrets architecture

The platform uses a **hybrid SOPS + OpenBao** model:

- **SOPS + Age** encrypts externally-sourced secrets in Git (API tokens,
  service credentials). These are consumed by `infrastructure-controllers`
  via Flux `postBuild` substitution for bootstrap-critical controllers
  (e.g., hcloud-csi), and seeded into OpenBao via PushSecrets for all
  other consumers.
- **ESO Password generators** create randomly-generatable secrets
  (database passwords, OIDC client secrets). PushSecrets seed these into
  OpenBao at first reconciliation.
- **OpenBao** (self-hosted Vault fork) is the single source of truth for
  all non-bootstrap secrets. Runs in the `openbao` namespace in HA mode
  with integrated Raft storage (`openbao_replicas`: 1 by default, 3 in prod).
- **External Secrets Operator** syncs secrets from OpenBao into native K8s
  `Secret` objects via `ExternalSecret` and `ClusterSecretStore` CRs.
- **PushSecret** CRs in `k8s/bases/infrastructure/vault-seed/` seed
  OpenBao from both generators and SOPS-decrypted Flux variable Secrets.

### Secret categories

| Category | Source | Example | Mechanism |
| ---------- | -------- | --------- | ----------- |
| Randomly-generatable | ESO Password generator | DB passwords, OIDC client secrets | Generator -> PushSecret -> OpenBao -> ExternalSecret |
| Externally-sourced | SOPS-encrypted Git | API tokens, service credentials | SOPS -> PushSecret -> OpenBao -> ExternalSecret |
| Bootstrap-critical | SOPS + Flux substitution | hcloud token | SOPS -> Flux `postBuild` -> inline K8s Secret |

### First-time vault setup (after cluster creation)

1. Deploy the cluster: `ksail cluster create && ksail workload push && ksail workload reconcile`
2. Flux deploys `infrastructure-controllers` -> OpenBao starts (sealed, uninitialized).
   Controllers with placeholder Secrets (Dex, OAuth2-proxy) start with dummy values.
3. Flux deploys `infrastructure` -> the `vault-config` Job auto-initializes:
   - `vault-init` container runs `bao operator init`, captures unseal key + root token
   - `store-keys` container persists credentials in the `openbao-unseal` K8s Secret
   - `vault-config` container configures policies, auth roles, and KV engine
4. The OpenBao `postStart` hook auto-unseals on subsequent pod restarts using the
   `openbao-unseal` Secret (volume mount with `optional: true`).
5. ESO Password generators create random secrets; PushSecrets seed all values
   (both generated and SOPS-sourced) into OpenBao.
6. ExternalSecrets sync secrets to consumer namespaces, overwriting placeholders.
7. Reloader restarts affected controllers (Dex, OAuth2-proxy) with real secrets.

Note: the Docker provider's platform CA key pair is **not** stored in OpenBao.
cert-manager auto-generates it via a self-signed CA Certificate resource
(see `k8s/providers/docker/infrastructure/cluster-issuers/`). The Hetzner
provider uses Let's Encrypt and does not need a local CA.

No manual steps are required -- cluster creation is fully automated.

## Adding a new environment

1. `cp -R talos talos-<env>` (or reuse `talos`).
2. `cp -R k8s/clusters/prod k8s/clusters/<env>` and update `cluster_name` +
   `provider` in the new overlay's `cluster-meta` patch.
3. Edit `k8s/clusters/<env>/bootstrap/config-map.yaml` and `k8s/clusters/<env>/bootstrap/secret.enc.yaml`.
4. `cp ksail.prod.yaml ksail.<env>.yaml`; update the per-cluster fields.
5. Add the new environment to `.github/workflows/` pipelines as needed.

That's the complete set of edits. Everything else is inherited from the
shared scaffold.
