#!/usr/bin/env bash
# RED/GREEN coverage for `guard-consumer-discovery-conservation.sh` (#3332).
#
# WHAT IS BEING PROVED
# The guard asserts that the publish-revision report's file-scan discovery and the production
# render name the SAME consumers. Its only harmful failure mode is exiting 0 while the two
# differ, or while it compared nothing, so every case but the first is a negative control: a
# fixture tree that diverges in one specific way (or that a static render cannot judge), which
# must be refused for THAT reason.
#
# The fixtures are written at run time into a temporary directory, never committed: the report
# scans every `*.yaml` in this repository, so a committed fixture consumer would become a
# consumer of the real report. Everything here renders with `kubectl kustomize`; no cluster,
# network or credential is touched.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GUARD="$REPO_ROOT/scripts/guard-consumer-discovery-conservation.sh"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ── The agreeing base fixture ──────────────────────────────────────────────────────────
# Mirrors the real shape: the prod overlay renders only a Flux Kustomization naming a
# provider path, which composes per-consumer bases; each consumer also carries a nested Flux
# Kustomization on its OWN source (as the real apps do), and an infrastructure layer holds a
# ResourceGraphDefinition whose OCIRepository template names a shared workflow, with no
# instance of its kind. None of those may trigger a refusal on their own.
BASE="$WORK/base"
mkdir -p "$BASE/k8s/clusters/prod" "$BASE/k8s/bases/apps/alpha" "$BASE/k8s/bases/apps/beta" \
  "$BASE/k8s/bases/infrastructure/tenant-rgd" "$BASE/k8s/providers/prod/apps" \
  "$BASE/k8s/providers/prod/infrastructure"

# KSail publishes this directory as the platform artifact and bootstraps its generated
# OCIRepository flux-system/flux-system. Root agreement alone cannot establish that source.
cat >"$BASE/ksail.prod.yaml" <<'YAML'
apiVersion: ksail.io/v1alpha1
kind: Cluster
spec:
  cluster:
    gitOpsEngine: Flux
    localRegistry:
      registry: ghcr.io/devantler-tech/platform/manifests
  workload:
    sourceDirectory: k8s
    kustomizationFile: clusters/prod
YAML

cat >"$BASE/k8s/clusters/prod/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - flux-kustomizations.yaml
YAML
cat >"$BASE/k8s/clusters/prod/flux-kustomizations.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 60m
  path: providers/prod/apps
  prune: true
  sourceRef:
    kind: OCIRepository
    name: flux-system
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: infrastructure
  namespace: flux-system
spec:
  interval: 60m
  path: ./providers/prod/infrastructure
  prune: true
  sourceRef:
    kind: OCIRepository
    name: flux-system
YAML

cat >"$BASE/k8s/bases/apps/alpha/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - oci-repository.yaml
  - flux-kustomization.yaml
YAML
cat >"$BASE/k8s/bases/apps/alpha/oci-repository.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: alpha
  namespace: alpha
spec:
  interval: 1m
  url: oci://ghcr.io/devantler-tech/alpha/manifests
  ref:
    semver: ">=1.0.0"
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}$'
YAML
cat >"$BASE/k8s/bases/apps/alpha/flux-kustomization.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: alpha
  namespace: alpha
spec:
  interval: 1m
  path: .
  prune: true
  sourceRef:
    kind: OCIRepository
    name: alpha
YAML

cat >"$BASE/k8s/bases/apps/beta/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - oci-repository.yaml
YAML
cat >"$BASE/k8s/bases/apps/beta/oci-repository.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: beta
  namespace: beta
spec:
  interval: 1m
  url: oci://ghcr.io/devantler-tech/beta/manifests
  ref:
    tag: 1.2.3
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@[0-9a-f]{40}$'
YAML

cat >"$BASE/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
YAML

cat >"$BASE/k8s/bases/infrastructure/tenant-rgd/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - resource-graph-definition.yaml
YAML
cat >"$BASE/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: tenant
spec:
  schema:
    apiVersion: v1alpha1
    kind: Tenant
    spec:
      name: string
  resources:
    - id: ociRepository
      template:
        apiVersion: source.toolkit.fluxcd.io/v1
        kind: OCIRepository
        metadata:
          name: ${schema.spec.name}
        spec:
          ref:
            semver: ">=1.0.0"
          url: oci://ghcr.io/devantler-tech/${schema.spec.name}/manifests
          verify:
            provider: cosign
            matchOIDCIdentity:
              - issuer: '^https://token\.actions\.githubusercontent\.com$'
                subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}$'
YAML
cat >"$BASE/k8s/providers/prod/infrastructure/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/infrastructure/tenant-rgd
YAML

# fixture <name> → a fresh copy of the agreeing base, for one case to mutate. Each case's
# output goes to `<fixture>.out`, BESIDE the tree rather than inside it (the scan would read
# it) and never named after the label, which can carry a `/`.
fixture() {
  cp -R "$BASE" "$WORK/$1"
  printf '%s\n' "$WORK/$1"
}

# expect_pass <label> <root> <text the success line must carry>
expect_pass() {
  local label="$1" root="$2" want="$3" out="$2.out" rc=0
  "$GUARD" "$root" >"$out" 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    fail "$label: exited $rc on an agreeing tree: $(cat "$out")"
  elif ! grep -qF -- "$want" "$out"; then
    fail "$label: exited 0 without the expected success line '$want': $(cat "$out")"
  else
    pass "$label"
  fi
}

# expect_refusal <label> <root> <text>... — must exit non-zero AND name every <text>, so a
# refusal for an unrelated reason (a typo in the fixture, a missing tool) cannot pass.
expect_refusal() {
  local label="$1" root="$2" out="$2.out" rc=0 want missing=''
  shift 2
  "$GUARD" "$root" >"$out" 2>&1 || rc=$?
  if [ "$rc" -eq 0 ]; then
    fail "$label: exited 0, so the case was NOT detected: $(cat "$out")"
    return 0
  fi
  for want in "$@"; do
    grep -qF -- "$want" "$out" || missing="$missing '$want'"
  done
  if [ -n "$missing" ]; then
    fail "$label: refused, but not for the expected reason (missing:$missing): $(cat "$out")"
  else
    pass "$label"
  fi
}

