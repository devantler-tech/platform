# Templating guide

This repository is structured so the shared platform scaffolding (bases,
providers, cluster Flux Kustomizations) can stay untouched when you fork it
for your own homelab. Everything a new instance needs to customise is listed
below; anything not listed is template *body* and should be left alone unless
you're upstreaming a change.

The long-term goal is to extract the template body into a standalone
cookiecutter/copier template, with this repository remaining as a reference
instance. Until that happens, "forking and editing the inputs" is the
supported path.

## Template inputs (edit these)

### 1. ksail configs — one per environment

Files: `ksail.yaml` (local), `ksail.dev.yaml`, `ksail.prod.yaml`.

Only these fields genuinely vary per instance:

| Field | local | dev / prod |
|---|---|---|
| `metadata.name` | cluster short name (e.g. `local`) | `dev` / `prod` |
| `spec.cluster.connection.context` | kubeconfig context | kubeconfig context |
| `spec.cluster.localRegistry.registry` | n/a | OCI registry URL for the manifest artefact |
| `spec.provider.omni.endpoint` | n/a | your Omni instance URL |
| `spec.provider.omni.machineClass` | n/a | Omni machine class name |
| `spec.workload.kustomizationFile` | `clusters/local` | `clusters/<env>` |

Everything else (distribution, provider, CNI, GitOps engine, timeouts,
`certManager`/`metricsServer`/`policyEngine`, Talos control-plane count,
`sourceDirectory`, tag) should match across all Omni-backed instances.

### 2. Talos machine-config directories

- `talos-local/` — Docker-provider patches.
- `talos-omni/` — Omni-provider patches. Shared between dev and prod.

Edit the YAML patches inside if your DNS, OIDC issuer, or networking differs.

### 3. Per-cluster overlay

Each `k8s/clusters/<env>/kustomization.yaml` carries two template inputs in a
local-config `cluster-meta` ConfigMap:

```yaml
data:
  cluster_name: <env>          # drives spec.path: clusters/<env>/variables
  provider: <docker|omni>      # drives spec.path: providers/<provider>/...
```

Replacements in the same file rewrite the sentinel placeholders
(`__CLUSTER__`, `__PROVIDER__`) that come from `k8s/bases/cluster/`. Adding a
new environment is "copy an existing overlay directory, change these two
values, point ksail at it".

### 4. Per-cluster variables

Each `k8s/clusters/<env>/variables/` directory contains the only resources
Flux reads that are genuinely per-cluster:

- `variables-cluster-config-map.yaml` — non-secret values (hostnames, URLs,
  feature flags, etc).
- `variables-cluster-secret.enc.yaml` — SOPS-encrypted secrets. Re-encrypt
  these with your own Age key (update `.sops.yaml`, then `sops -e` each file).

### 5. SOPS configuration

`.sops.yaml` lists the Age public keys authorised to decrypt secrets. Replace
with your own public key and re-encrypt every `*.enc.yaml` file in the repo.

### 6. CI/CD secrets and variables

GitHub Actions expect:

- Secrets: `GHCR_TOKEN`, `SOPS_AGE_KEY`, `OMNI_SERVICE_ACCOUNT_KEY`
- Variables: `OMNI_ENDPOINT` (per environment)

See `.github/workflows/` for the exact names.

## Template body (do not edit when instantiating)

- `k8s/bases/cluster/` — shared Flux Kustomizations with sentinel paths.
- `k8s/bases/infrastructure/` — Cilium, cert-manager, Kyverno, alerting configs, etc.
- `k8s/bases/apps/` — reference applications (homepage, whoami, headlamp).
- `k8s/providers/{docker,omni}/` — provider-specific assembly of bases.

Changes here are "platform changes" — upstream them instead of forking them.

## Adding a new environment

1. `cp -R talos-omni talos-<env>` (or reuse `talos-omni`).
2. `cp -R k8s/clusters/prod k8s/clusters/<env>` and update `cluster_name` +
   `provider` in the new overlay's `cluster-meta` patch.
3. Edit `k8s/clusters/<env>/variables/variables-cluster-{config-map,secret.enc}.yaml`.
4. `cp ksail.prod.yaml ksail.<env>.yaml`; update the five per-cluster fields.
5. Add the new environment to `.github/workflows/` pipelines as needed.

That's the complete set of edits. Everything else is inherited from the
shared scaffold.
