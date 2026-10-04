# AGENTS.md

This file is the **single canonical instructions file** for AI agents and assistants working on this repository (read natively by GitHub Copilot, Cursor, Codex, and — via `CLAUDE.md` (`@AGENTS.md`) — Claude). It provides project-specific conventions, operational workflows, and maintenance guidance.

Always reference these instructions first; fall back to search or ad-hoc commands only when you hit something that does not match what is written here.

## Project Overview

This is a **GitOps-based Kubernetes platform**. Kubernetes YAML manifests are managed with Kustomize overlays and deployed via Flux CD; operational Go commands and Bash scripts live under `scripts/`.

### Technology Stack

- **Flux CD** — GitOps engine reconciling from OCI artifacts
- **Kustomize** — manifest templating and overlays
- **Cilium** — CNI and Gateway API. WireGuard wire encryption is enabled for pod-to-pod traffic in prod. Cilium authentication and its SPIRE integration are disabled while no narrowly scoped authentication policy consumes them; enabling SPIRE without a consumer causes delegated-identity subscriptions to emit continuous `no identity issued` errors. Re-enable both only alongside a narrow consumer. The platform does **not** install a blanket cluster-wide mTLS policy: Cilium ingress rules are allow rules, so an authentication rule with a semantically empty source selector would impose no allow-list and weaken namespace/application isolation. Empty forms include `fromEndpoints: [{}]` and selectors with `matchLabels: {}`, `matchExpressions: []`, or both fields empty. The Docker provider overlay also disables encryption for local/CI.
- **Talos Linux** — immutable Kubernetes OS
- **KSail** — unified cluster and workload lifecycle management (Talos + Docker for local, Talos + Hetzner for prod)
- **SOPS + Age** — secret encryption at rest (per-environment Age keys)
- **GHCR** — OCI artifact storage (production)
- **Kyverno** — policy engine

## Repository Structure

```
k8s/                  # All Kubernetes manifests
  bases/              # Shared base resources (never modify directly from overlays)
    bootstrap/        # Flux post-build substitution variables (ConfigMap + SOPS secret)
    infrastructure/   # Component-folder-first: controllers/, gateway/, vault-*/, plus
                      #   plural-Kind CR folders (cluster-policies/, external-secrets/, …)
    apps/             # Application deployments
  providers/          # Provider-specific overlays (docker, hetzner)
  clusters/           # Per-environment overlays (local, prod)
    base/             # Cluster-level Flux Kustomization wiring (bootstrap, infra, apps ordering)
talos-local/          # Talos machine config patches for Docker (local)
talos/                # Talos machine config patches for Hetzner (prod): cluster/, control-planes/, workers/
docs/                 # Additional documentation (incl. docs/dr/ disaster-recovery runbooks)
hosts                 # Host entries mapping *.platform.lan names to 127.0.0.1 for local access
ksail.yaml            # KSail local cluster config (Talos + Docker, kustomizationFile: clusters/local)
ksail.prod.yaml       # KSail production cluster config (Talos + Hetzner, kustomizationFile: clusters/prod)
.sops.yaml            # SOPS encryption rules and Age public keys
.releaserc            # semantic-release configuration
```

Detailed, topic-scoped conventions live in this file's sections below — Kustomize overlays,
Flux dependency ordering and HelmRelease conventions (manifest structure), Talos machine-config
patch structure, and the SOPS encryption workflow and key rules. This file (`AGENTS.md`) is what
GitHub Copilot code review reads.

## Prerequisites and Tool Installation