# Exact-current-head review regressions: admission, controller carriers and tag filters.
regression_latest_findings() {
  local root placement target field boundary want
  for field in url ref verify; do
    root="$(fixture "admission-$field")"
    cat >"$root/k8s/providers/prod/apps/mutation.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: consumer-mutation
spec:
  rules:
    - name: rewrite-consumer
      match:
        any:
          - resources:
              kinds: [OCIRepository]
      mutate:
        patchStrategicMerge:
          spec:
            url: oci://ghcr.io/devantler-tech/changed/manifests
YAML
    case "$field" in
      ref) yq -i 'del(.spec.rules[0].mutate.patchStrategicMerge.spec.url) | .spec.rules[0].mutate.patchStrategicMerge.spec.ref.tag = "old"' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
      verify) yq -i 'del(.spec.rules[0].mutate.patchStrategicMerge.spec.url) | .spec.rules[0].mutate.patchStrategicMerge.spec.verify.provider = "cosign"' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
    esac
    printf '  - mutation.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "admission $field changes cannot attest consumer identity" "$root" 'admission mutation' 'consumers are UNKNOWN'
  done
  for target in wildcard qualified missing mutate-existing cel; do
    root="$(fixture "admission-$target")"
    cp "$WORK/admission-url/k8s/providers/prod/apps/mutation.yaml" "$root/k8s/providers/prod/apps/mutation.yaml"
    case "$target" in
      wildcard) yq -i '.spec.rules[0].match.any[0].resources.kinds = ["*"]' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
      qualified) yq -i '.spec.rules[0].match.any[0].resources.kinds = ["source.toolkit.fluxcd.io/v1/OCIRepository"]' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
      missing) yq -i 'del(.spec.rules[0].match.any[0].resources.kinds)' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
      mutate-existing) yq -i '.spec.rules[0].match.any[0].resources.kinds = ["ConfigMap"] | .spec.rules[0].mutate.targets = [{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"OCIRepository"}]' "$root/k8s/providers/prod/apps/mutation.yaml" ;;
      cel) cat >"$root/k8s/providers/prod/apps/mutation.yaml" <<'YAML'
apiVersion: policies.kyverno.io/v1
kind: MutatingPolicy
metadata:
  name: consumer-mutation
spec:
  matchConstraints:
    resourceRules:
      - apiGroups: [source.toolkit.fluxcd.io]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [ocirepositories]
  mutations:
    - patchType: ApplyConfiguration
      applyConfiguration:
        expression: 'Object{spec: Object.spec{url: "oci://ghcr.io/devantler-tech/changed/manifests"}}'
YAML
        ;;
    esac
    printf '  - mutation.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "$target admission policy cannot attest consumer identity" "$root" 'admission mutation' 'consumers are UNKNOWN'
  done
  root="$(fixture unrelated-admission)"
  cp "$WORK/admission-url/k8s/providers/prod/apps/mutation.yaml" "$root/k8s/providers/prod/apps/mutation.yaml"
  yq -i '.spec.rules[0].match.any[0].resources.kinds = ["Pod"]' "$root/k8s/providers/prod/apps/mutation.yaml"
  printf '  - mutation.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a literal Pod-only mutation does not alter consumers' "$root" '2 consumer(s)'

  for placement in resources steps; do
    root="$(fixture "nested-kustomization-$placement")"
    cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: additional-roots
  namespace: flux-system
spec:
  resources:
    - apiVersion: kustomize.toolkit.fluxcd.io/v1
      kind: Kustomization
      metadata:
        name: hidden-layer
        namespace: flux-system
      spec:
        sourceRef:
          kind: OCIRepository
          name: flux-system
        path: bases/unseen
YAML
    mkdir -p "$root/k8s/bases/unseen"
    cp "$root/k8s/bases/apps/alpha/oci-repository.yaml" "$root/k8s/bases/unseen/hidden.yml"
    yq -i '.metadata.name = "hidden" | .spec.url = "oci://ghcr.io/devantler-tech/hidden/manifests"' "$root/k8s/bases/unseen/hidden.yml"
    printf 'resources:\n  - hidden.yml\n' >"$root/k8s/bases/unseen/kustomization.yaml"
    if [ "$placement" = steps ]; then
      yq -i '.spec.steps = [{"name":"extra", "resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/resource-set.yaml"
    fi
    printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "a ResourceSet $placement nested platform Kustomization cannot hide a layer" "$root" 'Kustomization template' 'consumers are UNKNOWN'
  done
  root="$(fixture nested-tenant-kustomization)"
  cp "$WORK/nested-kustomization-resources/k8s/providers/prod/apps/resource-set.yaml" "$root/k8s/providers/prod/apps/resource-set.yaml"
  yq -i '.spec.resources[0].spec.sourceRef.name = "tenant"' "$root/k8s/providers/prod/apps/resource-set.yaml"
  printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a nested tenant-source Kustomization stays outside the platform artifact' "$root" '2 consumer(s)'

  root="$(fixture semver-filter-source)"
  yq -i '.spec.ref.semverFilter = "^v.*-stable$"' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
  expect_refusal 'equal filtered source rows cannot attribute an unfiltered release' "$root" 'semverFilter' 'UNKNOWN'
  root="$(fixture semver-filter-overlay)"
  cat >>"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
patches:
  - target:
      kind: OCIRepository
      name: alpha
    patch: |-
      - op: add
        path: /spec/ref/semverFilter
        value: ^v.*-stable$
YAML
  expect_refusal 'an overlay tag filter cannot preserve the old consumer identity' "$root" 'semverFilter' 'UNKNOWN'

  for target in foreign unsigned; do
    root="$(fixture "unrelated-filter-$target")"
    cp "$root/k8s/bases/apps/alpha/oci-repository.yaml" "$root/k8s/providers/prod/apps/unrelated.yaml"
    yq -i '.metadata.name = "unrelated" | .spec.ref.semverFilter = "^v.*-stable$"' "$root/k8s/providers/prod/apps/unrelated.yaml"
    if [ "$target" = foreign ]; then
      yq -i '.spec.url = "oci://registry.example/vendor/chart"' "$root/k8s/providers/prod/apps/unrelated.yaml"
    else
      yq -i 'del(.spec.verify)' "$root/k8s/providers/prod/apps/unrelated.yaml"
    fi
    printf '  - unrelated.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_pass "an unrelated $target filtered artifact stays outside the signing report" "$root" '2 consumer(s)'
  done

  for boundary in admission cel nested filter; do
    root="$(fixture "partial-latest-$boundary")"
    mkdir "$WORK/partial-latest-$boundary-bin"
    cat >"$WORK/partial-latest-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_LATEST:$*" in
  admission:*'select(.mutates == true)'*) exit 2 ;;
  cel:*'.kind == "MutatingAdmissionPolicy"'*) exit 2 ;;
  nested:*'and (path | length) > 0'*) exit 2 ;;
  filter:*'has("semverFilter")'*) exit 2 ;;
esac
SH
    chmod +x "$WORK/partial-latest-$boundary-bin/yq"
    case "$boundary" in
      admission) want='could not bound admission mutations' ;;
      cel) want='could not read CEL admission mutations' ;;
      nested) want='could not read nested Kustomization templates' ;;
      filter) want='could not parse' ;;
    esac
    REAL_YQ="$(command -v yq)" PARTIAL_LATEST="$boundary" PATH="$WORK/partial-latest-$boundary-bin:$PATH" \
      expect_refusal "a partial $boundary reader is not an attestation" "$root" "$want" 'UNKNOWN'
  done
}

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = latest ]; then
  regression_latest_findings
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

# ---------------------------------------------------------------------------
# 1. AGREEING: every consumer discovered is rendered, field for field. The nested
#    Flux Kustomization on the consumer's own source and the instance-less RGD template
#    must NOT be refused.
# ---------------------------------------------------------------------------
expect_pass 'an agreeing tree passes and counts both consumers' "$BASE" '2 consumer(s) found by the file scan match the production render exactly'

# ---------------------------------------------------------------------------
# 2. EXCLUDED BASE: the base still exists, so the file scan finds it, but production no
#    longer composes it.
# ---------------------------------------------------------------------------
root="$(fixture excluded)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
YAML
expect_refusal 'a base production excludes is reported as discovered but not rendered' "$root" \
  'Discovered by the file scan but NOT rendered by production' 'artifact=beta/manifests'

# ---------------------------------------------------------------------------
# 3. PATCHED REF: an overlay pins a different tag than the base the scan reads.
# ---------------------------------------------------------------------------
root="$(fixture patched-ref)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
patches:
  - target:
      kind: OCIRepository
      name: beta
    patch: |-
      - op: replace
        path: /spec/ref/tag
        value: 9.9.9
YAML
expect_refusal 'a ref an overlay patches is reported on both sides' "$root" \
  'artifact=beta/manifests repo=beta workflow=publish-manifests ref=1.2.3' \
  'artifact=beta/manifests repo=beta workflow=publish-manifests ref=9.9.9'

# A digest added beside the tag changes the EFFECTIVE ref (digest outranks tag in Flux).
root="$(fixture patched-digest)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
patches:
  - target:
      kind: OCIRepository
      name: beta
    patch: |-
      - op: add
        path: /spec/ref/digest
        value: sha256:0000000000000000000000000000000000000000000000000000000000000000
YAML
expect_refusal 'a digest an overlay adds changes the effective ref' "$root" \
  'ref=digest:sha256:0000000000000000000000000000000000000000000000000000000000000000'

# ---------------------------------------------------------------------------
# 4. PATCHED URL: an overlay points the consumer at a different artifact.
# ---------------------------------------------------------------------------
root="$(fixture patched-url)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
patches:
  - target:
      kind: OCIRepository
      name: alpha
    patch: |-
      - op: replace
        path: /spec/url
        value: oci://ghcr.io/devantler-tech/alpha/zone-manifests
YAML
expect_refusal 'a URL an overlay patches is reported on both sides' "$root" \
  'artifact=alpha/manifests' 'artifact=alpha/zone-manifests'

# ---------------------------------------------------------------------------
# 5. ADDED CONSUMERS the file scan cannot see.
# ---------------------------------------------------------------------------
# (a) A resource file kustomize loads but the scan's `*.yaml` filter skips.
root="$(fixture added-yml)"
cat >"$root/k8s/providers/prod/apps/gamma.yml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: gamma
  namespace: gamma
spec:
  url: oci://ghcr.io/devantler-tech/gamma/manifests
  ref:
    semver: ">=1.0.0"
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}$'
YAML
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
  - gamma.yml
YAML
expect_refusal 'a consumer in a .yml file is reported as rendered but not discovered' "$root" \
  'Rendered by production but NOT discovered by the file scan' 'artifact=gamma/manifests'

# (b) A subject that only a patch supplies: the base document carries none, so the scan
#     never attributes it, while production verifies it against a shared workflow.
root="$(fixture added-by-patch)"
mkdir -p "$root/k8s/bases/apps/delta"
cat >"$root/k8s/bases/apps/delta/oci-repository.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: delta
  namespace: delta
spec:
  url: oci://ghcr.io/devantler-tech/delta/manifests
  ref:
    tag: 2.0.0
YAML
cat >"$root/k8s/bases/apps/delta/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - oci-repository.yaml
YAML
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
  - ../../../bases/apps/delta
patches:
  - target:
      kind: OCIRepository
      name: delta
    patch: |-
      - op: add
        path: /spec/verify
        value:
          provider: cosign
          matchOIDCIdentity:
            - issuer: '^https://token\.actions\.githubusercontent\.com$'
              subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@[0-9a-f]{40}$'
YAML
expect_refusal 'a consumer whose subject a patch adds is reported as rendered but not discovered' "$root" \
  'artifact=delta/manifests repo=delta workflow=publish-manifests ref=2.0.0'

# (c) A consumer living OUTSIDE every production root is discovered but not deployed.
root="$(fixture stray)"
mkdir -p "$root/docs"
cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/docs/example.yaml"
sed -i.bak 's#devantler-tech/beta/manifests#devantler-tech/epsilon/manifests#' "$root/docs/example.yaml"
rm -f "$root/docs/example.yaml.bak"
expect_refusal 'a consumer outside every production root is reported as discovered but not rendered' "$root" \
  'artifact=epsilon/manifests'

# ---------------------------------------------------------------------------
# 6. VACUITY: nothing on either side is not agreement.
# ---------------------------------------------------------------------------
root="$(fixture empty)"
rm -rf "$root/k8s/bases/apps"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
YAML
cat >"$root/k8s/providers/prod/apps/namespace.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: empty
YAML
expect_refusal 'an empty consumer set on both sides is refused' "$root" 'compares nothing'

# ---------------------------------------------------------------------------
# 7. UNKNOWN ROOTS fail closed.
# ---------------------------------------------------------------------------
root="$(fixture broken-root)"
printf '  - ../../../bases/apps/missing\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'a root that does not render is refused' "$root" \
  'could not render production root providers/prod/apps'

root="$(fixture missing-root)"
sed -i.bak 's#path: providers/prod/apps#path: providers/prod/moved#' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
rm -f "$root/k8s/clusters/prod/flux-kustomizations.yaml.bak"
expect_refusal 'a root that does not exist is refused' "$root" \
  'production root providers/prod/moved does not exist'

# `./` names the source root itself; stripping its prefix must not leave an empty root that
# is skipped as a blank line. This tree has no kustomization at k8s/, so it must be RENDERED
# (and refused), never silently dropped from the comparison.
root="$(fixture source-root)"
sed -i.bak 's#path: providers/prod/apps#path: ./#' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
rm -f "$root/k8s/clusters/prod/flux-kustomizations.yaml.bak"
expect_refusal 'a root of ./ is rendered rather than skipped' "$root" \
  'could not render production root .,'