The tooling below is needed to run a cluster locally. **Maintenance work does not require a cluster** (see [Validation](#validation)); these are only for full local development.

```bash
# Docker (if not already installed)
curl -fsSL https://get.docker.com -o get-docker.sh && sudo sh get-docker.sh

# Age — secret encryption
sudo apt-get update && sudo apt-get install -y age

# SOPS — secret management
wget -O /tmp/sops_amd64.deb https://github.com/getsops/sops/releases/download/v3.8.1/sops_3.8.1_amd64.deb
sudo dpkg -i /tmp/sops_amd64.deb

# yq v4 — exact YAML field queries in production lifecycle/recovery scripts
brew install yq

# KSail — cluster + workload lifecycle (Homebrew)
brew tap devantler-tech/formulas && brew install ksail
```

Verify the toolchain:

```bash
docker --version && ksail --version && kubectl version --client
sops --version && age --version && yq --version
docker ps              # Docker daemon is running
ksail cluster list     # existing Talos clusters
```

## Validation

The repository has static manifest checks, Go tests and linters, and MegaLinter. **Never run a cluster for maintenance.** For Go changes, run the affected packages' tests from the repository root (for example, `go test ./scripts/analyze-cri-logs`) and the relevant offline script regressions. CI's jobs and path filters in [`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) identify those checks; the organization-provided Go workflow supplies the Go build, test and golangci-lint checks. MegaLinter's scope and dispositions live in [`.mega-linter.yml`](.mega-linter.yml). For manifest changes, validate with:

```bash
# Preferred when KSail is installed: schema-aware validation with Flux variable
# substitution (per the repo README). `validate` does not start a cluster.
ksail workload validate
ksail --config ksail.prod.yaml workload validate

# Fallback (no KSail): build the cluster overlays and every layer Flux reconciles —
# kubectl has Kustomize built in; standalone `kustomize` may not be installed. These
# catch YAML and Kustomize errors, but substitute no Flux variables and check no schemas.
kubectl kustomize k8s/clusters/local/
kubectl kustomize k8s/clusters/prod/
kubectl kustomize k8s/providers/docker/infrastructure/controllers/
kubectl kustomize k8s/providers/docker/infrastructure/
kubectl kustomize k8s/providers/docker/apps/
kubectl kustomize k8s/providers/hetzner/infrastructure/controllers/
kubectl kustomize k8s/providers/hetzner/infrastructure/
kubectl kustomize k8s/providers/hetzner/apps/
kubectl kustomize k8s/clusters/local/bootstrap/
kubectl kustomize k8s/clusters/prod/bootstrap/

# Validate a single manifest's YAML/schema. Like the builds above, this applies no
# Flux postBuild substitution, so `${VAR}` placeholders stay literal.
kubectl apply --dry-run=client -f <file>
```

The cluster overlays (`k8s/clusters/local/`, `k8s/clusters/prod/`) render only the four Flux
`Kustomization`s that point at those layers. Building them checks the overlays' own patches and
replacements, which select each cluster's provider paths, but parses none of the manifests under
`k8s/bases/` or `k8s/providers/`, so a manifest change needs the layer builds as well.

A zero exit does not show that a changed file was checked. Confirm coverage by finding each changed
file in the output: its path in `ksail workload validate`'s output, or its resource (`metadata.name`)
in the build of the layer that includes it.

`flux check` and other cluster-dependent checks require a running cluster — they are **not** part of static validation and should not be run during maintenance.

For finite read-only runtime investigations, use the opt-in offline diagnostic in
[`docs/runtime-log-diagnosis.md`](docs/runtime-log-diagnosis.md). Its regressions
run with `go test ./scripts/analyze-cri-logs`; no cluster or credential is needed.
Timing evidence alone does not establish Coroot request attribution or recovery.

CI runs **static manifest validation** on PRs that touch k8s-related paths (`k8s/**`, `ksail*.yaml`, `.sops.yaml`, `talos*/**`, the naming configuration `.github/manifest-naming.yaml`, the validation scripts `scripts/validate-embedded-json/` / `scripts/validate-embedded-json.sh` / `scripts/generate-kubescape-exceptions/`, or `ci.yaml` — the authoritative list is the `k8s` filter in `.github/workflows/ci.yaml`) — the `validate` job in `.github/workflows/ci.yaml` first json-parses every registered embedded-JSON ConfigMap key via [`scripts/validate-embedded-json.sh`](scripts/validate-embedded-json.sh) (keys listed in the Go command's `registeredKeys` or ending in `.json` — schema validation treats such blobs as opaque strings, so a stray comma would otherwise ship silently; run `bash scripts/validate-embedded-json.sh` locally when touching one, from any directory via the script's absolute path), then runs `ksail workload validate` for both the local and prod overlays plus a Kubescape scan (`scripts/generate-kubescape-exceptions` converts the `ClusterSecurityException` CRs into Kubescape's exceptions format, then `ksail workload scan --framework nsa,mitre --exceptions <generated> --compliance-threshold <floor>` gates on the combined score — the exact floor lives in `ci.yaml`). It is fast, needs no secrets (so it runs on fork PRs too), and starts no cluster. PRs touching `talos/**` or `talos-local/**` additionally run the `validate-talos` job: it renders the machine config with every patch applied (placeholder values stand in for env-expanded secrets like `${WG_SERVER_PRIVATE_KEY}`) and `talosctl validate`s the result, so a broken patch or an empty env expansion fails the PR event instead of the merge group's deploy (#2477). There is **no longer a full-cluster system test**: the local Docker cluster is a thin manual test-bed (see [Local Development Cluster](#local-development-cluster)), not a CI prod stand-in.

The Talos gate renders both roles, applies the reviewed KSail post-installer kernel-argument fold, then validates both. Production's extension path clears `extraKernelArgs` and pins the UKI command line; the local path without extensions is unchanged. The unconditional version contract and real offline positive/invalid-patch controls are documented in [`scripts/reconcile-talos-kernel-args/README.md`](scripts/reconcile-talos-kernel-args/README.md). A KSail pin change requires a new source audit of this fold.

The scan is a **hard gate**: it fails the PR if the combined compliance score drops below the threshold, so new findings must be fixed or justified before merge. It evaluates **two** frameworks, NSA-CISA and MITRE ATT&CK, because NSA-CISA evaluates 20 controls in total but only 17 of the 76 named by the `ClusterSecurityException` CRs — the RBAC controls those CRs exist to govern were outside the gate entirely. Three non-obvious limits:

- **ksail is Renovate-managed** (the Setup step, grouped `ksail` with the deploy pins). It was previously frozen at 7.65.0 because 7.66.x parallelised the in-process Helm render and made it racy — two distinct symptoms of the same regression: `ksail workload validate` non-deterministically corrupted the render with varying YAML parse errors ([devantler-tech/ksail#5362](https://github.com/devantler-tech/ksail/issues/5362), closed — contained since KSail v7.163.1 by the [ksail#5978](https://github.com/devantler-tech/ksail/issues/5978) stream-splitting fix, which is what let the temporary `--skip-helm-render` workaround be removed), and the scan's compliance score swung run-to-run ([devantler-tech/ksail#5371](https://github.com/devantler-tech/ksail/issues/5371), closed). Both are resolved upstream, so the pin is lifted back onto the latest release. Tripwire (kept in sync with the comments in `.github/workflows/ci.yaml`): if `validate` output or the `scan` score varies run-to-run again, re-add `--skip-helm-render` and reopen ksail#5362 (or re-pin to a known-good version, reopening #5371 if only the score swings).
- **52 of the 76 excepted controls are in neither framework, so the gate still cannot see them.** NSA-CISA covers 17 of the 76; adding MITRE ATT&CK brings 7 more under the gate — C-0007, C-0015, C-0031, C-0037, C-0045, C-0048, C-0053 (measured 2026-08-10, ksail 7.178.20). The residue is tracked on [#2823](https://github.com/devantler-tech/platform/issues/2823). Two consequences: an exception naming a control outside both frameworks is inert **here** whether or not it is correct, so never read "the score did not move" as "the exception is broken"; and a finding on such a control reaches neither the gate nor Code Scanning.
- **The threshold is a regression floor, not the actual score — and the scan runs WITH the platform's justified exceptions applied.** The `ClusterSecurityException` CRs (`k8s/bases/infrastructure/cluster-security-exceptions/`) are the single source of truth, and are converted at scan time into Kubescape's native exceptions format by [`scripts/generate-kubescape-exceptions`](scripts/generate-kubescape-exceptions) (fail-closed: an unrecognised CR shape aborts the scan step rather than silently dropping or widening an exception; Go unit tests alongside it), so runtime-enforced (Kyverno mutation, `CiliumNetworkPolicy`) and except-only findings (e.g. **C-0002**, the KubeVirt operator's `pods/exec` RBAC) no longer depress the score and the floor gates the residual REAL posture (#2264). The score has historically been **environment-dependent** (Linux CI runner vs macOS — a gap that is *not* the render mode, the framework cache, or PR-merge content, all ruled out) and shifts with the ksail render, so **CI is the source of truth** (re-baseline the floor after a ksail bump, from the combined and per-framework scores the scan step prints in its log and job summary); the observed CI reference with exceptions applied is **≈98.9%** (2026-07-11, ksail 7.165.2), with the floor a few points under it. Only **two** surfaces apply these exceptions: this scan, and the Headlamp view via the generated `headlamp-exceptions` ConfigMap. The kubescape-operator's stored scan resources (`workloadconfigurationscans` and the two summary kinds) do **not** — their `appliedIgnoreRules` is empty — so a posture number read off those says nothing about whether an exception works. They also outlive the objects they describe for every non-workload kind, so a stored result is not proof that its object still exists either; `scripts/report-kubescape-scan-orphans.sh` classifies them against the live cluster (#3697, [`docs/kubescape-result-coverage.md`](docs/kubescape-result-coverage.md)). A new justified exception is added as a CSE CR (kind+name-scoped, minimal — see the existing CRs' conventions), never by lowering the floor: **ratchet up** as genuine gaps close; never lower it.

### Updating Vendored Operator Bundles

The CDI, KubeVirt and kubelet-serving-cert-approver operator resources, plus the origin-ca-issuer
CRDs, are pinned upstream artifacts. Refresh them only through
[`scripts/update-vendored-operators.sh`](scripts/update-vendored-operators.sh): edit its corresponding
version, source commit and SHA-256 constants and run it from any directory in this repository. The updater downloads the
pinned release assets, reapplies the reviewed resource-scoped Checkov dispositions with the tested
`scripts/annotate-vendored-checkov` helper, and runs Checkov before replacing either committed file.
It requires `curl`, `go`, `sha256sum`, and the Checkov version pinned in the script on the local path.
That pin must equal `CI_CHECKOV_VERSION` in `scripts/megalinter-scan-counts.sh`, the version CI's scan
runs. Every mode of the updater, including CI's offline `--validate-committed`, refuses to run while
the two differ, so bump them in the same PR and re-run the updater on the new Checkov.

This convention deliberately keeps the suppressions narrow: only the named upstream ClusterRole and
Deployment receive annotations, no Checkov check is disabled repository-wide, and an unrelated new
finding still fails the update. A vendor rename/removal also fails closed because the annotator
requires exactly one of every expected target. Do not hand-edit the generated bundles or fetch them
directly; the updater is what makes a future vendor bump retain the dispositions instead of silently
reintroducing the scanner backlog (#2899).
The updater scans the unannotated asset first and refuses a disposition that no longer corresponds
to a current upstream finding, then scans the annotated result across both the Kubernetes and secrets
frameworks. That keeps an upstream fix from leaving a stale exception and prevents embedded secret
material from slipping through the non-blocking repository backlog scan.
The scan runs with an isolated home, empty Checkov config, and no inherited `CKV_*` environment;
upstream annotation and inline-comment suppressions are rejected before either framework runs.
The secrets scan adds one synthetic AWS-key canary and requires Checkov's explicit `secrets` report
to contain exactly that finding, so an empty or silently omitted secrets framework cannot pass.
Each reviewed finding also pins its Checkov evaluated keys and a line-number-independent fingerprint
of the affected resource in `scripts/annotate-vendored-checkov/main.go`. A real vendor bump that
changes either target therefore stops before replacement. Review the upstream resource diff and the
new finding evidence before updating those keys or fingerprints alongside the version and asset
digest; never copy a reported fingerprint without inspecting the changed resource.
CI runs the updater's offline `--validate-committed` mode, removes only those exact configured
disposition lines, requires the remaining bytes to match the pinned upstream SHA-256, and binds each
release version to the operator image tag in that source. Any other manual or automated rewrite of a
generated bundle, or a mismatched version/digest pair, therefore fails before merge.
Its isolated-file scan excludes CKV2_K8S_6 only: Checkov does not model the committed
`CiliumNetworkPolicy` that protects every CDI endpoint, while the full-repository CI scan retains the
check and remains authoritative for graph findings. Before applying that file-level exclusion, the
source validator requires every bundled workload to remain in the corresponding `cdi` or `kubevirt`
namespace covered by those policies.

Use `scripts/update-vendored-operators.sh --render-remotes` to refresh only origin-ca-issuer and
kubelet-serving-cert-approver. Their source URLs use immutable commits and their bytes are pinned by
SHA-256. The CRDs have no workload/RBAC Checkov dispositions: the updater requires their declared
CRD identities and scans the secrets framework with the same canary. The cert-approver retains its
HA deployment and has reviewed resource-scoped dispositions; its namespace is likewise protected by
the committed Cilium policy. Its generated files contain one upstream resource each, preserving the
original document bytes apart from the reviewed annotations. Offline CI concatenates the declared
resource order and validates the original digest and operator image version, so manual edits or a
Renovate version-only bump fail until the whole bundle is re-vendored. CI fails a refresh after which
the local PDB no longer selects the upstream pod labels (see the PodDisruptionBudget check under
[Validation Scenarios](#validation-scenarios)). All these resources render locally without network
access; `scripts/render-remote-resource-exceptions.tsv` has no remaining exceptions.

The cert-approver image is also pinned by **digest**, in its `kustomization.yaml` `images:` entry
rather than in the vendored bytes, because an upstream tag can move (#3515). The entry's tag and
digest must equal the updater's `cert_approver_version` and `cert_approver_image_digest`:
`scripts/guard-cert-approver-image-pin.sh` enforces that inside `--validate-committed`, and
`--render-remotes` re-resolves the tag from the registry and refuses to refresh until the constant
matches. On a bump, review the image the new tag points at, then update the version, the digest
constant and the `images:` entry together.

Renovate tracks the origin-ca-issuer CRD source as a digest of the upstream `trunk` branch, the ref
the CRDs were rendered from before they were vendored (#4136). Its update PR moves only
`origin_ca_issuer_commit`, and that alone cannot go green: `--render-remotes` records the commit it
fetched in `custom-resource-definitions.source-commit` beside the CRDs, and `--validate-committed`
fails while that record and the pin disagree. To finish such a PR, run `--render-remotes` on its
branch and commit what it changes. If the refresh stops on a CRD digest, the upstream CRD changed:
review that change, then record the digest the refresh printed and run it again. Never edit the
record by hand; a record that is missing or malformed is a failed check, not a match.
`scripts/tests/test-origin-ca-issuer-crd-source-pin.sh` pins both halves offline and runs
unconditionally in the `changes` job: its first case validates the tree as committed, so it is the
step that fails on a commit-only bump, on pull requests and in the merge group alike.

That Renovate rule sets `minimumReleaseAge` to `0 days` for this one source. The age of a branch
head is the age of its newest commit, so every upstream commit would restart the repository-wide
cooldown, and a branch that moves at least weekly would never be proposed. Raising the PR adopts
nothing: it cannot merge without the reviewed refresh. Expect a PR for every upstream commit, not
only for CRD changes; a refresh that changes nothing but the record is the normal case.

## Local Development Cluster

**Primary method (requires KSail + Docker):**

```bash
# NEVER CANCEL: full bootstrap takes 3-5 minutes. Set timeout to 10+ minutes.
ksail cluster create

# Push manifests and trigger Flux reconciliation
ksail workload push
ksail workload reconcile
```

The local cluster is a **thin manual test-bed** — a small Talos cluster for trying a component out before promoting it to prod, not a full prod stand-in. By default it brings up only the **core infrastructure** (Cilium + Gateway API, CoreDNS, cert-manager/trust-manager, Flux, metrics-server, Kyverno + cluster-policies, VPA, OpenBao + External Secrets, the Dex/oauth2-proxy/auth-proxy SSO stack, and CloudNativePG) — enough for a working, reachable cluster with prod-like admission, secrets and SSO. Core infrastructure UIs are reachable via the `hosts` file's `*.platform.lan` → `127.0.0.1` mappings (e.g. `dex.platform.lan`, `flux.platform.lan`).

Heavier/optional infrastructure (observability, progressive delivery, autoscaling, backup/Velero + MinIO, runtime security, the VM stack, …) is **opt-in**: uncomment the controller you want — plus its `infrastructure`-layer resources and patch where noted — in the docker provider overlays (`k8s/providers/docker/infrastructure/controllers/kustomization.yaml` and `…/infrastructure/kustomization.yaml`), which carry copy-paste templates. **Apps** are opt-in the same way: replace `resources: []` in the docker apps overlay (`k8s/providers/docker/apps/kustomization.yaml`) with the entries you want — its comments carry a copy-paste template, including the `patches:` block needed for `actual-budget`/`headlamp`. After any change, re-run `ksail workload push` + `ksail workload reconcile`. Only then do the opt-in routes respond — the apex `https://platform.lan` (served by the homepage app) and per-app subdomains such as `headlamp.platform.lan` or `whoami.platform.lan`.

**Cleanup:**

```bash
ksail cluster delete
```

### Local Development Workflow

1. **Setup** — install prerequisites and verify the toolchain.
2. **Start** — `ksail cluster create` (3-5 min, NEVER CANCEL).
3. **Deploy** — `ksail workload push` then `ksail workload reconcile`.
4. **Develop** — edit YAML in `k8s/`.
5. **Apply** — `ksail workload push` and `ksail workload reconcile` again.
6. **Cleanup** — `ksail cluster delete`.

## Production Deployment

Production uses **Talos + Hetzner** via KSail's native Hetzner provider. KSail owns the full lifecycle: Talos boot, Hetzner CCM + CSI install, kubeconfig handoff, and workload push. The committed `ksail.prod.yaml` also drives the KSail-managed Cluster Autoscaler and pins the Talos version/ISO.

**How it works:**

1. Merging a PR through the merge queue runs the `deploy-prod` job in `ci.yaml` (the normal path). A direct push to `main` bypasses the queue, so deploy it manually by running the `CD` workflow (`cd.yaml`, `workflow_dispatch`). Both run the same `ksail` steps below.
2. The `deploy-prod` composite action (shared by both paths) uses `ksail --config ksail.prod.yaml` to target the committed prod config.
3. `ksail.prod.yaml` has `kustomizationFile: clusters/prod`, so KSail/Flux use `k8s/clusters/prod/kustomization.yaml` as the entry point — no root `k8s/kustomization.yaml` or file rewriting is needed.
4. `scripts/run-ksail-prod-with-pull-auth.sh cluster create|update` provisions / reconciles the Hetzner servers, Talos, CCM, and CSI with the Git/SOPS pull credential; the wrapper also passes a SOPS-ciphertext revision so token-only rotations refresh the Cluster Autoscaler machine template.
5. The bridge decrypts only the Git/SOPS pull credential and performs real OCI manifest reads for all ten consumers (the Platform and tenant manifest artifacts, the data-product-controller image, the application and World at Ruin zone images, and the KSail plus provider-upjet-unifi packages used by Kyverno verification). On nodes with a changed credential revision, it applies Talos `RegistryAuthConfig` workers-first, reboots under a scheduling fence, removes the exact incoming KSail image from the CRI cache, proves a registry-backed pull, and only then records both proof markers. When the credential proof is current and only the verified image differs, it instead removes and pulls a proof copy in Talos containerd's `system` namespace without cordoning or evicting the Kubernetes CRI image; a failed registry pull leaves that workload cache intact and does not publish a new proof marker. It then updates `variables-base`, force-syncs and verifies the PushSecret plus tenant/Kyverno ExternalSecrets, and finally reasserts root auth — all before a mutable `latest` tag is published. The DR workflow first runs `--check-only` before creating infrastructure, then uses explicit `--allow-incomplete-fanout` bootstrap mode after cluster creation and requires a full bridge pass after Flux converges.
6. `scripts/run-ksail-prod-with-pull-auth.sh workload push` packages manifests and pushes them with the separate Actions write token.
7. `scripts/refresh-flux-ghcr-auth.sh --check-only` revalidates the newly-published artifact without mutating the cluster.
8. `scripts/reconcile-flux-workloads.sh` triggers Flux with Git/SOPS pull auth (DR calls `scripts/run-ksail-prod-with-pull-auth.sh workload reconcile` directly). The wrapper exists because a change to the Flux control plane restarts the controllers the reconcile runs through, cancelling the reconcile the deploy itself triggered; it retries once, and only on evidence that this deploy restarted the control plane, so a genuine failure still fails on the first attempt. Both normal delivery and DR then require `infrastructure-controllers` to apply the exact newly-published digest and report `Ready`.
9. After `cluster update`, the full bridge reasserts every pull path in case a partial update or older managed state was applied. The normal deploy passes a non-secret, same-job handoff that binds the staged credential revision and image to each Kubernetes Node UID. If KSail only erased the Talos proof annotation, the reassert restores that marker without repeating the uncached pull or rolling reboot; a changed credential, image, or Node UID still takes the full proof path. When nothing has drifted, the bridge first proves that read-only — root auth, `variables-base` and every consumer hold the Git/SOPS value, the `ghcr-seed-probe` ExternalSecret has just read that value back out of OpenBao, admission already enforces the candidate policy, and two node inventories show current proof and no fences — and then exits without taking the synchronization Lease, pausing Flux, restarting kustomize-controller or writing anything. Any failed read or mismatch runs the full fenced transaction instead. DR applies the same Cilium rollout guard around publish and convergence, and also runs the bridge after an OpenBao raft restore because the snapshot may contain an older GHCR value.

**Key differences from local:**

- OCI artifacts are pushed to **GHCR** (not a local registry).
- Nodes are real Hetzner servers; `ksail cluster update` can scale workers in place or swap ISO versions, and the KSail-managed Cluster Autoscaler adds/removes compute-only workers within configured pools.
- Ingress is a real Hetzner Cloud Load Balancer provisioned by the hcloud CCM from the Cilium Gateway's Service.
- DNS A/AAAA records at the apex + wildcard must point at the LB IP (a human step — see `docs/dr/runbook.md` scenario 4).

### Dual-Provider Model

- **Local / CI:** `ksail cluster create` → Talos + Docker provider → local OCI registry → `ksail workload push` / `reconcile`.
- **Production:** `scripts/run-ksail-prod-with-pull-auth.sh cluster create|update` → Talos + Hetzner provider → Hetzner CCM + CSI installed by KSail → the same wrapper's `workload push` to GHCR → `workload reconcile`.

## CI/CD Pipelines

- **`ci.yaml`** — runs on `pull_request` (static manifest validation + Kubescape scan, no cluster) and `merge_group` (deploys prod via the Hetzner provider). Concurrency is shared with `cd.yaml` so a manual deploy and a merge-queue deploy can never run against the prod cluster at the same time.
- **`cd.yaml`** — runs on `workflow_dispatch` (manual). Deploys to the production Hetzner cluster using `ksail --config ksail.prod.yaml`. Covers direct pushes to `main`, which bypass the merge queue and so are not deployed by `ci.yaml`.
- **`.github/actions/deploy-prod`** — the composite action both regular deploy paths call (stage/verify all GHCR pull consumers → push → cosign-sign → attest SBOM + SLSA provenance → revalidate published artifact → Flux reconcile → exact published-revision Ready proof → Talos `cluster update` → final reassert → gateway route check), so the merge-queue and manual deploys can never drift. The last step fails the deploy when the gateway has not applied every HTTPRoute at its current generation within five minutes, when it rejected a route or a route names a missing Gateway, or when a running Cilium operator logged that its Gateway API controller failed to start (#4198). DR uses the same exact-revision wait and rollout guard. **No Cilium rollout gate is active**, so every deploy runs its `cluster update` Talos machine-config sync. The guard still runs on both paths and is machinery for the next staged rollout: it reads as active only while the `homogeneous-devices` component is referenced **and** carries `type: OnDelete`. While that holds it suspends Cluster Autoscaler, proves no provider-side node addition is in flight before publish, and skips `cluster update` until a reviewed completion or rollback artifact has applied — a skip that happens inside an otherwise-green deploy, so it is time-bounded against a `platform.devantler.tech/rollout-gate-activated:` marker declared beside the component reference: `scripts/report-cilium-rollout-gate-suppression.sh` **warns** from `warn_after_days` (7) and **fails the deploy** from `fail_after_days` (14). Resolve an expired gate by stepping the remaining Cilium agents onto the current DaemonSet revision or rolling the component back — raising either bound is not a resolution. Secrets are passed as inputs because composite actions cannot read `secrets`.

**Required GitHub Secrets:**

- `GHCR_TOKEN` — long-lived PAT (owner: `devantler`) with `write:packages` scope, used only for OCI push/signing. It is **not** a pull credential.
- `SOPS_AGE_KEY` — Age private key for SOPS secret decryption.
- `HCLOUD_TOKEN` — Hetzner Cloud API token (read/write), used by the KSail Hetzner provider and by the Hetzner CCM / CSI at runtime.

The authoritative **production pull** credential for Flux, tenants, Kyverno,
and Talos hosts is
`stringData.ghcr_dockerconfigjson` in
`k8s/bases/bootstrap/secret.enc.yaml`. The deploy bridge refreshes
`flux-system/ksail-registry-credentials` from that value before Flux must fetch
the artifact and reasserts it after `cluster update` in case KSail rewrites its
managed Secret. Before publish on existing clusters, the bridge updates `variables-base`,
force-syncs `seed-ghcr` into OpenBao, force-syncs the tenant/Kyverno
ExternalSecrets, and verifies their materialised `ghcr-auth` payloads before
switching root Flux auth. Only explicit DR bootstrap mode may repair root auth
after staging `variables-base` while the fan-out is incomplete; DR must run the
full verifier after Flux converges. A direct credential commit to `main` still
needs a manual `CD` workflow dispatch because direct pushes bypass the merge-queue
deploy.
The lifecycle wrapper injects the same username/token into KSail's local
registry and Talos patches. A non-secret hash of the committed SOPS ciphertext
is the desired machine-template revision; the bridge stores a separate verified
revision on each existing node only after an exact image pull succeeds.

**Required GitHub Variables:** none.

### Production supply-chain tools

The shared publication action installs Cosign and Syft through
`.github/scripts/setup-supply-chain-tools.sh`. Both Linux x86_64 release assets must match their
repository-local SHA-256 pins before either tool is installed or executed. Keep each version and
digest together in the same PR, taking the digest from that release's published checksum manifest;
a version-only Renovate update fails verification. Do not replace this path with installer actions.
Run `bash scripts/tests/test-setup-supply-chain-tools.sh` and `go test ./scripts/validate-dr-signing`
when changing the installer or publication action. CI also runs the verified tools and generates a
CycloneDX SBOM without production credentials.

## Working with Secrets

This platform uses SOPS with Age encryption for all secrets. **Never decrypt a secret into a
terminal, transcript, or file — use the non-printing primitives** (see the absolute rules under
*Validate before any manifest PR* below):

```bash
# Change a value in place — nothing is printed, the file stays encrypted
sops set k8s/clusters/local/bootstrap/variables-cluster-secret.enc.yaml '["stringData"]["key"]' '"value"'
sops unset <file>.enc.yaml '["stringData"]["obsolete-key"]'

# Re-encrypt to new recipients after a .sops.yaml change
sops updatekeys <file>.enc.yaml

# Encrypt a new secret (then delete the plaintext source)
sops -e --input-type yaml --output-type yaml secret.yaml > secret.enc.yaml
```

You **cannot** decrypt existing secrets without the proper Age keys. For local development on a fork:

1. Fork the repository.
2. Generate your own Age keys: `age-keygen -o key.txt`.
3. Update `.sops.yaml` with your public key.
4. Re-encrypt all `*.enc.yaml` files with your key.

## Previously Protected Files — Editable Since 2026-07-16

The maintainer lifted the never-modify list on 2026-07-16 — no file in this repo is off-limits any
more. `ksail.prod.yaml` is ordinary config (draft PR, validated, reasoning in the body). `*.enc.yaml`
and `.sops.yaml` are editable **only** through the non-printing SOPS workflow and its absolute rules
(never decrypt into the session; verify `ENC[AES256_GCM,` before staging) — see *Working with
Secrets* above and the rules under *Validate before any manifest PR* below.

## Conventions

- **Semantic commits** — use Conventional Commit messages (e.g. `feat:`, `fix:`, `chore:`); semantic-release runs off them.
- **Draft PRs** — always create PRs as drafts.
- **Small, focused changes** — one concern per PR.
- **Never commit plaintext secrets** — all secrets must be SOPS-encrypted with the `.enc.yaml` suffix.
- **Put a change in the layer that matches its scope** — edit `k8s/bases/` when it should hold for **every consumer of that resource**; add an overlay `patches/` fragment only for a genuine per-consumer difference. "Every consumer" is **not** "every cluster": much of `k8s/bases/` has a single consumer today by design, since the local Docker overlay opts *in* to apps and heavier infrastructure rather than deploying them. A shared component's canonical configuration still belongs in its base — patching it into the one provider that happens to use it today leaves the base stale for whoever opts in next. Bases are shared, not frozen: editing them is the ordinary case, not an exception — `k8s/bases/` changes in about half of all commits, far more often than the overlays it feeds. What to avoid is mutating a base to obtain a *per-overlay* result — that silently moves every other consumer with it. One question decides it: **would every consumer of this resource want the change?** If yes, the base is correct.
- **Flux dependency order** — `bootstrap` → `infrastructure-controllers` → `infrastructure` → `apps`. One prod-only side layer hangs off `infrastructure` without gating `apps`: `infrastructure-overprovisioning` (apply-only autoscaler buffer). Declarative GitHub org management runs as a normal **app** (`github-config`) consuming the `devantler-tech/.github` artifact, with its Crossplane provider in the `infrastructure` layer — see [`docs/github-management.md`](docs/github-management.md).
- **File & directory naming** — kebab-case folders, one resource per file, and filenames led by the resource Kind (CR folders and `patches/` excepted — both name files by intent). Talos machine-config patches (`talos/`, `talos-local/`) also hold one document per file with intent names; only the k8s-manifest-specific rules don't apply to them. Enforced by the `naming` CI job. See [File and Directory Naming Conventions](#file-and-directory-naming-conventions) below.

### File and Directory Naming Conventions

Enforced by the shared `devantler-tech/.github/actions/validate-naming` action in the
`naming` job in `ci.yaml`. Validation is enabled by default, and CI requires its
completed-scan output. Repository-owned roots and exceptions live in
[`.github/manifest-naming.yaml`](.github/manifest-naming.yaml); the `talos*` root
pattern includes new machine-config environments automatically. Run the same
pinned gate locally before a manifest PR, with Go installed:

```bash
actions_dir="$(mktemp -d)"
git clone --quiet --depth 1 --branch v4.9.1 https://github.com/devantler-tech/.github.git "$actions_dir"
git -C "$actions_dir" checkout --quiet --detach 30882cccda9e41c622f6493e21e7eb6f3339b91f
GOWORK=off go -C "$actions_dir/actions/validate-naming" run -mod=readonly . \
  --root "$PWD" --config .github/manifest-naming.yaml
```

Go setup and dependency downloads need network access; validation itself is
offline and never contacts a cluster.

- **Directories are kebab-case**, named after the **application/component** *or* a **CR Kind in plural**. Co-locate a component's own CRs in its folder by default; break a CR out into a `‹kind-plural›/` folder only when it cannot live with its component (see the two reasons in the next section). `‹kind-plural›` is the **kebab-cased plural of the Kind** (`VerticalPodAutoscaler → vertical-pod-autoscalers/`, `LimitRange → limit-ranges/`) — a folder that groups ≥2 instances of one non-workload Kind under any other name is flagged.
- **One Kubernetes resource per file** — patch fragments included. The only exception is a vendored upstream operator bundle, listed in the configuration's `multi-resource-files` (today `controllers/cdi/cdi-operator.yaml` and `controllers/kubevirt/kubevirt-operator.yaml`).
- **Component-folder files are named after their resource Kind, kebab-cased**: `‹kind›.yaml` (e.g. `helm-release.yaml`, `http-route.yaml`, `cilium-network-policy.yaml`, `service-account.yaml`). When a folder holds more than one of a Kind, qualify each with a purpose: `‹kind›-‹purpose›.yaml` (e.g. `external-secret-db-backup.yaml`). The Kind→kebab map is acronym-aware: `HTTPRoute → http-route`, `OCIRepository → oci-repository`, `CiliumNetworkPolicy → cilium-network-policy`, `PodDisruptionBudget → pod-disruption-budget`.
- **CR-folder files** omit the folder-implied Kind and are named `‹verb›-‹purpose›.yaml` (e.g. `restrict-tenant-secret-stores.yaml`).
- A **Flux `Kustomization` CR** (`kustomize.toolkit.fluxcd.io`) is named `flux-kustomization.yaml` or `flux-kustomization-<purpose>.yaml`; the `flux-` prefix disambiguates it from the kustomize **build** file, which must stay exactly `kustomization.yaml` (`kustomize.config.k8s.io`).
- **Patch fragments** are overlay inputs, not deployed resources. They live under a `patches/` directory (a `*-patch.yaml` loose next to a kustomization is flagged as misplaced) and follow the **CR-folder naming convention**: an intent-describing `‹verb›-‹purpose›.yaml` (e.g. `enable-oidc.yaml`, `store-spire-data-on-hcloud.yaml`) that neither leads with the patched Kind nor carries a `-patch` suffix — the folder already says it's a patch. One-resource-per-file applies to them too; a patch on a Flux `Kustomization` CR keeps the `flux-kustomization` prefix (e.g. `flux-kustomization-protect-wedding-db.yaml`).
- **Talos machine-config patches** (`talos/`, `talos-local/`) follow the same spirit: **one YAML document per file** and intent-describing `‹verb›-‹purpose›.yaml` names (e.g. `enable-apparmor.yaml`, `block-ingress-by-default.yaml`, `allow-apid-ingress.yaml`). They are Talos config fragments, not Kubernetes manifests, so the k8s-specific rules — Kind-led filenames, `patches/` placement, the `flux-kustomization` prefix — are the only parts that don't apply. Ingress-firewall rule files stay **one `NetworkRuleConfig` per file**, but keep the rule *count* low by consolidating ports into an existing rule when protocol + subnets match (see the ENOBUFS note in `talos/control-planes/allow-public-ingress.yaml`).

### Infrastructure File Structure Convention

Resources under `k8s/bases/infrastructure/` are **component-folder-first**: a component's HelmRelease/HelmRepository and its own CRs live together in a folder named after the component — `controllers/<component>/` in the controller layer, and a sibling folder in the `infrastructure` layer (e.g. `gateway/`, `coroot/`, `policy-reporter/`, `vault-*/`). The central Cilium `Gateway`, its HTTP→HTTPS `HTTPRoute` and its TLS `Certificate` all live in `gateway/` and deploy to `kube-system` (the Cilium namespace).

A CR is split out into its own **plural-Kind folder** only when it cannot live with its component:

- **Dependency split** — the CRD ships with the controller's HelmRelease, so the CR must reconcile a layer later to avoid the CR-and-its-CRD-in-one-Kustomization deadlock: `metric-templates/` (Flagger `MetricTemplate`; see [`docs/progressive-delivery.md`](docs/progressive-delivery.md)), `tracing-policies/` (Tetragon `TracingPolicy`), the Coroot CR in `coroot/`, and `resource-graph-definitions/` (KRO, which also installs its CRD via the controller layer).
- **Cluster-scoped / cross-cutting** — no single owning component: `cluster-policies/` (Kyverno), `cluster-roles/` + `cluster-role-bindings/`, `cluster-secret-stores/`, `external-secrets/` (bootstrap ExternalSecrets), `cluster-security-exceptions/` (Kubescape), `limit-ranges/`, and `vertical-pod-autoscalers/` (prod system VPAs).

### Kustomization Flow

The platform uses a hierarchical kustomization structure: **base** configurations in `k8s/bases/` → **provider-specific** overlays in `k8s/providers/` → **cluster-specific** overlays in `k8s/clusters/`. The cluster overlay's `cluster-meta` ConfigMap drives Kustomize `replacements:` that repoint each Flux Kustomization (`bootstrap`, `infrastructure-controllers`, `infrastructure`, `apps`) at the correct provider/cluster path.

## Timing Expectations and Warnings

**CRITICAL: NEVER CANCEL long-running cluster commands.** (These apply to full local/prod runs only — maintenance work uses static validation and does not run a cluster.)

- **`ksail cluster create`** — 3-5 minutes for full bootstrap. NEVER CANCEL. Timeout 10+ minutes.
- **Cluster create (provisioning step alone)** — ~30-45 seconds. NEVER CANCEL. Timeout 5+ minutes.
- **`ksail cluster delete`** — ~1-2 seconds. NEVER CANCEL. Timeout 2+ minutes.
- **Flux reconciliation** — 2-5 minutes per kustomization. NEVER CANCEL. Timeout 10+ minutes.
- **Tool installation** — 1-3 minutes total (apt update alone can take 30+ seconds). NEVER CANCEL. Timeout 5+ minutes.
- **`kubectl kustomize` build** — under 1 second.

## Known Limitations and Workarounds

### macOS Port Exposure
- LoadBalancer / virtual IPs are not directly reachable from macOS Docker Desktop (Docker VM isolation).
- Port mappings in `ksail.yaml` under `spec.cluster.talos.extraPortMappings` expose ports 80 and 443 from the Talos Docker container to the host.
- The `hosts` file maps the `*.platform.lan` names to `127.0.0.1`.

### SOPS Decryption Requirements
- Existing secrets cannot be decrypted without the proper Age keys.
- **Workaround:** fork the repository and use your own Age keys; re-encrypt every `*.enc.yaml` with your key.

### CNI Configuration
- The Talos cluster starts with its default CNI disabled (via `talos-local/cluster/disable-default-cni-and-kube-proxy.yaml`).
- Nodes stay `NotReady` until Cilium is installed by KSail.
- This is expected — KSail handles CNI installation automatically.

## Validation Scenarios

After making changes, validate at the appropriate level. **For maintenance, only the static checks below apply.**

### Static (always, no cluster)
1. **Kustomize build** — the cluster overlays and every layer listed under [Validation](#validation) build; the overlays alone cover only the Flux wiring.
2. **YAML / schema** — `kubectl apply --dry-run=client -f <file>` on changed manifests (no Flux variable substitution).
3. **Coverage** — each changed file appears in the validator's output (its path for `ksail workload validate`, its resource for a layer build); a zero exit alone proves nothing.
4. **Post-renderers** — no build above executes a HelmRelease's `postRenderers`; only helm-controller does, against the chart's rendered output. When a change touches a release that carries them (its post-renderers, chart version, values, substitution variables or Kubernetes pin), run `bash scripts/guard-helm-post-renderers.sh --flux-version 2.8.8 --base origin/main --base-kube-version "$(git show origin/main:ksail.prod.yaml | yq -er '.spec.cluster.kubernetesVersion')" --kube-version "$(yq -er '.spec.cluster.kubernetesVersion' ksail.prod.yaml)" k8s` with Helm v4.2.0: it pulls each changed release's chart, renders it with the release's own values as an install and an upgrade, and applies its post-renderers, failing where one cannot apply (#3581). The audited offline profile refuses unsupported historical or SDK capability inputs. CI runs the same check in the `validate-helm-post-renderers` job.
5. **PodDisruptionBudget selectors** — a budget whose selector matches no pod is valid YAML and passes every build and scan above; its only symptom is `expectedPods: 0` on the live cluster. When a change touches a budget, the pod labels of a workload one guards, a chart version or values of a release one guards, or a Flagger `Canary`, run `bash scripts/guard-pdb-selector-match.sh --kube-version "$(yq -er '.spec.cluster.kubernetesVersion' ksail.prod.yaml)" k8s`. It needs Go, to build the chart renderer once (`CONTROLLER_HELM` names one already built by `scripts/build-controller-helm.sh`), and network access to pull charts. It renders both clusters and requires each budget to select a workload rendered into its namespace, rendering the HelmReleases installed beside a budget the manifests do not satisfy (with their values and post-renderers, as an install and an upgrade) and deriving the `<target>-primary` workload Flagger serves from, whose pod labels differ from the target's, for a `Canary` in the manifests or in a rendered chart (#3596, #4365). Replica counts are not judged. A budget whose pods nothing here renders (an operator building its workload at runtime, the Talos-installed CoreDNS) takes a reviewed row in [`scripts/pdb-selector-exceptions.tsv`](scripts/pdb-selector-exceptions.tsv) naming the one cluster it applies to, what builds the pods, the version pin their labels were verified at, and where. The guard does not check an excepted selector against its pods; what it enforces on a row is narrower: it applies to no other cluster; it is refused while that cluster renders, into the budget's namespace, a workload carrying part of what the selector names, unless the row lists that workload as reviewed and unrelated; it is refused when it names a workload or a HelmRelease the guard can render as the producer; and it fails when its pin (a chart version, or the Talos version in `ksail.prod.yaml`) no longer equals the verified version, so a bump cannot merge until the labels are verified again. A stale row fails. A budget it cannot decide is exit 2, never clean, and each such entry leads with the cause; a chart pull is tried three times before it counts as failed. CI runs it on pull requests in the `validate-pdb-selectors` job.

### Cluster scenarios (CI / full local dev only)
1. **Cluster creation** — `ksail cluster create` succeeds.
2. **Node status** — nodes become `Ready` after Cilium installation.
3. **Pod deployment** — core pods start successfully.
4. **Ingress / app access** — app routes respond (if configured).
5. **Secret handling** — SOPS integration works.

Illustrative healthy local node listing (Kubernetes version tracks the pinned Talos release, so the exact `VERSION` will vary):

```bash
# kubectl get nodes (after Cilium installation)
NAME                  STATUS   ROLES           AGE   VERSION
local-controlplane-1  Ready    control-plane   5m    v1.xx.x
local-worker-1        Ready    <none>          4m    v1.xx.x
```

## Emergency / Recovery Procedures

### Local Cluster Recovery

```bash
# If the local cluster is unresponsive
ksail cluster delete
ksail cluster create

# Then redeploy workloads
ksail workload push
ksail workload reconcile
```

### Production Cluster Recovery

With the KSail Hetzner provider the cluster is cattle — rebuild it in place:

```bash
export HCLOUD_TOKEN=...
export WG_SERVER_PRIVATE_KEY=...
export SOPS_AGE_KEY_FILE=~/.config/sops/age/keys.txt
export GHCR_TOKEN=...  # publication only
export GITHUB_ACTOR=devantler
./scripts/run-ksail-prod-with-pull-auth.sh cluster update
# For a full rebuild from zero, see docs/dr/runbook.md scenario 4.
./scripts/run-ksail-prod-with-pull-auth.sh workload push
./scripts/run-ksail-prod-with-pull-auth.sh workload reconcile
```

### Tool Reinstallation

If tools stop working, reinstall in order: Docker (restart the service if needed) → KSail (`brew reinstall ksail`) → kubectl (check the cluster context) → SOPS, Age, and yq v4 (check the encryption keys and `yq --version`).

## What's Useful for the AI Assistant

- **Issue labelling and triage** — very helpful.
- **Issue investigation** — manifest misconfigurations, Helm chart issues, Flux sync / dependency-order problems.
- **Engineering investments** — Helm chart version bumps (via HelmRelease `spec.chart.spec.version`), GitHub Actions updates.
- **Manifest improvements** — Kustomize structure cleanup, documentation gaps, dead-resource removal.
- **Testing and refactoring** — Go commands, Bash tooling and their offline regressions; preserve fail-closed behaviour and observed output when simplifying them.
- **Performance investigation** — measured manifest rendering, validation and operational-tool bottlenecks.

## Maintenance

Repository maintenance follows the **Agentic Engineer** contract in the [devantler-tech monorepo `AGENTS.md`](https://github.com/devantler-tech/monorepo/blob/main/AGENTS.md) and its [agent guides](https://github.com/devantler-tech/monorepo/tree/main/.claude/guides). Resolve trusted identities and the registered writer namespace from its **Trust gate** and **Writer namespaces**; use the [claim protocol](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/claim-protocol.md) and [worktree rules](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/git-and-worktrees.md) before editing. The [readiness guide](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/pr-readiness.md) governs draft promotion and user evaluation, the [merge policy](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/merge-policy.md) governs PR ownership, dependency automation and author-specific merge commands, and the [artifact conventions](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/github-artifacts.md) govern titles and disclosure. Shared rules are not redefined here. Before editing manifests, also skim the manifest-structure sections above.

**Platform evaluation** — inspect rendered output for the intended manifest effect, or use authorized read-only checks against the running cluster; after deployment, confirm the effect the same read-only way. Static checks do not prove runtime behaviour or satisfy a named provider or rollout gate. A change with no exercisable runtime surface (documentation or agent instructions) explains that in its readiness comment. Maintenance validation starts no cluster.

**Validate before any manifest PR** — prefer `ksail workload validate` (and `ksail --config ksail.prod.yaml workload validate`) for schema-aware checks with Flux substitution when KSail is installed; it does not start a cluster. Without KSail, the cluster overlays and every layer Flux reconciles MUST build — `kubectl kustomize` on `k8s/clusters/<local|prod>/`, `k8s/providers/<docker|hetzner>/{infrastructure/controllers,infrastructure,apps}/` and `k8s/clusters/<local|prod>/bootstrap/` (standalone `kustomize` isn't installed; `kubectl` has it built in). The overlays alone build only the Flux wiring, not the manifests, so they are no check on a manifest change, and none of these builds substitutes Flux variables or checks schemas. Per-file: `kubectl apply --dry-run=client -f <file>`, which substitutes no Flux variables either. Confirm each changed file appears in the validator's output (its path, or its resource in the layer build) rather than trusting a zero exit. CI runs the same static checks on k8s PRs (`ksail workload validate` for both overlays + a Kubescape `scan`) — there is no full-cluster system test to rely on, so validating locally matters more. **Never run a cluster** (no `ksail up`/create/switch/delete, no mutating `~/.kube/config`). **No file in this repo is off-limits any more — the maintainer lifted the never-modify list on 2026-07-16** (`ksail.prod.yaml` first, then `*.enc.yaml` + `.sops.yaml`). `ksail.prod.yaml` is now ordinary config: draft PR, validated, reasoning in the body — the old rule had left a one-line fix unshippable through two prod-CD outages. **The SOPS files are editable but NOT ordinary — they carry live secrets, and the failure mode is irreversible, so these rules are absolute:**
- **NEVER decrypt into the session.** No `sops -d` to stdout, no `cat`/`Read` of a decrypted file, no plaintext in a command's output. Transcripts are durable: a secret that reaches one is leaked, full stop. *(Maintainer's condition, verbatim: "as long as you do not read the unencrypted files into the session".)*
- **Edit in place with the non-printing primitives**, never a decrypt→edit→encrypt round-trip: `sops set <file> '["key"]' '"value"'` and `sops unset` change a value without emitting the document; `sops updatekeys <file>` re-encrypts to new recipients after a `.sops.yaml` change.
- **Verify a file is still ENCRYPTED before you stage it.** It must contain a `sops:` metadata block and `ENC[AES256_GCM,` values. If either is missing it is plaintext — do NOT stage it.
- **`.decrypted*` is gitignored (`.gitignore:16`) and no such file has ever been committed. Keep it that way:** never `git add -f` one, never remove that ignore rule, and stage explicit paths only (never `git add -A`).
- **If plaintext ever reaches git or a transcript, the secret is COMPROMISED** — revoke immediately (containment outranks continuity), then sweep every copy per the monorepo `AGENTS.md` credential-rotation rule. Do not quietly fix it up.

**Platform work areas** — selection, priority and finishing existing PRs follow the monorepo's **Work-selection ladder**:

- **Manifest and runtime investigation:** Helm configuration, Flux dependency ordering, policy enforcement and confirmed platform faults.
- **Engineering investments:** HelmRelease chart pins and GitHub Actions/workflow health.
- **Manifest improvements:** Kustomize cleanup, dead-resource removal and documentation gaps.
- **Tooling improvements:** Go and Bash tests, refactoring and measured performance work with the validation described above.

**Merge queue — `main` IS gated by a GitHub merge queue** (`Require merge queue` ruleset). Use the monorepo [merge policy's author-specific commands and queue rules](https://github.com/devantler-tech/monorepo/blob/main/.claude/guides/merge-policy.md); the queue does not widen permission to use `--auto`. The queue sets the strategy, so omit `--squash` and retain the repository and exact-head pins. Confirm queue membership with `isInMergeQueue` or `mergeQueueEntry`: `autoMergeRequest` can stay `null` while a PR is queued. A queued PR runs the **`merge_group`** event of `ci.yaml`, whose `deploy-prod` job **deploys to the real prod cluster** — so a `merge_group` failure **evicts the PR from the queue**. **Root-cause a stall/kick-out before re-queuing** (per the monorepo contract *Merge policy → Merge-queue repos*): a PR that "was queued" but didn't merge has usually failed its `merge_group` run — pull it (`gh run list --repo devantler-tech/platform --event merge_group --json headBranch,conclusion` → `pr-<n>` → `gh run view --repo devantler-tech/platform --log-failed`) and diagnose. The `deploy-prod` step's inline tenant provisioning can still expose a real platform fault during the gating verify; when that happens, re-queuing just re-hits it — advance the root-cause fix rather than looping the PR. Only a genuine one-off transient (runner OOM, network) warrants a clean re-queue.

**Tell a timeout eviction from a failed-check eviction before diagnosing the run.** They look the same on the PR, and a timeout can follow a `merge_group` run that later ends `success`. The PR timeline records which one it was:

```sh
gh api graphql -f query='{repository(owner:"devantler-tech",name:"platform"){pullRequest(number:<n>){timelineItems(itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT],last:10){nodes{... on RemovedFromMergeQueueEvent{createdAt reason}}}}}}'
```

`failed_checks` means a required check failed, so diagnose the `merge_group` run as above. `checks_timed_out` means no result arrived within the queue's check-response timeout. That timeout is 90 minutes and is managed in `devantler-tech/.github` (`deploy/repository-rulesets/require-merge-queue-on-platform.yaml`). For a timeout, look for what kept the run waiting, such as the `prod-deploy` concurrency lock or a runner queue, rather than a deploy fault. `manual` means someone removed the PR from the queue, and `merged` is a normal merge.

**Green checks with `mergeStateStatus: BLOCKED` do not identify the blocker.** Rule out unresolved
threads, approvals and other branch rules first. A missing current-head run of an effective required
workflow can produce the same state, and workflow job names do not identify the workflow.
Read the [merge-queue diagnostic runbook](docs/operations/merge-queue-blocked.md) on demand;
it derives workflow paths and source identities from the live effective rules for `main`.
A missing or failed read is UNKNOWN, and enqueueing is a production-deploying write.

**Safe cancellation:** once a merge-group `deploy-prod` job enters the shared deploy composite, it
may already have pushed the speculative ref to the mutable `latest` tag. Use only a normal workflow
cancellation; the `always()` heal job treats the cancelled deploy as unsuccessful and restores the
current tip of `main` after the production lock is released. Never force-cancel this workflow:
GitHub's force-cancel endpoint bypasses conditions such as `always()` and can strand the speculative
artifact. If a legacy/cancelled run did not execute `🩹 Heal Prod`, dispatch `CD` on `main` and
verify that deployment before treating the production lane as clean.

**Persistence retirement is always two-stage.** A merge-group artifact is speculative, but
Kubernetes PVC deletion is irreversible once `deletionTimestamp` is set: queue eviction and the
heal job cannot un-delete it. The production persistence-safety component therefore disables Flux
pruning on every PVC, HelmRelease, and Namespace. Platform and generated tenant Flux layers set
`spec.force: false`; only individual Jobs that need recreation carry `force: enabled`.
`force: disabled` on a resource cannot override a forcing layer. The replacement-safety guard
checks layer defaults, both tenant template branches, and rendered patches before publication.
HelmRelease protection prevents chart uninstall from deleting chart-owned claims; Namespace
protection prevents cascading deletion from bypassing a claim's own annotation. To retire any of
these objects, first merge and deploy that protection in its own revision; only a later PR may
remove the manifest. After the second PR lands and no workload depends on the orphan, delete it
explicitly. The `observability/prune-protected-orphan-alert` CronJob is the proof that this last
step is done: it lists every prune-protected PVC, HelmRelease, Namespace and CloudNativePG Cluster
that is missing from its Kustomization's inventory, logs it while it is younger than seven days and
posts it to Slack after that. Run it on demand with `kubectl -n observability create job
--from=cronjob/prune-protected-orphan-alert "prune-protected-orphan-check-$(date +%s)"`; step 3 is
complete when that Job **Succeeded** and its log does not name the object (a failed Job judged
nothing, and an object still Terminating is not listed). The heal's orphan check includes older
objects written by Flux during the speculative deployment and trusts an inventory only after its
Kustomization and Source report Ready for their current generations at the same revision. Active
reconciles, Unknown readiness and attempts at another source revision must settle even when the
existing inventory contains only older objects. When a
protected object is instead handed to another controller on purpose, annotate it
`platform.devantler.tech/prune-orphan: adopted` in
the PR that protects it, so the check does not report it; an owner reference alone is not that handoff.
`scripts/tests/test-pvc-prune-safety.sh` checks every production reconciliation root,
rejects an unprotected current or base resource, and compares a deploy candidate with the actual
live Flux-owned objects before the mutable production artifact moves. Do not collapse the two
revisions or use Flux force replacement for a PVC migration.

This is the rule in force today, and it is being replaced.
[`docs/adr/storage-retention.md`](docs/adr/storage-retention.md) records the decision
to keep data at the storage layer (`Retain` on every StorageClass and PersistentVolume) and to
remove every prune opt-out, and the order that happens in. Until the step that removes the opt-outs
(#4444) has merged and deployed, everything above still applies unchanged, including the protection
on new stateful resources.

**Feature flags — four independent layers (feature-flag-first, monorepo#2059).** Land new behaviour **off**, validate it, then flip it on — using the right layer, coarsest first:
1. **Runtime per-request flags → flagd + OpenFeature Operator** (`k8s/bases/infrastructure/controllers/openfeature-operator/`, `#2510`). Flag definitions live in Git as **`FeatureFlag` CRs** (`core.openfeature.dev/v1beta1`) reconciled by Flux; workloads opt in with the `openfeature.dev/enabled` + `openfeature.dev/featureflagsource` pod annotations. Prefer **flagd-proxy** sync (`provider: flagd-proxy` on the `FeatureFlagSource`) so pods need no cluster-wide API RBAC — and so Flux never fights the operator over the `flagd-kubernetes-sync` ClusterRoleBinding (that drift only happens under `provider: kubernetes`). A `FeatureFlag` CR belongs in the **`infrastructure` layer**, never the controllers layer (a CR can't share a Flux Kustomization with the controller that installs its CRD).
2. **Version rollout / traffic shifting → Flagger** (already deployed): the release/canary toggle — "is this build safe to shift traffic to?", metric-analysed auto-rollback. Distinct from per-user flags; not a runtime flag.
3. **Coarse component on/off → Helm `values` (`{{- if .Values.x.enabled }}`) + Kustomize overlays** — the low-tech gate; prefer values for simple on/off, reserve patches for what values can't express.
4. **Platform behaviour → Kubernetes `--feature-gates`** (alpha/beta/GA) — orthogonal, owned by Talos machine config.

**Pick the right tool, not always a flag:** a permanent setting is plain config; a version/traffic rollout is Flagger (layer 2), not a runtime flag. **Flag lifecycle:** a *release* flag is short-lived and **removed after rollout** (file the removal when it's born); only *kill-switch* and *permissioning* flags are long-lived. FeatureFlag/FeatureFlagSource CRDs are runtime-installed, so add them to `validation.skipKinds` in `ksail.yaml`+`ksail.prod.yaml` when the first CR lands (same as the Flagger/Tenant CRDs).

CI checks every authored `FeatureFlag.spec.flagSpec` with the pinned, offline
flagd schema via [`go run ./scripts/validate-feature-flags`](scripts/validate-feature-flags/README.md). Run it before adding
or changing flags. The check reports empty coverage explicitly until a real flag
exists; skipping the runtime-installed CRD's manifest schema does not skip its
flag definitions. The operator requires a string default variant, which must
name a defined variant.