# A file the scan selects (it carries a shared-workflow subject) but cannot parse. It sits
# outside every production root, so the render is unaffected and both sides still hold
# alpha and beta: dropping the file would make two partial sets agree.
root="$(fixture unparsable)"
mkdir -p "$root/docs"
cat >"$root/docs/broken.yaml" <<'YAML'
kind: OCIRepository
spec:
  url: oci://ghcr.io/devantler-tech/eta/manifests
  verify:
    matchOIDCIdentity:
      - subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}$'
  ref: [unclosed
YAML
expect_refusal 'a file the scan selects but cannot parse is refused' "$root" \
  'could not parse' 'docs/broken.yaml' 'the file scan could not read every file it selected'

root="$(fixture no-overlay)"
rm -rf "$root/k8s/clusters/prod"
expect_refusal 'a tree with no production overlay is refused' "$root" 'no production overlay'

# ---------------------------------------------------------------------------
# 8. WHAT A STATIC RENDER CANNOT SEE is refused rather than assumed equal.
# ---------------------------------------------------------------------------
# (a) A Flux-side patch on a production root rewrites the build after kustomize.
root="$(fixture flux-patches)"
cat >>"$root/k8s/clusters/prod/flux-kustomizations.yaml" <<'YAML'
  patches:
    - target:
        kind: OCIRepository
        name: beta
      patch: |-
        - op: replace
          path: /spec/ref/tag
          value: 9.9.9
YAML
expect_refusal 'a Flux-side patch on a production root is refused' "$root" \
  'production Flux Kustomization infrastructure carries spec.patches'

# (b) A root drawn from another source is relative to a tree this guard cannot render.
root="$(fixture two-sources)"
cat >>"$root/k8s/clusters/prod/flux-kustomizations.yaml" <<'YAML'
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: elsewhere
  namespace: flux-system
spec:
  interval: 60m
  path: providers/prod/apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: elsewhere
YAML
expect_refusal 'production roots from more than one source are refused' "$root" 'more than one source'

# (c) A nested Flux Kustomization on the platform's OWN source applies a path never rendered.
root="$(fixture nested-self)"
cat >"$root/k8s/providers/prod/apps/nested.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: more-apps
  namespace: flux-system
spec:
  interval: 60m
  path: providers/prod/more-apps
  prune: true
  sourceRef:
    kind: OCIRepository
    name: flux-system
YAML
printf '  - nested.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal "a nested root on the platform's own source is refused" "$root" \
  'renders Flux Kustomization more-apps, which applies another path'

# (d) A consumer field Flux substitution decides at apply time.
root="$(fixture substituted)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
patches:
  - target:
      kind: OCIRepository
      name: beta
    patch: |-
      - op: replace
        path: /spec/ref/tag
        value: ${BETA_VERSION}
YAML
expect_refusal 'a consumer ref decided by Flux substitution is refused' "$root" \
  'beta/beta whose URL, ref or subject is decided by Flux substitution'

# (e) An instance of the RGD whose template is a consumer: kro creates the OCIRepository
#     inside the cluster, so no document for it exists on either side.
root="$(fixture rgd-instance)"
cat >"$root/k8s/providers/prod/apps/tenant.yaml" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: zeta
spec:
  name: zeta
YAML
printf '  - tenant.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'an instance of a consumer-templating RGD is refused' "$root" \
  'production renders 1 Tenant instance(s)'

# (f) A v1beta2 Flux-side patch field: the same post-build rewrite under its older name.
root="$(fixture flux-v1beta2-patches)"
cat >>"$root/k8s/clusters/prod/flux-kustomizations.yaml" <<'YAML'
  patchesStrategicMerge:
    - apiVersion: source.toolkit.fluxcd.io/v1
      kind: OCIRepository
      metadata:
        name: beta
        namespace: beta
      spec:
        ref:
          tag: 9.9.9
YAML
expect_refusal 'a v1beta2 Flux-side patch on a production root is refused' "$root" \
  'production Flux Kustomization infrastructure carries' 'spec.patchesStrategicMerge'

# (g) An OCIRepository template inside a document that is not a kro RGD: the objects it
#     generates exist only in the cluster.
root="$(fixture resourceset)"
cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: flux-system
spec:
  resources:
    - apiVersion: source.toolkit.fluxcd.io/v1
      kind: OCIRepository
      metadata:
        name: theta
      spec:
        url: oci://ghcr.io/devantler-tech/theta/manifests
        ref:
          semver: ">=1.0.0"
YAML
printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'an OCIRepository template outside a kro RGD is refused' "$root" \
  'holds ResourceSet flux-system/tenants, which carries an OCIRepository template'

# (h) An RGD whose template subject is itself templated, with an instance: it names no
#     shared workflow literally, so a subject filter would have waved its instances through.
root="$(fixture rgd-templated-subject)"
# shellcheck disable=SC2016 # A literal kro expression: the shell must not expand it.
sed -i.bak 's#publish-app\\.yaml@\[0-9a-f\]{40}#${schema.spec.workflow}#' \
  "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
rm -f "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml.bak"
if grep -q 'publish-app' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"; then
  fail 'rgd-templated-subject: the subject was not templated, so this case proves nothing'
fi
cat >"$root/k8s/providers/prod/apps/tenant.yaml" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: iota
spec:
  name: iota
YAML
printf '  - tenant.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'an instance of an RGD with a templated subject is refused' "$root" \
  'production renders 1 Tenant instance(s)'

# (i) An RGD that templates an OCIRepository but names no schema kind: its instances
#     cannot be counted, so it cannot be judged.
root="$(fixture rgd-no-kind)"
sed -i.bak '/^    kind: Tenant$/d' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
rm -f "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml.bak"
expect_refusal 'an RGD templating an OCIRepository with no schema kind is refused' "$root" \
  'names no schema kind'

# ---------------------------------------------------------------------------
# 9. THE COMPARED IDENTITY includes the subject, and the overlay is a consumer source.
# ---------------------------------------------------------------------------
# (a) An overlay that widens a consumer's signer constraint: every other field is equal.
root="$(fixture patched-subject)"
cat >"$root/k8s/providers/prod/apps/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../bases/apps/alpha
  - ../../../bases/apps/beta
patches:
  - target:
      kind: OCIRepository
      name: beta
    patch: |-
      - op: replace
        path: /spec/verify/matchOIDCIdentity/0/subject
        value: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@.*$'
YAML
expect_refusal 'a subject an overlay patches is reported on both sides' "$root" \
  'workflow=publish-manifests ref=1.2.3 subject=^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@[0-9a-f]{40}$' \
  'workflow=publish-manifests ref=1.2.3 subject=^https://github\.com/devantler-tech/actions/\.github/workflows/publish-manifests\.yaml@.*$'

# (b) A consumer the overlay itself applies is rendered, not reported as missing.
root="$(fixture overlay-consumer)"
sed 's#devantler-tech/beta/manifests#devantler-tech/kappa/manifests#; s#name: beta#name: kappa#; s#namespace: beta#namespace: kappa#' \
  "$root/k8s/bases/apps/beta/oci-repository.yaml" >"$root/k8s/clusters/prod/kappa.yaml"
printf '  - kappa.yaml\n' >>"$root/k8s/clusters/prod/kustomization.yaml"
expect_pass "a consumer the overlay itself applies counts as rendered" "$root" \
  '3 consumer(s) found by the file scan match the production render exactly'

# (c) A consumer whose attribution is ambiguous (both shared workflows named) is UNKNOWN
#     on the side that cannot attribute it, never silently dropped from both.
root="$(fixture ambiguous)"
sed -i.bak 's#workflows/publish-manifests\\.yaml@#workflows/publish-app\\.yaml@x|publish-manifests\\.yaml@#' \
  "$root/k8s/bases/apps/beta/oci-repository.yaml"
rm -f "$root/k8s/bases/apps/beta/oci-repository.yaml.bak"
grep -q 'publish-app.*publish-manifests' "$root/k8s/bases/apps/beta/oci-repository.yaml" ||
  fail 'ambiguous: the subject does not name both workflows, so this case proves nothing'
expect_refusal 'an ambiguous consumer in production is refused, not dropped' "$root" \
  'ambiguous:' 'rendered set is UNKNOWN'

# ---------------------------------------------------------------------------
# 10. SOURCE ATTRIBUTION: agreeing roots must use this checkout's platform artifact.
# ---------------------------------------------------------------------------
for field in name kind namespace; do
  root="$(fixture "unrelated-source-$field")"
  case "$field" in
    name) yq -i '.spec.sourceRef.name = "unrelated-source"' "$root/k8s/clusters/prod/flux-kustomizations.yaml" ;;
    kind) yq -i '.spec.sourceRef.kind = "GitRepository"' "$root/k8s/clusters/prod/flux-kustomizations.yaml" ;;
    namespace) yq -i '.spec.sourceRef.namespace = "elsewhere"' "$root/k8s/clusters/prod/flux-kustomizations.yaml" ;;
  esac
  expect_refusal "agreeing roots on an unrelated source $field are refused" "$root" \
    'not the KSail-generated platform source' 'consumers are UNKNOWN'
done

root="$(fixture explicit-source-namespace)"
yq -i '.spec.sourceRef.namespace = "flux-system"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_pass 'an explicit canonical platform source namespace passes' "$root" '2 consumer(s)'

for field in registry sourceDirectory kustomizationFile; do
  root="$(fixture "different-ksail-$field")"
  case "$field" in
    registry) yq -i '.spec.cluster.localRegistry.registry = "ghcr.io/devantler-tech/another/manifests"' "$root/ksail.prod.yaml" ;;
    sourceDirectory) yq -i '.spec.workload.sourceDirectory = "elsewhere"' "$root/ksail.prod.yaml" ;;
    kustomizationFile) yq -i '.spec.workload.kustomizationFile = "clusters/another"' "$root/ksail.prod.yaml" ;;
  esac
  expect_refusal "a different production KSail $field is refused" "$root" \
    'KSail production artifact contract' 'UNKNOWN'
done

root="$(fixture missing-ksail)"
rm "$root/ksail.prod.yaml"
expect_refusal 'a missing production artifact declaration is refused' "$root" \
  'KSail production artifact contract' 'UNKNOWN'

root="$(fixture multiple-ksail)"
cat "$root/ksail.prod.yaml" >"$WORK/config-copy"
printf '\n---\n' >>"$root/ksail.prod.yaml"
cat "$WORK/config-copy" >>"$root/ksail.prod.yaml"
expect_refusal 'multiple production artifact declarations are refused' "$root" \
  'exactly one KSail Cluster' 'UNKNOWN'

root="$(fixture credentialed-registry)"
# shellcheck disable=SC2016 # KSail expands this credential later; it is not a shell value.
yq -i '.spec.cluster.localRegistry.registry = "devantler:${GHCR_TOKEN}@ghcr.io/devantler-tech/platform/manifests"' "$root/ksail.prod.yaml"
expect_pass 'an ordinary credential variable in the KSail registry is accepted' "$root" '2 consumer(s)'

# A reader can produce all expected rows and still fail. Its partial output is not proof.
real_yq="$(command -v yq)"
mkdir "$WORK/partial-yq-bin"
cat >"$WORK/partial-yq-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "${!#}" in */ksail.prod.yaml) exit 2 ;; esac
SH
chmod +x "$WORK/partial-yq-bin/yq"
root="$(fixture partial-artifact-reader)"
REAL_YQ="$real_yq" PATH="$WORK/partial-yq-bin:$PATH" expect_refusal \
  'partial artifact-reader output is refused' "$root" \
  'could not read the KSail production artifact contract' 'UNKNOWN'

# ---------------------------------------------------------------------------
# 11. FLUX POST-BUILD SUBSTITUTION can change kind before consumer discovery.
# ---------------------------------------------------------------------------
root="$(fixture substituted-consumer-kind)"
cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/apps/gamma.yaml"
# shellcheck disable=SC2016 # The literal Flux expression is supplied below.
yq -i '.kind = "${CONSUMER_KIND}" | .metadata.name = "gamma" | .spec.url = "oci://ghcr.io/devantler-tech/gamma/manifests"' \
  "$root/k8s/providers/prod/apps/gamma.yaml"
printf '  - gamma.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
yq -i '.spec.postBuild.substitute.CONSUMER_KIND = "OCIRepository"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a supplied Flux variable creating another OCIRepository is refused' "$root" \
  'kind is decided by substitution' 'consumers are UNKNOWN'

root="$(fixture substituted-root-kind)"
# shellcheck disable=SC2016 # Substitution is part of the fixture, never shell execution.
yq -i 'select(.metadata.name == "apps").kind = "${ROOT_KIND}"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a substituted root kind cannot hide one production layer' "$root" \
  'kind is decided by substitution' 'consumers are UNKNOWN'

root="$(fixture substituted-template-kind)"
# shellcheck disable=SC2016 # The carrier's type is not known before substitution.
yq -i '.spec.resources[0].template.kind = "${SOURCE_KIND}"' \
  "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
expect_refusal 'a substituted template kind cannot hide consumer-producing objects' "$root" \
  'kind is decided by substitution' 'consumers are UNKNOWN'

root="$(fixture ordinary-postbuild-variable)"
cat >"$root/k8s/providers/prod/apps/config-map.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ordinary
data:
  domain: ${DOMAIN}
YAML
printf '  - config-map.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
yq -i '.spec.postBuild.substitute.DOMAIN = "example.test"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_pass 'ordinary post-build data variables do not change consumer discovery' "$root" '2 consumer(s)'

# Even a canonical name can be overridden by declarations in the rendered artifact. The
# source generated at bootstrap and the source that the production layers keep must agree.
for target in platform another; do
  root="$(fixture "source-object-$target")"
  cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: oci://ghcr.io/devantler-tech/$target/manifests
YAML
  printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  if [ "$target" = platform ]; then
    expect_pass 'an explicit source declaration for the same platform artifact passes' "$root" '2 consumer(s)'
  else
    expect_refusal 'a rendered source declaration cannot override the platform artifact' "$root" \
      'platform source override' 'consumers are UNKNOWN'
  fi
done

for target in platform another; do
  root="$(fixture "source-sync-$target")"
  cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<YAML
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  sync:
    kind: OCIRepository
    url: oci://ghcr.io/devantler-tech/$target/manifests
    ref: latest
    path: clusters/prod
YAML
  printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  if [ "$target" = platform ]; then
    expect_pass 'an explicit FluxInstance sync for the same platform artifact passes' "$root" '2 consumer(s)'
  else
    expect_refusal 'FluxInstance sync cannot override the platform artifact' "$root" \
      'platform source override' 'consumers are UNKNOWN'
  fi
done

root="$(fixture source-url-patch)"
cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |-
          - op: replace
            path: /spec/url
            value: oci://ghcr.io/devantler-tech/another/manifests
YAML
printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
expect_refusal 'a FluxInstance patch cannot override the platform source URL' "$root" \
  'platform source override' 'consumers are UNKNOWN'

root="$(fixture source-preserving-patch)"
cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |-
          - op: add
            path: /spec/verify
            value:
              provider: cosign
          - op: replace
            path: /spec/ref/tag
            value: latest
YAML
printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
expect_pass 'verification and ref patches preserve platform source attribution' "$root" '2 consumer(s)'

root="$(fixture partial-kind-reader)"
mkdir "$WORK/partial-kind-bin"
cat >"$WORK/partial-kind-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$*" in *'has("apiVersion")'*) exit 2 ;; esac
SH
chmod +x "$WORK/partial-kind-bin/yq"
REAL_YQ="$real_yq" PATH="$WORK/partial-kind-bin:$PATH" expect_refusal \
  'partial kind-reader output cannot clear consumer discovery' "$root" \
  'could not read object kinds' 'consumers are UNKNOWN'

root="$(fixture partial-source-reader)"
mkdir "$WORK/partial-source-bin"
cat >"$WORK/partial-source-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$*" in *'.spec.url == "oci://ghcr.io/devantler-tech/platform/manifests"'*) exit 2 ;; esac
SH
chmod +x "$WORK/partial-source-bin/yq"
REAL_YQ="$real_yq" PATH="$WORK/partial-source-bin:$PATH" expect_refusal \
  'partial source-reader output cannot attest the production artifact' "$root" \
  'could not read platform source declarations' 'consumers are UNKNOWN'

root="$(fixture nested-source-kind-substitution)"
# shellcheck disable=SC2016 # Flux receives the supplied source-kind variable.
yq -i '.spec.sourceRef.kind = "${SOURCE_KIND}" | .spec.sourceRef.name = "flux-system" | .spec.sourceRef.namespace = "flux-system"' \
  "$root/k8s/bases/apps/alpha/flux-kustomization.yaml"
yq -i '.spec.postBuild.substitute.SOURCE_KIND = "OCIRepository"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a nested source kind cannot hide an unrendered platform layer' "$root" \
  'source is decided by Flux substitution' 'consumers are UNKNOWN'

# A root's targetNamespace rewrites the source identity after the local build. An
# apparently unrelated OCIRepository can therefore replace the platform source.
root="$(fixture target-namespace-source-override)"
yq -i 'select(.metadata.name == "apps").spec.targetNamespace = "flux-system"' \
  "$root/k8s/clusters/prod/flux-kustomizations.yaml"
cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: elsewhere
spec:
  url: oci://ghcr.io/devantler-tech/another/manifests
YAML
printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'targetNamespace cannot hide a platform source override' "$root" \
  'production Flux Kustomization apps carries' 'spec.targetNamespace'

root="$(fixture empty-target-namespace)"
yq -i '.spec.targetNamespace = ""' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_pass 'an empty targetNamespace makes no unseen namespace transform' "$root" '2 consumer(s)'

# Post-build substitutions can give a top-level source the generated source's identity.
# This applies even when it has no shared-workflow subject and contributes no consumer row.
for field in name namespace; do
  root="$(fixture "substituted-platform-source-$field")"
  cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: oci://ghcr.io/devantler-tech/another/manifests
YAML
  # shellcheck disable=SC2016 # The fixture supplies Flux's literal substitution value.
  FIELD="$field" yq -i '.metadata[strenv(FIELD)] = "${PLATFORM_SOURCE_IDENTITY}"' \
    "$root/k8s/providers/prod/apps/platform-source.yaml"
  printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  yq -i '.spec.postBuild.substitute.PLATFORM_SOURCE_IDENTITY = "flux-system"' \
    "$root/k8s/clusters/prod/flux-kustomizations.yaml"
  expect_refusal "a substituted platform source $field is refused before attribution" "$root" \
    'source identity is decided by Flux substitution' 'consumers are UNKNOWN'
done

# The same artifact URL is insufficient if Flux selects a different layer or excludes
# files. Explicit empty/null selection fields are also not part of the generated contract.
for selection in ignore layer-selector empty-ignore null-ignore empty-layer-selector null-layer-selector; do
  root="$(fixture "source-content-$selection")"
  cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: oci://ghcr.io/devantler-tech/platform/manifests
YAML
  case "$selection" in
    ignore) yq -i '.spec.ignore = "/*\n!/providers/prod/apps"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    layer-selector) yq -i '.spec.layerSelector = {"mediaType": "application/vnd.example.other.layer.v1.tar+gzip", "operation": "copy"}' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    empty-ignore) yq -i '.spec.ignore = ""' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    null-ignore) yq -i '.spec.ignore = null' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    empty-layer-selector) yq -i '.spec.layerSelector = {}' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    null-layer-selector) yq -i '.spec.layerSelector = null' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
  esac
  printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal "a platform source with $selection cannot attest the checkout contents" "$root" \
    'platform source content selection' 'consumers are UNKNOWN'
done

# An API version chosen after discovery can hide a nested platform-source root, just
# like a substituted kind. Its actual consumer uses .yml, so the raw scan misses it too.
root="$(fixture substituted-nested-api-version)"
mkdir -p "$root/k8s/providers/prod/hidden"
cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/hidden/hidden.yml"
yq -i '.metadata.name = "hidden" | .spec.url = "oci://ghcr.io/devantler-tech/hidden/manifests"' \
  "$root/k8s/providers/prod/hidden/hidden.yml"
printf 'resources:\n  - hidden.yml\n' >"$root/k8s/providers/prod/hidden/kustomization.yaml"
cat >"$root/k8s/providers/prod/apps/nested.yml" <<'YAML'
apiVersion: ${NESTED_API_VERSION}
kind: Kustomization
metadata:
  name: hidden
  namespace: flux-system
spec:
  interval: 1m
  path: providers/prod/hidden
  prune: true
  sourceRef:
    kind: OCIRepository
    name: flux-system
YAML
printf '  - nested.yml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
yq -i '.spec.postBuild.substitute.NESTED_API_VERSION = "kustomize.toolkit.fluxcd.io/v1"' \
  "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a substituted API version cannot hide a nested platform-source root' "$root" \
  'apiVersion is decided by substitution' 'consumers are UNKNOWN'

root="$(fixture substituted-consumer-api-version)"
# shellcheck disable=SC2016 # Flux supplies this API version after discovery.
yq -i '.apiVersion = "${SOURCE_API_VERSION}"' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
yq -i '.spec.postBuild.substitute.SOURCE_API_VERSION = "source.toolkit.fluxcd.io/v1"' \
  "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a substituted consumer API version is refused before classification' "$root" \
  'apiVersion is decided by substitution' 'consumers are UNKNOWN'

root="$(fixture substituted-template-api-version)"
# shellcheck disable=SC2016 # Object-template type must also remain literal.
yq -i '.spec.resources[0].template.apiVersion = "${SOURCE_API_VERSION}"' \
  "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
expect_refusal 'a substituted template API version cannot hide its object type' "$root" \
  'apiVersion is decided by substitution' 'consumers are UNKNOWN'

# A substituted instance identity can hide the generated source's owning declaration too.
for field in name namespace; do
  root="$(fixture "substituted-platform-instance-$field")"
  cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  sync:
    kind: OCIRepository
    url: oci://ghcr.io/devantler-tech/another/manifests
    ref: latest
    path: clusters/prod
YAML
  # shellcheck disable=SC2016 # Flux decides this owning identity after discovery.
  FIELD="$field" yq -i '.metadata[strenv(FIELD)] = "${PLATFORM_INSTANCE_IDENTITY}"' \
    "$root/k8s/providers/prod/infrastructure/flux-instance.yaml"
  printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal "a substituted platform instance $field cannot hide the source owner" "$root" \
    'source identity is decided by Flux substitution' 'consumers are UNKNOWN'
done

root="$(fixture tenant-source-content-selector)"
yq -i '.spec.layerSelector = {"mediaType": "application/vnd.example.tenant.layer.v1.tar+gzip"} |
  .spec.ignore = "/*.md"' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
expect_pass 'content selection on a tenant source does not override the platform source' "$root" '2 consumer(s)'

# A namespace omitted from static output is not proof that this source cannot become
# flux-system/flux-system. Explicit empty namespace is equally ambiguous.
for namespace in omitted empty; do
  root="$(fixture "platform-source-namespace-$namespace")"
  cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
spec:
  url: oci://ghcr.io/devantler-tech/another/manifests
YAML
  if [ "$namespace" = empty ]; then
    yq -i '.metadata.namespace = ""' "$root/k8s/providers/prod/apps/platform-source.yaml"
  fi
  printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal "a platform source override with $namespace namespace is refused" "$root" \
    'platform source override' 'consumers are UNKNOWN'
  yq -i '.spec.url = "oci://ghcr.io/devantler-tech/platform/manifests" | .spec.ignore = "/*"' \
    "$root/k8s/providers/prod/apps/platform-source.yaml"
  expect_refusal "platform content selection with $namespace namespace is refused" "$root" \
    'platform source content selection' 'consumers are UNKNOWN'
done

root="$(fixture tenant-source-same-name)"
cat >"$root/k8s/providers/prod/apps/tenant-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: tenant
spec:
  url: oci://ghcr.io/devantler-tech/tenant/manifests
  ignore: '/*.md'
YAML
printf '  - tenant-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_pass 'a source explicitly in another tenant namespace keeps its own content contract' "$root" '2 consumer(s)'

# New attestation readers must reject a failed parse even after all its output was emitted.
for boundary in api-version identity content; do
  root="$(fixture "partial-$boundary-reader")"
  mkdir "$WORK/partial-$boundary-bin"
  cat >"$WORK/partial-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_BOUNDARY:$*" in
  api-version:*'(.apiVersion // "" | tostring)'*) exit 2 ;;
  identity:*'select(.kind == "OCIRepository" or .kind == "FluxInstance")'*) exit 2 ;;
  content:*'has("layerSelector")'*) exit 2 ;;
esac
SH
  chmod +x "$WORK/partial-$boundary-bin/yq"
  case "$boundary" in
    api-version) want='could not read object API versions' ;;
    identity) want='could not read source identities' ;;
    content) want='could not read platform source content selection' ;;
  esac
  REAL_YQ="$real_yq" PARTIAL_BOUNDARY="$boundary" PATH="$WORK/partial-$boundary-bin:$PATH" \
    expect_refusal "partial $boundary-reader output is not an attestation" "$root" \
    "$want" 'consumers are UNKNOWN'
done

# A canonical URL must still select the checkout's generated latest artifact.
platform_source() {
  local root="$1"
  cat >"$root/k8s/providers/prod/apps/platform-source.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: oci://ghcr.io/devantler-tech/platform/manifests
YAML
  printf '  - platform-source.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
}
for ref in tag semver digest latest-digest latest-semver; do
  root="$(fixture "platform-ref-$ref")"
  platform_source "$root"
  case "$ref" in
    tag) yq -i '.spec.ref.tag = "old"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    semver) yq -i '.spec.ref.semver = ">=1.0.0"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    digest) yq -i '.spec.ref.digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    latest-digest) yq -i '.spec.ref.tag = "latest" | .spec.ref.digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
    latest-semver) yq -i '.spec.ref.tag = "latest" | .spec.ref.semver = ">=1.0.0"' "$root/k8s/providers/prod/apps/platform-source.yaml" ;;
  esac
  expect_refusal "a canonical platform URL with $ref cannot attest latest" "$root" \
    'platform source reference' 'latest' 'consumers are UNKNOWN'
done
root="$(fixture platform-ref-latest)"
platform_source "$root"
yq -i '.spec.ref.tag = "latest"' "$root/k8s/providers/prod/apps/platform-source.yaml"
expect_pass 'an explicit latest platform source reference passes' "$root" '2 consumer(s)'

for ref in old digest missing; do
  root="$(fixture "platform-sync-ref-$ref")"
  cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  sync:
    kind: OCIRepository
    url: oci://ghcr.io/devantler-tech/platform/manifests
    ref: latest
    path: clusters/prod
YAML
  case "$ref" in
    old) yq -i '.spec.sync.ref = "old"' "$root/k8s/providers/prod/infrastructure/flux-instance.yaml" ;;
    digest) yq -i '.spec.sync.ref = "sha256:0000000000000000000000000000000000000000000000000000000000000000"' "$root/k8s/providers/prod/infrastructure/flux-instance.yaml" ;;
    missing) yq -i 'del(.spec.sync.ref)' "$root/k8s/providers/prod/infrastructure/flux-instance.yaml" ;;
  esac
  printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal "FluxInstance $ref sync reference cannot attest latest" "$root" \
    'platform source override' 'consumers are UNKNOWN'
done

for ref in tag digest whole-ref; do
  root="$(fixture "platform-patch-ref-$ref")"
  cat >"$root/k8s/providers/prod/infrastructure/flux-instance.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: flux-system
spec:
  kustomize:
    patches:
      - target:
          kind: OCIRepository
          name: flux-system
          namespace: flux-system
        patch: |
          - op: add
            path: /spec/ref/tag
            value: old
YAML
  case "$ref" in
    digest) yq -i '.spec.kustomize.patches[0].patch = "- op: add\n  path: /spec/ref/digest\n  value: sha256:0000000000000000000000000000000000000000000000000000000000000000\n"' "$root/k8s/providers/prod/infrastructure/flux-instance.yaml" ;;
    whole-ref) yq -i '.spec.kustomize.patches[0].patch = "- op: replace\n  path: /spec/ref\n  value:\n    semver: \">=1.0.0\"\n"' "$root/k8s/providers/prod/infrastructure/flux-instance.yaml" ;;
  esac
  printf '  - flux-instance.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal "a FluxInstance $ref patch cannot change latest selection" "$root" \
    'platform source override' 'consumers are UNKNOWN'
done

root="$(fixture suspended-root)"
yq -i 'select(.metadata.name == "apps").spec.suspend = true' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_refusal 'a suspended production root cannot attest current deployment' "$root" 'is suspended' 'UNKNOWN'
root="$(fixture active-root)"
yq -i '.spec.suspend = false' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_pass 'explicitly active production roots pass' "$root" '2 consumer(s)'

# Make the outside root render exactly the same consumers: disagreement cannot catch it.
for escape in outside prefix-peer symlink; do
  root="$(fixture "escaped-root-$escape")"
  outside="$root/outside"
  [ "$escape" != prefix-peer ] || outside="$root/k8s-peer/apps"
  mkdir -p "$outside"
  relative_k8s='../k8s'
  [ "$escape" != prefix-peer ] || relative_k8s='../../k8s'
  cat >"$outside/kustomization.yaml" <<YAML
resources:
  - $relative_k8s/bases/apps/alpha
  - $relative_k8s/bases/apps/beta
YAML
  case "$escape" in
    outside) escaped_path='../outside' ;;
    prefix-peer) escaped_path='../k8s-peer/apps' ;;
    symlink)
      ln -s "$outside" "$root/k8s/providers/prod/escape"
      escaped_path='providers/prod/escape'
      ;;
  esac
  ROOT_PATH="$escaped_path" yq -i 'select(.metadata.name == "apps").spec.path = strenv(ROOT_PATH)' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
  expect_refusal "a $escape root outside the published tree is refused" "$root" 'outside the published k8s tree' 'UNKNOWN'
done
root="$(fixture canonical-inside-root)"
yq -i 'select(.metadata.name == "apps").spec.path = "./providers/prod/apps/../apps"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
expect_pass 'a canonicalized root that stays inside the published tree passes' "$root" '2 consumer(s)'

for placement in top-level step; do
  root="$(fixture "resourceset-string-$placement")"
  cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
  namespace: flux-system
spec:
  resourcesTemplate: |
    apiVersion: source.toolkit.fluxcd.io/v1
    kind: OCIRepository
    metadata:
      name: generated
    spec:
      url: oci://ghcr.io/devantler-tech/generated/manifests
YAML
  if [ "$placement" = step ]; then
    yq -i '.spec.steps = [{"name": "sources", "resourcesTemplate": .spec.resourcesTemplate}] | del(.spec.resourcesTemplate)' \
      "$root/k8s/providers/prod/apps/resource-set.yaml"
  fi
  printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal "ResourceSet $placement string templates are not silently omitted" "$root" 'resourcesTemplate' 'consumers are UNKNOWN'
done

for peer in another-group another-version custom-schema-group; do
  root="$(fixture "rgd-peer-$peer")"
  peer_api='unrelated.example.test/v1alpha1'
  case "$peer" in
    another-version) peer_api='kro.run/v1beta1' ;;
    custom-schema-group)
      yq -i '.spec.schema.group = "tenants.example.test"' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
      peer_api='kro.run/v1alpha1'
      ;;
  esac
  cat >"$root/k8s/providers/prod/apps/peer.yaml" <<YAML
apiVersion: $peer_api
kind: Tenant
metadata:
  name: unrelated
spec:
  name: unrelated
YAML
  printf '  - peer.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass "an unrelated same-kind $peer peer is not a kro instance" "$root" '2 consumer(s)'
done
root="$(fixture rgd-custom-group-instance)"
yq -i '.spec.schema.group = "tenants.example.test"' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
cat >"$root/k8s/providers/prod/apps/tenant.yaml" <<'YAML'
apiVersion: tenants.example.test/v1alpha1
kind: Tenant
metadata:
  name: generated
spec:
  name: generated
YAML
printf '  - tenant.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
expect_refusal 'a matching custom-group kro instance is still refused' "$root" \
  'production renders 1 Tenant instance(s)' 'tenants.example.test/v1alpha1'

regression_latest_findings

printf '\n%d failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
