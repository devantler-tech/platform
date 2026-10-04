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

# Published artifacts contain k8s, so checkout-only dependencies cannot attest production.
regression_published_closure() {
  local root field placement reference boundary dependency_field
  for field in resources bases components; do
    for placement in direct transitive; do
      dependency_field="$field"
      root="$(fixture "closure-$field-$placement")"
      mkdir "$root/outside"
      cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/outside/oci-repository.yaml"
      yq -i '.metadata.name = "gamma" | .metadata.namespace = "gamma" | .spec.url = "oci://ghcr.io/devantler-tech/gamma/manifests"' "$root/outside/oci-repository.yaml"
      printf 'resources:\n  - oci-repository.yaml\n' >"$root/outside/kustomization.yaml"
      if [ "$field" = components ]; then
        printf 'apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n' >>"$root/outside/kustomization.yaml"
      fi
      reference='../../../../outside'
      if [ "$placement" = transitive ]; then
        mkdir "$root/k8s/bases/closure"
        printf '%s:\n  - ../../../outside\n' "$field" >"$root/k8s/bases/closure/kustomization.yaml"
        dependency_field=resources
        reference='../../../bases/closure'
      fi
      if [ "$dependency_field" = resources ]; then
        printf '  - %s\n' "$reference" >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      else
        printf '%s:\n  - %s\n' "$dependency_field" "$reference" >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      fi
      expect_refusal "a $placement $field directory escape is absent from the published artifact" "$root" 'published k8s' 'UNKNOWN'
    done
  done

  root="$(fixture closure-overlay)"
  mkdir "$root/outside"
  cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/outside/oci-repository.yaml"
  yq -i '.metadata.name = "gamma" | .metadata.namespace = "gamma" | .spec.url = "oci://ghcr.io/devantler-tech/gamma/manifests"' "$root/outside/oci-repository.yaml"
  printf 'resources:\n  - oci-repository.yaml\n' >"$root/outside/kustomization.yaml"
  printf '  - ../../../outside\n' >>"$root/k8s/clusters/prod/kustomization.yaml"
  expect_refusal 'the production overlay cannot load an unpublished directory' "$root" 'published k8s' 'UNKNOWN'

  for boundary in absolute relative; do
    root="$(fixture "closure-symlink-$boundary")"
    mkdir "$root/outside"
    cp "$root/k8s/bases/apps/beta/kustomization.yaml" "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/outside/"
    if [ "$boundary" = absolute ]; then
      ln -s "$root/outside" "$root/k8s/outside-link"
    else
      ln -s ../outside "$root/k8s/outside-link"
    fi
    printf '  - ../../../outside-link\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    expect_refusal "a $boundary symlink cannot escape the published artifact" "$root" 'published k8s' 'UNKNOWN'
  done

  root="$(fixture closure-overlay-symlink)"
  mv "$root/k8s/clusters/prod" "$root/outside-overlay"
  ln -s "$root/outside-overlay" "$root/k8s/clusters/prod"
  expect_refusal 'the overlay itself cannot be an outside symlink' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-source-symlink)"
  mv "$root/k8s" "$root/k8s-real"
  ln -s "$root/k8s-real" "$root/k8s"
  expect_refusal 'a symlinked publication root is refused before copying' "$root" 'symlinked k8s' 'UNKNOWN'

  root="$(fixture closure-absolute-directory)"
  printf '  - %s\n' "$root/k8s/bases/apps/beta" >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal 'absolute directory references cannot depend on a checkout path' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-contained-base)"
  printf '  - ../../../bases/apps/beta\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_pass 'contained relative bases retain exact consumer conservation' "$root" '2 consumer(s)'

  root="$(fixture closure-contained-symlink)"
  ln -s ../../../bases/apps/beta "$root/k8s/providers/prod/infrastructure/beta-link"
  printf '  - beta-link\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal 'even contained directory symlinks are absent from the published artifact' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-contained-file-symlink)"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: file-link\ndata:\n  key: value\n' >"$root/k8s/file-link-target.txt"
  ln -s ../../../file-link-target.txt "$root/k8s/providers/prod/infrastructure/file-link.yaml"
  printf '  - file-link.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_pass 'contained selected file symlinks publish regular manifest bytes' "$root" '2 consumer(s)'

  root="$(fixture closure-unpublished-txt)"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unpublished-txt\n' >"$root/k8s/providers/prod/infrastructure/unpublished.txt"
  printf '  - unpublished.txt\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal 'non-manifest resource files are absent from the published artifact' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-unpublished-env)"
  printf 'setting=value\n' >"$root/k8s/providers/prod/infrastructure/settings.env"
  printf 'configMapGenerator:\n  - name: unpublished-env\n    envs:\n      - settings.env\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal 'non-manifest generator inputs are absent from the published artifact' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-unpublished-kustomization)"
  mv "$root/k8s/providers/prod/infrastructure/kustomization.yaml" "$root/k8s/providers/prod/infrastructure/Kustomization"
  expect_refusal 'extensionless Kustomization files are absent from the published artifact' "$root" 'published k8s' 'UNKNOWN'

  root="$(fixture closure-zero-selected)"
  : >"$root/k8s/empty.yaml"
  expect_refusal 'an empty selected manifest prevents publication even when unused' "$root" 'empty selected manifest' 'UNKNOWN'

  root="$(fixture closure-selected-directory-link)"
  ln -s ../../../bases/apps/beta "$root/k8s/providers/prod/infrastructure/directory-link.yaml"
  expect_refusal 'a selected directory link prevents publication even when unused' "$root" 'selected manifest' 'directory link' 'UNKNOWN'

  root="$(fixture closure-unreferenced-outside-file-link)"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside-file-link\n' >"$root/outside.yaml"
  ln -s "$root/outside.yaml" "$root/k8s/outside-file-link.yaml"
  expect_refusal 'selected file links cannot read outside the source artifact boundary' "$root" 'selected manifest' 'outside' 'UNKNOWN'

  for boundary in uppercase-yaml uppercase-json ignored-yml; do
    root="$(fixture "closure-published-$boundary")"
    reference='selected.YAML'
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: selected\n' >"$WORK/selected.yaml"
    case "$boundary" in
      uppercase-json)
        reference='selected.JSON'
        yq -o=json '.' "$WORK/selected.yaml" >"$root/k8s/providers/prod/infrastructure/$reference" ;;
      ignored-yml)
        reference='.selected.YmL'
        cp "$WORK/selected.yaml" "$root/k8s/providers/prod/infrastructure/$reference"
        printf '.selected.YmL\n' >"$root/k8s/providers/prod/infrastructure/.gitignore" ;;
      *) cp "$WORK/selected.yaml" "$root/k8s/providers/prod/infrastructure/$reference" ;;
    esac
    printf '  - %s\n' "$reference" >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    expect_pass "publisher selection retains $boundary manifest bytes" "$root" '2 consumer(s)'
  done

  root="$(fixture closure-unused)"
  mkdir "$root/k8s/unused"
  printf 'resources:\n  - https://github.com/devantler-tech/platform//k8s/unresolved\n' >"$root/k8s/unused/kustomization.yaml"
  expect_pass 'unused Kustomizations do not enter the reachable dependency closure' "$root" '2 consumer(s)'

  for boundary in scalar entry; do
    root="$(fixture "closure-malformed-$boundary")"
    if [ "$boundary" = scalar ]; then
      yq -i '.resources = "../../../bases/apps/beta"' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    else
      yq -i '.resources = [true]' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    fi
    expect_refusal "a malformed $boundary dependency cannot attest artifact closure" "$root" 'published k8s dependency closure' 'UNKNOWN'
  done

  root="$(fixture closure-remote)"
  printf '  - https://github.com/devantler-tech/platform//k8s/bases/apps/beta?ref=7421f9ddaf6e72efe1366079e906fffdb37966a2\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  mkdir "$WORK/closure-remote-bin"
  cat >"$WORK/closure-remote-bin/kubectl" <<'SH'
#!/usr/bin/env bash
if [ "$1" = kustomize ] && grep -qF 'https://' "$2/kustomization.yaml"; then
  touch "$REMOTE_RENDER_ATTEMPT"
  printf 'offline control intercepted remote loading before any fetch\n' >&2
  exit 2
fi
exec "$REAL_KUBECTL" "$@"
SH
  chmod +x "$WORK/closure-remote-bin/kubectl"
  REAL_KUBECTL="$(command -v kubectl)" REMOTE_RENDER_ATTEMPT="$WORK/remote-render-attempt" PATH="$WORK/closure-remote-bin:$PATH" \
    expect_refusal 'reachable remote dependencies are refused before rendering' "$root" 'published k8s dependency closure' 'UNKNOWN'
  if [ -e "$WORK/remote-render-attempt" ]; then
    fail 'a remote dependency reached kubectl instead of refusing before rendering'
  else
    pass 'no remote dependency reached kubectl or performed a fetch'
  fi

  for boundary in checks references; do
    root="$(fixture "closure-partial-reader-$boundary")"
    mkdir "$WORK/closure-partial-reader-$boundary-bin"
    cat >"$WORK/closure-partial-reader-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_CLOSURE:$3" in
  checks:*'as $closure_paths'*) exit 2 ;;
  references:*'[.mode, .path]'*) exit 2 ;;
esac
SH
    chmod +x "$WORK/closure-partial-reader-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" PARTIAL_CLOSURE="$boundary" PATH="$WORK/closure-partial-reader-$boundary-bin:$PATH" \
      expect_refusal "partial $boundary closure evidence cannot attest an artifact" "$root" 'could not read the published k8s dependency closure' 'UNKNOWN'
  done
}

# Require preflight refusal before a renderer can inspect a deliberately unavailable input.
# The wrapper is a no-fetch witness, not a substitute successful render.
closure_expect_no_render() {
  local title="$1" root="$2" marker="$WORK/closure-loader-attempt"
  rm -f "$marker"
  REAL_KUBECTL="$(command -v kubectl)" CLOSURE_RENDER_ATTEMPT="$marker" PATH="$WORK/closure-loader-bin:$PATH" \
    expect_refusal "$title" "$root" 'published k8s dependency closure' 'UNKNOWN'
  if [ -e "$marker" ]; then
    fail "$title reached kubectl instead of refusing before rendering"
  else
    pass "$title never reached the loader"
  fi
}

# Intercept unavailable inputs before the real renderer can perform any file/network load.
closure_make_loader_witness() {
  [ ! -d "$WORK/closure-loader-bin" ] || return 0
  mkdir "$WORK/closure-loader-bin"
  cat >"$WORK/closure-loader-bin/kubectl" <<'SH'
#!/usr/bin/env bash
case "$1:$2" in
  kustomize:*/providers/prod/infrastructure)
    touch "$CLOSURE_RENDER_ATTEMPT"
    printf 'offline control intercepted build inputs before any load or fetch\n' >&2
    exit 2 ;;
esac
exec "$REAL_KUBECTL" "$@"
SH
  chmod +x "$WORK/closure-loader-bin/kubectl"
}

# All declared file-loader inputs must be accounted for, not only resource directories.
regression_published_loader_inputs() {
  local root boundary field reference config
  closure_make_loader_witness

  for boundary in empty truncated; do
    root="$(fixture "closure-successful-partial-$boundary")"
    printf '  - https://github.com/devantler-tech/platform//k8s/bases/apps/beta?ref=7421f9dd\n' \
      >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    mkdir "$WORK/closure-successful-partial-$boundary-bin"
    cat >"$WORK/closure-successful-partial-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$3" in
  '[.resources[], .bases[], .components[]] | .[]' | *'[.mode, .path]'*)
    if [ "$SUCCESSFUL_PARTIAL" = empty ]; then exit 0; fi
    "$REAL_YQ" "$@" | head -n 1
    exit 0 ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/closure-successful-partial-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" SUCCESSFUL_PARTIAL="$boundary" \
      PATH="$WORK/closure-successful-partial-$boundary-bin:$PATH" \
      closure_expect_no_render "successful $boundary path extraction cannot attest completeness" "$root"
  done

  for field in patches patchesJson6902 patchesStrategicMerge configurations crds transformers generators \
    configMap-files secret-files configMap-envs secret-envs configMap-env secret-env replacements openapi; do
    for boundary in remote omitted; do
      root="$(fixture "closure-loader-$field-$boundary")"
      reference='unpublished.txt'
      [ "$boundary" != remote ] || reference='https://github.com/devantler-tech/platform/raw/7421f9dd/loader.yaml'
      config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
      case "$field" in
        patches | patchesJson6902 | replacements) printf '%s:\n  - path: %s\n' "$field" "$reference" >>"$config" ;;
        patchesStrategicMerge | configurations | crds | transformers | generators) printf '%s:\n  - %s\n' "$field" "$reference" >>"$config" ;;
        configMap-files | secret-files)
          printf '%sGenerator:\n  - name: loader\n    files:\n      - key=%s\n' "${field%-files}" "$reference" >>"$config" ;;
        configMap-envs | secret-envs)
          printf '%sGenerator:\n  - name: loader\n    envs:\n      - %s\n' "${field%-envs}" "$reference" >>"$config" ;;
        configMap-env | secret-env)
          printf '%sGenerator:\n  - name: loader\n    env: %s\n' "${field%-env}" "$reference" >>"$config" ;;
        openapi) printf 'openapi:\n  path: %s\n' "$reference" >>"$config" ;;
      esac
      closure_expect_no_render "$field $boundary inputs must refuse before rendering" "$root"
    done
  done

  for field in patches patchesJson6902 patchesStrategicMerge configurations crds transformers generators \
    configMap-files secret-files configMap-envs secret-envs configMap-env secret-env replacements openapi; do
    root="$(fixture "closure-loader-local-$field")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: input\ndata:\n  value: unchanged\n' \
      >"${config%/*}/input.yaml"
    printf '  - input.yaml\n' >>"$config"
    case "$field" in
      patches | patchesStrategicMerge)
        printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: input\ndata:\n  value: unchanged\n' >"${config%/*}/loader.yaml"
        if [ "$field" = patches ]; then
          printf 'patches:\n  - path: loader.yaml\n' >>"$config"
        else
          printf 'patchesStrategicMerge:\n  - loader.yaml\n' >>"$config"
        fi ;;
      patchesJson6902)
        printf '[{"op":"replace","path":"/data/value","value":"unchanged"}]\n' >"${config%/*}/loader.json"
        printf 'patchesJson6902:\n  - target:\n      version: v1\n      kind: ConfigMap\n      name: input\n    path: loader.json\n' >>"$config" ;;
      configurations)
        printf 'namePrefix:\n  - path: metadata/name\n' >"${config%/*}/loader.yaml"
        printf 'configurations:\n  - loader.yaml\n' >>"$config" ;;
      crds | openapi)
        if [ "$field" = openapi ]; then
          printf '{"swagger":"2.0","info":{"title":"fixture","version":"v1"},"paths":{},"definitions":{}}\n' >"${config%/*}/loader.json"
          printf 'openapi:\n  path: loader.json\n' >>"$config"
        else
          printf '{"fixture":{"type":"object"}}\n' >"${config%/*}/loader.json"
          printf 'crds:\n  - loader.json\n' >>"$config"
        fi ;;
      transformers)
        printf 'apiVersion: builtin\nkind: AnnotationsTransformer\nmetadata:\n  name: loader\nannotations:\n  fixture: local\nfieldSpecs:\n  - path: metadata/annotations\n    create: true\n' >"${config%/*}/loader.yaml"
        printf 'transformers:\n  - loader.yaml\n' >>"$config" ;;
      generators)
        mkdir "${config%/*}/nested"
        printf 'apiVersion: builtin\nkind: ConfigMapGenerator\nmetadata:\n  name: loader\nfiles:\n  - settings.yaml\n' >"${config%/*}/nested/loader.yaml"
        printf 'setting=value\n' >"${config%/*}/settings.yaml"
        printf 'generators:\n  - nested/loader.yaml\n' >>"$config" ;;
      configMap-files | secret-files)
        printf 'setting=value\n' >"${config%/*}/settings.yaml"
        printf '%sGenerator:\n  - name: loader\n    files:\n      - key=settings.yaml\n' "${field%-files}" >>"$config" ;;
      configMap-envs | secret-envs)
        printf 'setting=value\n' >"${config%/*}/settings.yaml"
        printf '%sGenerator:\n  - name: loader\n    envs:\n      - settings.yaml\n' "${field%-envs}" >>"$config" ;;
      configMap-env | secret-env)
        printf 'setting=value\n' >"${config%/*}/settings.yaml"
        printf '%sGenerator:\n  - name: loader\n    env: settings.yaml\n' "${field%-env}" >>"$config" ;;
      replacements)
        printf 'source:\n  kind: ConfigMap\n  name: input\n  fieldPath: data.value\ntargets:\n  - select:\n      kind: ConfigMap\n      name: input\n    fieldPaths:\n      - data.value\n' >"${config%/*}/loader.yaml"
        printf 'replacements:\n  - path: loader.yaml\n' >>"$config" ;;
    esac
    expect_pass "$field retains valid selected local-file inputs" "$root" '2 consumer(s)'
  done

  root="$(fixture closure-inline-loaders)"
  cat >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml" <<'YAML'
patchesStrategicMerge:
  - |-
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: inline
    data:
      value: https://example.test/data
patches:
  - patch: |-
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: inline
      data:
        value: https://example.test/data
transformers:
  - |-
    apiVersion: builtin
    kind: AnnotationsTransformer
    metadata:
      name: inline
    annotations:
      fixture: https://example.test/data
    fieldSpecs:
      - path: metadata/annotations
        create: true
generators:
  - |-
    apiVersion: builtin
    kind: ConfigMapGenerator
    metadata:
      name: inline
    literals:
      - value=https://example.test/data
secretGenerator:
  - name: inline-secret
    literals:
      - fixture=https://example.test/data
YAML
  expect_pass 'inline patches, builtin configs and literal URLs remain data' "$root" '2 consumer(s)'

  root="$(fixture closure-inline-json-patch)"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: inline-json\n' >"$root/k8s/providers/prod/infrastructure/input.yaml"
  printf '  - input.yaml\npatchesStrategicMerge:\n  - '\''{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"inline-json"}}'\''\n' \
    >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_pass 'single-line JSON strategic patches remain inline data' "$root" '2 consumer(s)'

  for field in generator-file generator-inline secret-generator-file generator-envs secret-generator-envs generator-env patch-transformer patch-transformer-inline; do
    root="$(fixture "closure-builtin-$field")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    case "$field" in
      generator* | secret-generator*)
        reference='generators'
        boundary=ConfigMapGenerator
        [[ "$field" != secret-* ]] || boundary=SecretGenerator
        printf 'apiVersion: builtin\nkind: %s\nmetadata:\n  name: loader\n' "$boundary" >"${config%/*}/loader.yaml"
        case "$field" in
          *-envs) printf 'envs:\n  - https://example.test/unavailable.yaml\n' >>"${config%/*}/loader.yaml" ;;
          *-env) printf 'env: https://example.test/unavailable.yaml\n' >>"${config%/*}/loader.yaml" ;;
          *) printf 'files:\n  - key=https://example.test/unavailable.yaml\n' >>"${config%/*}/loader.yaml" ;;
        esac ;;
      patch-transformer*)
        reference='transformers'
        printf 'apiVersion: builtin\nkind: PatchTransformer\nmetadata:\n  name: loader\npath: https://example.test/unavailable.yaml\n' >"${config%/*}/loader.yaml" ;;
    esac
    if [[ "$field" = *-inline ]]; then
      LOADER="$(cat "${config%/*}/loader.yaml")" FIELD="$reference" yq -i '.[strenv(FIELD)] = [strenv(LOADER)]' "$config"
    else
      printf '%s:\n  - loader.yaml\n' "$reference" >>"$config"
    fi
    closure_expect_no_render "$field refuses builtin-config remote paths before rendering" "$root"
  done

  for boundary in patches-array patches-path configurations generators-files generators-envs generator-file-type openapi path-control legacy-mapping-path; do
    root="$(fixture "closure-malformed-loader-$boundary")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    case "$boundary" in
      patches-array) yq -i '.patches = {"path":"missing.yaml"}' "$config" ;;
      patches-path) yq -i '.patches = [{"path":123}]' "$config" ;;
      configurations) yq -i '.configurations = "missing.yaml"' "$config" ;;
      generators-files) yq -i '.configMapGenerator = [{"name":"loader","files":"missing.yaml"}]' "$config" ;;
      generators-envs) yq -i '.secretGenerator = [{"name":"loader","envs":"missing.yaml"}]' "$config" ;;
      generator-file-type) yq -i '.configMapGenerator = [{"name":"loader","files":[true]}]' "$config" ;;
      openapi) yq -i '.openapi = "missing.yaml"' "$config" ;;
      path-control) yq -i '.patches = [{"path":"missing\t.yaml"}]' "$config" ;;
      legacy-mapping-path) yq -i '.patchesStrategicMerge = ["https://example.test/patch: payload"]' "$config" ;;
    esac
    closure_expect_no_render "malformed $boundary cannot authorize a loader" "$root"
  done
}

# Installed builtin legacy strategic transformers load paths[] from their owning root.
regression_builtin_legacy_paths() {
  local root boundary config
  closure_make_loader_witness
  for boundary in file inline; do
    root="$(fixture "closure-legacy-builtin-$boundary")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    printf 'apiVersion: builtin\nkind: PatchStrategicMergeTransformer\nmetadata:\n  name: loader\npaths:\n  - https://example.test/unavailable.yaml\n' >"${config%/*}/loader.yaml"
    if [ "$boundary" = inline ]; then
      LOADER="$(cat "${config%/*}/loader.yaml")" yq -i '.transformers = [strenv(LOADER)]' "$config"
    else
      printf 'transformers:\n  - loader.yaml\n' >>"$config"
    fi
    closure_expect_no_render "legacy builtin $boundary paths refuse before rendering" "$root"
  done
  root="$(fixture closure-legacy-builtin-local)"
  config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: legacy-input\ndata:\n  value: local\n' >"${config%/*}/input.yaml"
  printf '  - input.yaml\ntransformers:\n  - loader.yaml\n' >>"$config"
  printf 'apiVersion: builtin\nkind: PatchStrategicMergeTransformer\nmetadata:\n  name: loader\npaths:\n  - patch.yaml\n' >"${config%/*}/loader.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: legacy-input\ndata:\n  value: local\n' >"${config%/*}/patch.yaml"
  expect_pass 'legacy builtin paths retain selected local strategic patches' "$root" '2 consumer(s)'
  root="$(fixture closure-legacy-builtin-inline-patch)"
  config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: legacy-input\ndata:\n  value: local\n' >"${config%/*}/input.yaml"
  printf '  - input.yaml\ntransformers:\n  - loader.yaml\n' >>"$config"
  printf 'apiVersion: builtin\nkind: PatchStrategicMergeTransformer\nmetadata:\n  name: loader\npatches: |-\n  apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: legacy-input\n  data:\n    value: local\n' >"${config%/*}/loader.yaml"
  expect_pass 'legacy builtin inline patch strings remain data' "$root" '2 consumer(s)'
}

# Inline replacement configs have the same secondary file inputs as selected config files.
regression_inline_replacement_paths() {
  local root config boundary
  closure_make_loader_witness
  for boundary in remote local malformed; do
    root="$(fixture "closure-inline-replacement-$boundary")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: replacement-input\ndata:\n  value: unchanged\n' >"${config%/*}/input.yaml"
    printf '  - input.yaml\n' >>"$config"
    cat >>"$config" <<'YAML'
transformers:
  - |-
    apiVersion: builtin
    kind: ReplacementTransformer
    metadata:
      name: replacements
    replacements:
      - path: replacement.yaml
YAML
    case "$boundary" in
      remote)
        yq -i '.transformers[0] |= sub("replacement.yaml", "https://example.test/unavailable.yaml")' "$config"
        closure_expect_no_render 'inline builtin replacement paths refuse before rendering' "$root" ;;
      local)
        printf 'source:\n  kind: ConfigMap\n  name: replacement-input\n  fieldPath: data.value\ntargets:\n  - select:\n      kind: ConfigMap\n      name: replacement-input\n    fieldPaths:\n      - data.value\n' >"${config%/*}/replacement.yaml"
        expect_pass 'inline builtin replacement paths retain selected local files' "$root" '2 consumer(s)' ;;
      malformed)
        yq -i '.transformers[0] = "apiVersion: builtin\nkind: ReplacementTransformer\nmetadata:\n  name: replacements\nreplacements:\n  path: missing.yaml\n"' "$config"
        closure_expect_no_render 'malformed inline replacements cannot authorize a loader' "$root" ;;
    esac
  done
}

# Parse whole inline strings: from_yaml alone does not attest later YAML documents.
regression_inline_document_bounds() {
  local root field boundary first second inline config
  closure_make_loader_witness
  for field in generators transformers; do
    if [ "$field" = generators ]; then
      first=$'apiVersion: builtin\nkind: ConfigMapGenerator\nmetadata:\n  name: first\nliterals:\n  - value=local\n'
      second=$'apiVersion: builtin\nkind: ConfigMapGenerator\nmetadata:\n  name: second\nfiles:\n  - key=https://example.test/unavailable.yaml\n'
    else
      first=$'apiVersion: builtin\nkind: AnnotationsTransformer\nmetadata:\n  name: first\nannotations:\n  value: local\nfieldSpecs:\n  - path: metadata/annotations\n    create: true\n'
      second=$'apiVersion: builtin\nkind: ReplacementTransformer\nmetadata:\n  name: second\nreplacements:\n  - path: https://example.test/unavailable.yaml\n'
    fi
    for boundary in plain commented middle-null; do
      root="$(fixture "closure-inline-documents-$field-$boundary")"
      config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
      case "$boundary" in
        plain) inline="$first"$'---\n'"$second" ;;
        commented) inline="$first"$'--- # another document\n'"$second" ;;
        middle-null) inline="$first"$'---\n\n---\n'"$second" ;;
      esac
      INLINE="$inline" FIELD="$field" yq -i '.[strenv(FIELD)] = [strenv(INLINE)]' "$config"
      closure_expect_no_render "multi-document inline $field $boundary must refuse before rendering" "$root"
    done

    root="$(fixture "closure-inline-literal-separator-$field")"
    config="$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    if [ "$field" = generators ]; then
      inline=$'---\napiVersion: builtin\nkind: ConfigMapGenerator\nmetadata:\n  name: literal-separator\nliterals:\n  - |-\n    value=first\n    --- # literal data\n    second\n'
    else
      inline=$'---\napiVersion: builtin\nkind: AnnotationsTransformer\nmetadata:\n  name: literal-separator\nannotations:\n  value: |-\n    first\n    --- # literal data\n    second\nfieldSpecs:\n  - path: metadata/annotations\n    create: true\n'
    fi
    INLINE="$inline" FIELD="$field" yq -i '.[strenv(FIELD)] = [strenv(INLINE)]' "$config"
    expect_pass "single-document inline $field preserves leading marker and literal separators" "$root" '2 consumer(s)'
  done

  for boundary in empty truncated failed; do
    root="$(fixture "closure-inline-document-receipt-$boundary")"
    INLINE="$first" FIELD=transformers yq -i '.[strenv(FIELD)] = [strenv(INLINE)]' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    mkdir "$WORK/closure-inline-doc-$boundary-bin"
    cat >"$WORK/closure-inline-doc-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$*" in
  *'INLINE_DOCUMENTS'*)
    case "$INLINE_PARTIAL" in
      empty) exit 0 ;;
      truncated) "$REAL_YQ" "$@" | cut -f1-2; exit 0 ;;
      failed) "$REAL_YQ" "$@" || exit $?; exit 2 ;;
    esac ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/closure-inline-doc-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" INLINE_PARTIAL="$boundary" PATH="$WORK/closure-inline-doc-$boundary-bin:$PATH" \
      closure_expect_no_render "incomplete $boundary document receipt cannot authorize inline input" "$root"
  done

  for boundary in empty truncated; do
    root="$(fixture "closure-inline-extraction-$boundary")"
    INLINE="$first" yq -i '.transformers = [strenv(INLINE), strenv(INLINE)]' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    mkdir "$WORK/closure-inline-extract-$boundary-bin"
    cat >"$WORK/closure-inline-extract-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$3" in
  '[.transformers[], .generators[]] | .[] | to_json(0)')
    if [ "$INLINE_PARTIAL" = empty ]; then exit 0; fi
    "$REAL_YQ" "$@" | head -n 1
    exit 0 ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/closure-inline-extract-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" INLINE_PARTIAL="$boundary" PATH="$WORK/closure-inline-extract-$boundary-bin:$PATH" \
      closure_expect_no_render "successful $boundary inline extraction cannot attest complete input" "$root"
  done

  for boundary in empty truncated failed; do
    root="$(fixture "closure-inline-decoder-$boundary")"
    INLINE="$first"$'---\n'"$second" yq -i '.transformers = [strenv(INLINE)]' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    mkdir "$WORK/closure-inline-decoder-$boundary-bin"
    cat >"$WORK/closure-inline-decoder-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
if [ "$3" = . ] && [[ "$4" = */inline-input.json ]]; then
  case "$INLINE_PARTIAL" in
    empty) exit 0 ;;
    truncated) "$REAL_YQ" "$@" | head -n 1; exit 0 ;;
    failed) "$REAL_YQ" "$@" || exit $?; exit 2 ;;
  esac
fi
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/closure-inline-decoder-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" INLINE_PARTIAL="$boundary" PATH="$WORK/closure-inline-decoder-$boundary-bin:$PATH" \
      closure_expect_no_render "partial $boundary decoder output cannot lose later inline documents" "$root"
  done

  for boundary in empty truncated failed; do
    root="$(fixture "closure-inline-byte-receipt-$boundary")"
    INLINE="$first" yq -i '.transformers = [strenv(INLINE)]' "$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    mkdir "$WORK/closure-inline-bytes-$boundary-bin"
    cat >"$WORK/closure-inline-bytes-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$*" in
  *'load_str(strenv(DECODED_INPUT))'*)
    case "$INLINE_PARTIAL" in
      empty) exit 0 ;;
      truncated) "$REAL_YQ" "$@" | cut -c1-2; exit 0 ;;
      failed) "$REAL_YQ" "$@" || exit $?; exit 2 ;;
    esac ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/closure-inline-bytes-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" INLINE_PARTIAL="$boundary" PATH="$WORK/closure-inline-bytes-$boundary-bin:$PATH" \
      closure_expect_no_render "incomplete $boundary decoded-byte receipt cannot authorize inline input" "$root"
  done
}

# OCI object identities are part of conservation even when report selectors are equal.
regression_consumer_identities() {
  local root field value target boundary
  for field in name namespace url; do
    root="$(fixture "identity-patched-$field")"
    case "$field" in
      name) target='/metadata/name'; value='beta-renamed' ;;
      namespace) target='/metadata/namespace'; value='beta-other' ;;
      url) target='/spec/url'; value='oci://ghcr.io/devantler-tech/beta/manifests/' ;;
    esac
    cat >>"$root/k8s/providers/prod/apps/kustomization.yaml" <<YAML
patches:
  - target:
      kind: OCIRepository
      name: beta
    patch: |-
      - op: replace
        path: $target
        value: $value
YAML
    case "$field" in
      name) expect_refusal 'a rename-only overlay cannot conserve the old OCI identity' "$root" 'DISAGREE' 'source=beta/beta ' 'source=beta/beta-renamed ' ;;
      namespace) expect_refusal 'a namespace-only overlay cannot conserve the old OCI identity' "$root" 'DISAGREE' 'source=beta/beta ' 'source=beta-other/beta ' ;;
      url) expect_refusal 'an exact URL change cannot hide behind the same report artifact' "$root" 'DISAGREE' 'artifact=beta/manifests' 'url=oci://ghcr.io/devantler-tech/beta/manifests/' ;;
    esac
  done

  for field in name namespace; do
    root="$(fixture "identity-distinct-$field")"
    cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/apps/peer.yaml"
    if [ "$field" = name ]; then
      yq -i '.metadata.name = "beta-peer"' "$root/k8s/providers/prod/apps/peer.yaml"
    else
      yq -i '.metadata.namespace = "beta-peer"' "$root/k8s/providers/prod/apps/peer.yaml"
    fi
    printf '  - peer.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_pass "different OCI $field identities using one artifact remain distinct" "$root" '3 consumer(s)'
  done

  root="$(fixture identity-repeated-root)"
  printf '  - ../../../bases/apps/beta\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_pass 'identical OCI identities repeated across roots remain one consumer' "$root" '2 consumer(s)'

  root="$(fixture identity-conflicting-roots)"
  cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/infrastructure/conflict.yaml"
  yq -i '.spec.ref.tag = "9.9.9"' "$root/k8s/providers/prod/infrastructure/conflict.yaml"
  printf '  - conflict.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
  expect_refusal 'conflicting declarations of one OCI identity are UNKNOWN' "$root" 'conflicting OCIRepository identity' 'beta/beta' 'UNKNOWN'

  # Unsigned declarations are absent from report rows but can overwrite the same
  # object. Missing namespaces cannot prove those objects are disjoint.
  for target in explicit unsigned-absent consumer-absent foreign-registry; do
    root="$(fixture "identity-unsigned-overwrite-$target")"
    cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    yq -i 'del(.spec.verify) | .spec.url = "oci://ghcr.io/devantler-tech/epsilon/manifests"' \
      "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    case "$target" in
      unsigned-absent) yq -i 'del(.metadata.namespace)' "$root/k8s/providers/prod/infrastructure/unsigned.yaml" ;;
      consumer-absent) yq -i 'del(.metadata.namespace)' "$root/k8s/bases/apps/beta/oci-repository.yaml" ;;
      foreign-registry) yq -i '.spec.url = "oci://registry.example.test/epsilon/manifests"' "$root/k8s/providers/prod/infrastructure/unsigned.yaml" ;;
    esac
    printf '  - unsigned.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    expect_refusal "an unsigned $target declaration cannot overwrite an attributed OCI object" "$root" 'conflicting OCIRepository identity' 'beta' 'UNKNOWN'
  done

  for field in name namespace; do
    root="$(fixture "identity-unsigned-unrelated-$field")"
    cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    yq -i 'del(.spec.verify) | .spec.url = "oci://ghcr.io/devantler-tech/epsilon/manifests"' \
      "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    if [ "$field" = name ]; then
      yq -i '.metadata.name = "epsilon"' "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    else
      yq -i '.metadata.namespace = "epsilon"' "$root/k8s/providers/prod/infrastructure/unsigned.yaml"
    fi
    printf '  - unsigned.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    expect_pass "an unsigned unrelated OCI $field identity remains outside the report" "$root" '2 consumer(s)'
  done

  for boundary in failure truncated; do
    root="$(fixture "identity-all-object-partial-$boundary")"
    mkdir "$WORK/identity-all-object-partial-$boundary-bin"
    cat >"$WORK/identity-all-object-partial-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$*" in
  *'(.spec.verify // {} | sort_keys(..) | to_json(0))'*)
    if [ "$PARTIAL_IDENTITY" = truncated ]; then
      "$REAL_YQ" "$@" | cut -f1-9
      exit 0
    fi
    "$REAL_YQ" "$@" || exit $?
    exit 2 ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/identity-all-object-partial-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" PARTIAL_IDENTITY="$boundary" PATH="$WORK/identity-all-object-partial-$boundary-bin:$PATH" \
      expect_refusal "partial $boundary all-object evidence cannot attest conservation" "$root" 'could not read OCI object contracts' 'UNKNOWN'
  done

  for target in missing-name numeric-name mapping-name template-name numeric-namespace; do
    root="$(fixture "identity-malformed-$target")"
    mkdir "$root/docs"
    cp "$root/k8s/bases/apps/beta/oci-repository.yaml" "$root/docs/malformed.yaml"
    case "$target" in
      missing-name) yq -i 'del(.metadata.name)' "$root/docs/malformed.yaml" ;;
      numeric-name) yq -i '.metadata.name = 123' "$root/docs/malformed.yaml" ;;
      mapping-name) yq -i '.metadata.name = {"value":"beta"}' "$root/docs/malformed.yaml" ;;
      template-name) yq -i '.metadata.name = "{{ inputs.name }}"' "$root/docs/malformed.yaml" ;;
      numeric-namespace) yq -i '.metadata.namespace = 123' "$root/docs/malformed.yaml" ;;
    esac
    expect_refusal "a $target raw consumer has no literal object identity" "$root" 'consumer identity' 'malformed.yaml' 'UNKNOWN'
  done

  for boundary in failure truncated; do
    root="$(fixture "identity-partial-$boundary")"
    mkdir "$WORK/identity-partial-$boundary-bin"
    cat >"$WORK/identity-partial-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
case "$*" in
  *'(.metadata.name | type)'*)
  if [ "$PARTIAL_IDENTITY" = truncated ]; then
    "$REAL_YQ" "$@" | cut -f1-6
    exit 0
  fi
  "$REAL_YQ" "$@" || exit $?
  exit 2 ;;
esac
exec "$REAL_YQ" "$@"
SH
    chmod +x "$WORK/identity-partial-$boundary-bin/yq"
    REAL_YQ="$(command -v yq)" PARTIAL_IDENTITY="$boundary" PATH="$WORK/identity-partial-$boundary-bin:$PATH" \
      expect_refusal "partial $boundary consumer identity evidence cannot attest conservation" "$root" 'could not read the consumers in the production render' 'UNKNOWN'
  done
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
  nested:*'select(.spec.sourceRef.kind == "OCIRepository"'*) exit 2 ;;
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

regression_controller_findings() {
  local root target placement want boundary
  for target in foreign missing; do
    root="$(fixture "consumer-api-$target")"
    if [ "$target" = foreign ]; then
      yq -i '.apiVersion = "unrelated.example.test/v1"' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
    else
      yq -i 'del(.apiVersion)' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
    fi
    expect_pass "a $target API-group peer is not a Flux consumer" "$root" '1 consumer(s)'
  done
  root="$(fixture consumer-api-flux-beta)"
  yq -i '.apiVersion = "source.toolkit.fluxcd.io/v1beta2"' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
  expect_pass 'another Flux source API version remains a consumer' "$root" '2 consumer(s)'

  for target in source root clone clone-list wildcard carrier; do
    root="$(fixture "generated-consumer-$target")"
    cat >"$root/k8s/providers/prod/apps/generate.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generated-source
spec:
  rules:
    - name: generate
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: source.toolkit.fluxcd.io/v1
        kind: OCIRepository
        name: generated
        namespace: generated
        data:
          spec:
            url: oci://ghcr.io/devantler-tech/generated/manifests
YAML
    case "$target" in
      root) yq -i '.spec.rules[0].generate.apiVersion = "kustomize.toolkit.fluxcd.io/v1" | .spec.rules[0].generate.kind = "Kustomization" |
          .spec.rules[0].generate.data.spec = {"path":"bases/unseen","sourceRef":{"kind":"OCIRepository","name":"flux-system"}}' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      clone) yq -i 'del(.spec.rules[0].generate.data) | .spec.rules[0].generate.clone = {"namespace":"alpha","name":"alpha"}' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      clone-list) yq -i 'del(.spec.rules[0].generate.kind,.spec.rules[0].generate.apiVersion,.spec.rules[0].generate.data) |
          .spec.rules[0].generate.cloneList = {"kinds":["source.toolkit.fluxcd.io/v1/OCIRepository"],"namespace":"alpha"}' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      wildcard) yq -i '.spec.rules[0].generate.kind = "*"' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      carrier) yq -i '.spec.rules[0].generate.kind = "ResourceSet" | .spec.rules[0].generate.apiVersion = "fluxcd.controlplane.io/v1"' "$root/k8s/providers/prod/apps/generate.yaml" ;;
    esac
    printf '  - generate.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "Kyverno $target generation cannot omit a runtime consumer" "$root" 'consumer generation' 'UNKNOWN'
  done
  root="$(fixture unrelated-generation)"
  cp "$WORK/generated-consumer-source/k8s/providers/prod/apps/generate.yaml" "$root/k8s/providers/prod/apps/generate.yaml"
  yq -i '.spec.rules[0].generate.kind = "ConfigMap" | .spec.rules[0].generate.apiVersion = "v1" |
    .spec.rules[0].generate.data = {"data":{"ordinary":"value"}}' "$root/k8s/providers/prod/apps/generate.yaml"
  printf '  - generate.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'literal ConfigMap-only generation remains unrelated' "$root" '2 consumer(s)'

  for target in subject url spec; do
    root="$(fixture "substituted-key-$target")"
    # shellcheck disable=SC2016 # These are literal Flux post-build mapping keys.
    case "$target" in
      subject) yq -i '.spec.verify.matchOIDCIdentity[0]["${SUBJECT_FIELD}"] = .spec.verify.matchOIDCIdentity[0].subject |
          del(.spec.verify.matchOIDCIdentity[0].subject)' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
      url) yq -i '.spec["${URL_FIELD}"] = .spec.url | del(.spec.url)' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
      spec) yq -i '.["${SPEC_FIELD}"] = .spec | del(.spec)' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
    esac
    yq -i '.spec.postBuild.substitute = {"SUBJECT_FIELD":"subject","URL_FIELD":"url","SPEC_FIELD":"spec","KIND_FIELD":"kind"}' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
    expect_refusal "a substituted consumer $target key is not silently omitted" "$root" 'mapping key' 'UNKNOWN'
  done
  root="$(fixture ordinary-data-key)"
  cat >"$root/k8s/providers/prod/apps/config-map.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ordinary
data:
  ${DATA_FIELD}: value
YAML
  printf '  - config-map.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a data-only ConfigMap mapping key does not change consumers' "$root" '2 consumer(s)'

  for placement in resources steps generation; do
    root="$(fixture "carried-kro-instance-$placement")"
    cat >"$root/k8s/providers/prod/apps/carrier.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: tenants
spec:
  resources:
    - apiVersion: kro.run/v1alpha1
      kind: Tenant
      metadata:
        name: generated
      spec:
        name: generated
YAML
    case "$placement" in
      steps) yq -i '.spec.steps = [{"name":"instances","resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/carrier.yaml" ;;
      generation) cat >"$root/k8s/providers/prod/apps/carrier.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: tenants
spec:
  rules:
    - name: tenant
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: kro.run/v1alpha1
        kind: Tenant
        name: generated
        data:
          spec:
            name: generated
YAML
        ;;
    esac
    printf '  - carrier.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "a $placement carrier cannot hide a consumer-producing kro instance" "$root" 'production renders 1 Tenant instance(s)' 'kro.run/v1alpha1'
  done
  root="$(fixture unrelated-carried-kro-peer)"
  cp "$WORK/carried-kro-instance-resources/k8s/providers/prod/apps/carrier.yaml" "$root/k8s/providers/prod/apps/carrier.yaml"
  yq -i '.spec.resources[0].apiVersion = "unrelated.example.test/v1alpha1"' "$root/k8s/providers/prod/apps/carrier.yaml"
  printf '  - carrier.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a same-kind carrier in another group is not a kro instance' "$root" '2 consumer(s)'

  for target in true string null substituted; do
    root="$(fixture "consumer-suspend-$target")"
    # shellcheck disable=SC2016 # Flux, not the test shell, resolves the expression.
    case "$target" in
      true) yq -i '.spec.suspend = true' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
      string) yq -i '.spec.suspend = "false"' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
      null) yq -i '.spec.suspend = null' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
      substituted) yq -i '.spec.suspend = "${SOURCE_SUSPEND}"' "$root/k8s/bases/apps/alpha/oci-repository.yaml" ;;
    esac
    expect_refusal "a $target consumer suspension cannot attribute a fetched revision" "$root" 'consumer suspension' 'UNKNOWN'
  done
  root="$(fixture consumer-suspend-false)"
  yq -i '.spec.suspend = false' "$root/k8s/bases/apps/alpha/oci-repository.yaml"
  expect_pass 'a literal false consumer suspension remains active' "$root" '2 consumer(s)'

  for boundary in generation keys instances; do
    root="$(fixture "partial-controller-$boundary")"
    mkdir "$WORK/partial-controller-$boundary-bin"
    cat >"$WORK/partial-controller-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_CONTROLLER:$*" in
  generation:*'.generate.foreach[]'*'.cloneList.kinds'*) exit 2 ;;
  keys:*'to_entries'*) exit 2 ;;
  instances:*'[.. | select(type == "!!map"'*'strenv(CONSUMER_GVK_KIND)'*) exit 2 ;;
esac
SH
    chmod +x "$WORK/partial-controller-$boundary-bin/yq"
    case "$boundary" in
      generation) want='could not bound consumer generation' ;;
      keys) want='could not read object mapping keys' ;;
      instances) want='could not count Tenant instances' ;;
    esac
    REAL_YQ="$(command -v yq)" PARTIAL_CONTROLLER="$boundary" PATH="$WORK/partial-controller-$boundary-bin:$PATH" \
      expect_refusal "partial $boundary output cannot clear the controller chain" "$root" "$want" 'UNKNOWN'
  done
}

# Exercise runtime mutation paths that can leave both static consumer sets incomplete.
regression_native_admission_findings() {
  local root placement field target want
  for placement in resources steps; do
    for field in kind name namespace default-namespace; do
      root="$(fixture "nested-source-value-$placement-$field")"
      cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: additional-roots
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
          namespace: flux-system
        path: bases/unseen
YAML
      mkdir -p "$root/k8s/bases/unseen"
      cp "$root/k8s/bases/apps/alpha/oci-repository.yaml" "$root/k8s/bases/unseen/hidden.yml"
      yq -i '.metadata.name = "hidden" | .spec.url = "oci://ghcr.io/devantler-tech/hidden/manifests"' "$root/k8s/bases/unseen/hidden.yml"
      printf 'resources:\n  - hidden.yml\n' >"$root/k8s/bases/unseen/kustomization.yaml"
      # shellcheck disable=SC2016 # Flux resolves the literal source reference after this build.
      if [ "$field" = default-namespace ]; then
        # shellcheck disable=SC2016 # The namespace supplies an omitted source namespace.
        yq -i '.spec.resources[0].metadata.namespace = "${NESTED_SOURCE}" | del(.spec.resources[0].spec.sourceRef.namespace)' "$root/k8s/providers/prod/apps/resource-set.yaml"
      else
        # shellcheck disable=SC2016 # The nested source field is resolved after discovery.
        FIELD="$field" yq -i '.spec.resources[0].spec.sourceRef[strenv(FIELD)] = "${NESTED_SOURCE}"' "$root/k8s/providers/prod/apps/resource-set.yaml"
      fi
      if [ "$placement" = steps ]; then
        yq -i '.spec.steps = [{"name":"extra","resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/resource-set.yaml"
      fi
      printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      expect_refusal "a $placement template cannot substitute its platform source $field" "$root" 'nested source reference' 'UNKNOWN'
    done
  done

  for target in source root carrier wildcard-group wildcard-resources substituted-group substituted-resource missing-group missing-resource missing-operation substituted-operation substituted-key; do
    root="$(fixture "native-webhook-$target")"
    cat >"$root/k8s/providers/prod/apps/webhook.yaml" <<'YAML'
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: source-mutator
webhooks:
  - name: sources.example.test
    admissionReviewVersions: [v1]
    sideEffects: None
    clientConfig:
      url: https://example.test/mutate
    rules:
      - apiGroups: [source.toolkit.fluxcd.io]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [ocirepositories]
YAML
    want='native admission mutation'
    # shellcheck disable=SC2016 # These literal substitutions are controller inputs.
    case "$target" in
      root) yq -i '.webhooks[0].rules[0].apiGroups = ["kustomize.toolkit.fluxcd.io"] | .webhooks[0].rules[0].resources = ["kustomizations"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      carrier) yq -i '.webhooks[0].rules[0].apiGroups = ["fluxcd.controlplane.io"] | .webhooks[0].rules[0].resources = ["resourcesets"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      wildcard-group) yq -i '.webhooks[0].rules[0].apiGroups = ["*"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      wildcard-resources) yq -i '.webhooks[0].rules[0].resources = ["*/*"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      substituted-group) yq -i '.webhooks[0].rules[0].apiGroups = ["${SOURCE_GROUP}"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      substituted-resource) yq -i '.webhooks[0].rules[0].resources = ["${SOURCE_RESOURCE}"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      missing-group) yq -i 'del(.webhooks[0].rules[0].apiGroups)' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      missing-resource) yq -i 'del(.webhooks[0].rules[0].resources)' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      missing-operation) yq -i 'del(.webhooks[0].rules[0].operations)' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      substituted-operation) yq -i '.webhooks[0].rules[0].operations = ["${SOURCE_OPERATION}"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      substituted-key) yq -i '.webhooks[0]["${RULES_FIELD}"] = .webhooks[0].rules | del(.webhooks[0].rules)' "$root/k8s/providers/prod/apps/webhook.yaml"; want='mapping key' ;;
    esac
    printf '  - webhook.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "a $target native mutator cannot attest a static consumer" "$root" "$want" 'UNKNOWN'
  done

  for target in pod foreign validating; do
    root="$(fixture "unrelated-native-webhook-$target")"
    cp "$WORK/native-webhook-source/k8s/providers/prod/apps/webhook.yaml" "$root/k8s/providers/prod/apps/webhook.yaml"
    case "$target" in
      pod) yq -i '.webhooks[0].rules[0].apiGroups = [""] | .webhooks[0].rules[0].resources = ["pods"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      foreign) yq -i '.webhooks[0].rules[0].apiGroups = ["unrelated.example.test"]' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
      validating) yq -i '.kind = "ValidatingWebhookConfiguration"' "$root/k8s/providers/prod/apps/webhook.yaml" ;;
    esac
    printf '  - webhook.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_pass "a $target webhook does not mutate the guarded consumer fields" "$root" '2 consumer(s)'
  done
  root="$(fixture partial-native-webhook)"
  mkdir "$WORK/partial-native-webhook-bin"
  cat >"$WORK/partial-native-webhook-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$*" in *'.webhooks[]'*) exit 2 ;; esac
SH
  chmod +x "$WORK/partial-native-webhook-bin/yq"
  REAL_YQ="$(command -v yq)" PATH="$WORK/partial-native-webhook-bin:$PATH" \
    expect_refusal 'partial native webhook evidence cannot clear consumer discovery' "$root" 'could not bound native admission' 'UNKNOWN'
  root="$(fixture partial-nested-source)"
  mkdir "$WORK/partial-nested-source-bin"
  cat >"$WORK/partial-nested-source-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$*" in *'| [(.spec.sourceRef.kind // "")'*) exit 2 ;; esac
SH
  chmod +x "$WORK/partial-nested-source-bin/yq"
  REAL_YQ="$(command -v yq)" PATH="$WORK/partial-nested-source-bin:$PATH" \
    expect_refusal 'partial nested source evidence cannot clear consumer discovery' "$root" 'could not read nested source references' 'UNKNOWN'

  root="$(fixture dormant-nested-source-rgd)"
  cat >>"$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml" <<'YAML'
    - id: nestedRoot
      template:
        apiVersion: kustomize.toolkit.fluxcd.io/v1
        kind: Kustomization
        metadata:
          name: generated-root
          namespace: ${schema.spec.name}
        spec:
          sourceRef:
            kind: OCIRepository
            name: ${ociRepository.metadata.name}
          path: deploy
YAML
  expect_pass 'an OCI-producing RGD with no instances keeps its dormant nested references' "$root" '2 consumer(s)'
  cat >"$root/k8s/providers/prod/apps/tenant.yaml" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: generated
spec:
  name: generated
YAML
  printf '  - tenant.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal 'a matching instance still refuses a dormant nested-reference carrier' "$root" 'production renders 1 Tenant instance(s)' 'kro turns each into an OCIRepository'
  root="$(fixture unbounded-nested-source-rgd)"
  cp "$WORK/dormant-nested-source-rgd/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml" "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
  yq -i 'del(.spec.resources[0])' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
  expect_refusal 'a Kustomization-only RGD has no existing OCI-instance bound' "$root" 'nested source reference' 'UNKNOWN'
}

regression_runtime_chain_findings() {
  local root target placement want boundary
  for target in ResourceSet ResourceGraphDefinition ClusterPolicy Policy MutatingPolicy GeneratingPolicy MutatingAdmissionPolicy MutatingWebhookConfiguration; do
    for placement in match targets generate clone; do
      root="$(fixture "runtime-carrier-$target-$placement")"
      cat >"$root/k8s/providers/prod/apps/policy.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: runtime-carrier
spec:
  rules:
    - name: rewrite
      match:
        any:
          - resources:
              kinds: [ConfigMap]
      mutate:
        patchesJson6902: |-
          - op: add
            path: /spec/resourcesTemplate
            value: "apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nspec: {}"
YAML
      want='admission mutation'
      case "$placement" in
        match) TARGET_KIND="$target" yq -i '.spec.rules[0].match.any[0].resources.kinds = [strenv(TARGET_KIND)]' "$root/k8s/providers/prod/apps/policy.yaml" ;;
        targets) TARGET_KIND="$target" yq -i '.spec.rules[0].mutate.targets = [{"apiVersion":"example.test/v1","kind":strenv(TARGET_KIND)}]' "$root/k8s/providers/prod/apps/policy.yaml" ;;
        generate) TARGET_KIND="$target" yq -i 'del(.spec.rules[0].mutate) | .spec.rules[0].generate = {"apiVersion":"example.test/v1","kind":strenv(TARGET_KIND),"name":"carrier","data":{"spec":{}}}' "$root/k8s/providers/prod/apps/policy.yaml"; want='consumer generation' ;;
        clone) TARGET_KIND="$target" yq -i 'del(.spec.rules[0].mutate) | .spec.rules[0].generate.cloneList = {"kinds":["example.test/v1/" + strenv(TARGET_KIND)],"namespace":"existing"}' "$root/k8s/providers/prod/apps/policy.yaml"; want='consumer generation' ;;
      esac
      printf '  - policy.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      expect_refusal "$placement cannot hide a runtime $target carrier" "$root" "$want" 'UNKNOWN'
    done
  done

  for placement in resources steps substituted-key; do
    root="$(fixture "recursive-resource-set-$placement")"
    cat >"$root/k8s/providers/prod/apps/carrier.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: outer
spec:
  resources:
    - apiVersion: fluxcd.controlplane.io/v1
      kind: ResourceSet
      metadata:
        name: inner
      spec:
        resourcesTemplate: |-
          apiVersion: source.toolkit.fluxcd.io/v1
          kind: OCIRepository
          spec: {}
YAML
    want='resourcesTemplate'
    case "$placement" in
      steps) yq -i '.spec.resources[0].spec.steps = [{"name":"source","resourcesTemplate":.spec.resources[0].spec.resourcesTemplate}] | del(.spec.resources[0].spec.resourcesTemplate)' "$root/k8s/providers/prod/apps/carrier.yaml" ;;
      substituted-key)
        # shellcheck disable=SC2016 # Flux substitutes this mapping key after the static build.
        yq -i '.spec.resources[0].spec["${TEMPLATE_FIELD}"] = .spec.resources[0].spec.resourcesTemplate | del(.spec.resources[0].spec.resourcesTemplate)' "$root/k8s/providers/prod/apps/carrier.yaml"
        yq -i '.spec.postBuild.substitute.TEMPLATE_FIELD = "resourcesTemplate"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
        want='mapping key' ;;
    esac
    printf '  - carrier.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "a nested ResourceSet $placement string cannot escape the census" "$root" "$want" 'UNKNOWN'
  done
  root="$(fixture substituted-policy-key)"
  cp "$WORK/runtime-carrier-ResourceSet-match/k8s/providers/prod/apps/policy.yaml" "$root/k8s/providers/prod/apps/policy.yaml"
  # shellcheck disable=SC2016 # A controller-significant key decided by Flux substitution.
  yq -i '.spec.rules[0]["${MUTATE_FIELD}"] = .spec.rules[0].mutate | del(.spec.rules[0].mutate)' "$root/k8s/providers/prod/apps/policy.yaml"
  yq -i '.spec.postBuild.substitute.MUTATE_FIELD = "mutate"' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
  printf '  - policy.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal 'a substituted mutation key cannot hide a policy rule' "$root" 'mapping key' 'UNKNOWN'

  for target in exact custom-group kind-only version-kind foreach foreach-source foreign-group foreign-version unrelated-foreach malformed-match malformed-clone empty-foreach unknown-foreach nested-foreach cel; do
    root="$(fixture "clone-schema-$target")"
    cat >"$root/k8s/providers/prod/apps/generate.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: clone-tenants
spec:
  rules:
    - name: clone
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        cloneList:
          kinds: [kro.run/v1alpha1/Tenant]
          namespace: existing
YAML
    want='consumer-producing schema'
    case "$target" in
      custom-group) yq -i '.spec.schema.group = "tenants.example.test"' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"; yq -i '.spec.rules[0].generate.cloneList.kinds = ["tenants.example.test/v1alpha1/Tenant"]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      kind-only) yq -i '.spec.rules[0].generate.cloneList.kinds = ["Tenant"]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      version-kind) yq -i '.spec.rules[0].generate.cloneList.kinds = ["v1alpha1/Tenant"]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      foreach|foreach-source) yq -i '.spec.rules[0].generate = {"foreach":[{"list":"request.object.metadata.labels","cloneList":.spec.rules[0].generate.cloneList}]}' "$root/k8s/providers/prod/apps/generate.yaml"
        if [ "$target" = foreach-source ]; then yq -i '.spec.rules[0].generate.foreach[0].cloneList.kinds = ["source.toolkit.fluxcd.io/v1/OCIRepository"]' "$root/k8s/providers/prod/apps/generate.yaml"; want='consumer generation'; fi ;;
      foreign-group) yq -i '.spec.rules[0].generate.cloneList.kinds = ["other.example.test/v1alpha1/Tenant"]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      foreign-version) yq -i '.spec.rules[0].generate.cloneList.kinds = ["kro.run/v9/Tenant"]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      unrelated-foreach) yq -i '.spec.rules[0].generate = {"foreach":[{"list":"request.object.metadata.labels","apiVersion":"v1","kind":"ConfigMap","name":"ordinary","data":{"data":{"ordinary":"value"}}}]}' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      malformed-match) yq -i '.spec.rules[0].generate = null | .spec.rules[0].mutate.patchStrategicMerge.metadata.labels.ordinary = "value" | .spec.rules[0].match.any[0].resources.kinds = {"bad":"Pod"} | del(.spec.rules[0].generate)' "$root/k8s/providers/prod/apps/generate.yaml"; want='admission mutation' ;;
      malformed-clone) yq -i '.spec.rules[0].generate.cloneList.kinds = {"bad":"v1/ConfigMap"}' "$root/k8s/providers/prod/apps/generate.yaml"; want='consumer generation' ;;
      empty-foreach) yq -i '.spec.rules[0].generate = {"foreach":[]}' "$root/k8s/providers/prod/apps/generate.yaml"; want='consumer generation' ;;
      unknown-foreach) yq -i '.spec.rules[0].generate = {"foreach":[{"list":"request.object.metadata.labels","data":{}}]}' "$root/k8s/providers/prod/apps/generate.yaml"; want='consumer generation' ;;
      nested-foreach) yq -i '.spec.rules[0].generate = {"foreach":[{"foreach":[{"kind":"ConfigMap"}]}]}' "$root/k8s/providers/prod/apps/generate.yaml"; want='consumer generation' ;;
      cel) yq -i '.apiVersion = "policies.kyverno.io/v1" | .kind = "GeneratingPolicy" | .spec = {"generation":[{"expression":"generator.Apply(\"v1\", \"configmaps\", \"default\", [])"}]}' "$root/k8s/providers/prod/apps/generate.yaml"; want='unevaluated CEL' ;;
    esac
    printf '  - generate.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    case "$target" in
      foreign-group|foreign-version|unrelated-foreach) expect_pass "a bounded $target generator does not match a consumer schema" "$root" '2 consumer(s)' ;;
      *) expect_refusal "$target generation cannot hide an unseen consumer instance" "$root" "$want" 'UNKNOWN' ;;
    esac
  done
  for boundary in clone templates; do
    root="$(fixture "partial-runtime-$boundary")"
    cp "$WORK/clone-schema-foreign-group/k8s/providers/prod/apps/generate.yaml" "$root/k8s/providers/prod/apps/generate.yaml"
    printf '  - generate.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    mkdir "$WORK/partial-runtime-$boundary-bin"
    cat >"$WORK/partial-runtime-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_RUNTIME:$*" in
  clone:*'CONSUMER_GVK_SELECTOR'*) exit 2 ;;
  templates:*'and .kind == "ResourceSet"'*'resourcesTemplate'*) exit 2 ;;
esac
SH
    chmod +x "$WORK/partial-runtime-$boundary-bin/yq"
    case "$boundary" in clone) want='could not bound cloning of Tenant';; templates) want='could not read ResourceSet resourcesTemplate';; esac
    REAL_YQ="$(command -v yq)" PARTIAL_RUNTIME="$boundary" PATH="$WORK/partial-runtime-$boundary-bin:$PATH" \
      expect_refusal "partial $boundary reader cannot clear a runtime chain" "$root" "$want" 'UNKNOWN'
  done
}

regression_controller_template_findings() {
  local root field placement want
  for placement in resources steps nested; do
    for field in canonical alternate; do
      root="$(fixture "nested-instance-$placement-$field")"
      cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: root-factory
spec:
  inputs:
    - id: root
  resources:
    - apiVersion: fluxcd.controlplane.io/v1
      kind: FluxInstance
      metadata:
        name: flux
        namespace: flux-system
      spec:
        distribution:
          version: 2.8.x
          registry: ghcr.io/fluxcd
        components: [source-controller, kustomize-controller]
        sync:
          kind: OCIRepository
          url: oci://ghcr.io/devantler-tech/platform/manifests
          ref: latest
          path: clusters/prod
YAML
      if [ "$field" = alternate ]; then yq -i '.spec.resources[0].spec.sync.path = "bases/unseen"' "$root/k8s/providers/prod/apps/resource-set.yaml"; fi
      case "$placement" in
        steps) yq -i '.spec.steps = [{"name":"root","resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        nested) yq -i '.spec = {"resources":[{"apiVersion":"fluxcd.controlplane.io/v1","kind":"ResourceSet","metadata":{"name":"inner"},"spec":.spec}]}' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
      esac
      printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      expect_refusal "a $placement FluxInstance $field sync cannot create unseen roots" "$root" 'nested FluxInstance' 'UNKNOWN'
    done
  done
  root="$(fixture dormant-nested-flux-instance)"
  yq -i '.spec.resources += [{"id":"flux","template":{"apiVersion":"fluxcd.controlplane.io/v1","kind":"FluxInstance","metadata":{"name":"generated","namespace":"generated"},"spec":{"distribution":{"version":"2.8.x","registry":"ghcr.io/fluxcd"},"sync":{"kind":"OCIRepository","url":"oci://ghcr.io/devantler-tech/platform/manifests","ref":"latest","path":"bases/unseen"}}}}]' "$root/k8s/bases/infrastructure/tenant-rgd/resource-graph-definition.yaml"
  expect_pass 'a complete OCI-producing kro schema keeps dormant FluxInstance templates' "$root" '2 consumer(s)'
  cat >"$root/k8s/providers/prod/apps/tenant.yaml" <<'YAML'
apiVersion: kro.run/v1alpha1
kind: Tenant
metadata:
  name: generated
spec:
  name: generated
YAML
  printf '  - tenant.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_refusal 'a matching instance still refuses the dormant FluxInstance carrier' "$root" 'production renders 1 Tenant instance(s)'
  for field in mutation clone native-group native-resource; do
    root="$(fixture "resource-set-policy-$field")"
    cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: policy-templates
spec:
  inputs:
    - id: policy
      affectedKind: OCIRepository
      affectedGroup: source.toolkit.fluxcd.io
      affectedResource: ocirepositories
      schema: kro.run/v1alpha1/Tenant
  resources:
    - apiVersion: kyverno.io/v1
      kind: ClusterPolicy
      metadata:
        name: controller-template
      spec:
        rules:
          - name: runtime
            match:
              any:
                - resources:
                    kinds: ['<< inputs.affectedKind >>']
            mutate:
              patchesJson6902: |-
                - op: replace
                  path: /spec/url
                  value: oci://ghcr.io/devantler-tech/changed/manifests
YAML
    want='admission mutation'
    case "$field" in
      clone) yq -i 'del(.spec.resources[0].spec.rules[0].mutate) | .spec.resources[0].spec.rules[0].match.any[0].resources.kinds = ["Namespace"] | .spec.resources[0].spec.rules[0].generate.cloneList = {"kinds":["<< inputs.schema >>"],"namespace":"existing"}' "$root/k8s/providers/prod/apps/resource-set.yaml"; want='consumer generation' ;;
      native-group|native-resource) yq -i '.spec.resources[0] = {"apiVersion":"admissionregistration.k8s.io/v1","kind":"MutatingWebhookConfiguration","metadata":{"name":"runtime"},"webhooks":[{"name":"runtime.example.test","clientConfig":{"url":"https://webhook.example.test/"},"admissionReviewVersions":["v1"],"sideEffects":"None","rules":[{"apiGroups":["source.toolkit.fluxcd.io"],"apiVersions":["v1"],"operations":["CREATE","UPDATE"],"resources":["ocirepositories"]}]}]}' "$root/k8s/providers/prod/apps/resource-set.yaml"
        if [ "$field" = native-group ]; then yq -i '.spec.resources[0].webhooks[0].rules[0].apiGroups = ["<< inputs.affectedGroup >>"]' "$root/k8s/providers/prod/apps/resource-set.yaml"; else yq -i '.spec.resources[0].webhooks[0].rules[0].resources = ["<< inputs.affectedResource >>"]' "$root/k8s/providers/prod/apps/resource-set.yaml"; fi
        want='native admission mutation' ;;
    esac
    printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    expect_refusal "a ResourceSet policy $field contract must remain literal" "$root" "$want" 'UNKNOWN'
  done
  for placement in resources steps nested; do
    for field in kind api-version source-kind source-name source-namespace inherited-namespace key; do
      root="$(fixture "controller-template-$placement-$field")"
      cat >"$root/k8s/providers/prod/apps/resource-set.yaml" <<'YAML'
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata:
  name: runtime-templates
spec:
  inputs:
    - id: hidden
      sourceKind: OCIRepository
      sourceName: flux-system
      namespace: flux-system
      group: kro.run
      resourceKind: Tenant
      typeField: kind
  resources:
    - apiVersion: kustomize.toolkit.fluxcd.io/v1
      kind: Kustomization
      metadata:
        name: unseen
        namespace: flux-system
      spec:
        path: bases/unseen
        sourceRef:
          kind: OCIRepository
          name: alpha
YAML
      want='nested source reference'
      case "$field" in
        kind) yq -i '.spec.resources[0].apiVersion = "kro.run/v1alpha1" | .spec.resources[0].kind = "<< inputs.resourceKind >>" | .spec.resources[0].spec = {"name":"generated"}' "$root/k8s/providers/prod/apps/resource-set.yaml"; want='ResourceSet object types' ;;
        api-version) yq -i '.spec.resources[0].kind = "Tenant" | .spec.resources[0].apiVersion = "<< inputs.group >>/v1alpha1" | .spec.resources[0].spec = {"name":"generated"}' "$root/k8s/providers/prod/apps/resource-set.yaml"; want='ResourceSet object types' ;;
        source-kind) yq -i '.spec.resources[0].spec.sourceRef = {"kind":"<< inputs.sourceKind >>","name":"flux-system"}' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        source-name) yq -i '.spec.resources[0].spec.sourceRef.name = "<< inputs.sourceName >>"' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        source-namespace) yq -i '.spec.resources[0].spec.sourceRef.name = "flux-system" | .spec.resources[0].spec.sourceRef.namespace = "<< inputs.namespace >>"' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        inherited-namespace) yq -i '.spec.resources[0].spec.sourceRef.name = "flux-system" | .spec.resources[0].metadata.namespace = "<< inputs.namespace >>"' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        key) yq -i '.spec.resources[0]["<< inputs.typeField >>"] = .spec.resources[0].kind | del(.spec.resources[0].kind)' "$root/k8s/providers/prod/apps/resource-set.yaml"; want='mapping key' ;;
      esac
      case "$placement" in
        steps) yq -i '.spec.steps = [{"name":"sources","resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
        nested) yq -i '.spec = {"resources":[{"apiVersion":"fluxcd.controlplane.io/v1","kind":"ResourceSet","metadata":{"name":"inner"},"spec":.spec}]}' "$root/k8s/providers/prod/apps/resource-set.yaml" ;;
      esac
      printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
      expect_refusal "$placement $field runtime selection cannot hide consumers" "$root" "$want" 'UNKNOWN'
    done
  done
  for placement in direct foreach owner-references; do
    root="$(fixture "generated-object-template-$placement")"
    cat >"$root/k8s/providers/prod/apps/generate.yaml" <<'YAML'
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: runtime-group
spec:
  rules:
    - name: generate
      match:
        any:
          - resources:
              kinds: [Namespace]
      generate:
        apiVersion: '{{ request.object.metadata.labels.group }}/v1alpha1'
        kind: Tenant
        name: generated
        data:
          spec:
            name: generated
YAML
    case "$placement" in
      foreach) yq -i '.spec.rules[0].generate = {"foreach":[.spec.rules[0].generate + {"list":"request.object.metadata.labels"}]}' "$root/k8s/providers/prod/apps/generate.yaml" ;;
      owner-references) yq -i '.spec.rules[0].generate.apiVersion = "autoscaling.k8s.io/v1" | .spec.rules[0].generate.kind = "VerticalPodAutoscaler" |
        .spec.rules[0].generate.data.metadata.ownerReferences = [{"apiVersion":"{{request.object.apiVersion}}","kind":"{{request.object.kind}}","name":"{{request.object.metadata.name}}","uid":"{{request.object.metadata.uid}}"}]' "$root/k8s/providers/prod/apps/generate.yaml" ;;
    esac
    printf '  - generate.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    if [ "$placement" = owner-references ]; then
      expect_pass 'literal generated GVKs retain templated workload owner references' "$root" '2 consumer(s)'
    else
      expect_refusal "$placement generation needs a literal complete object GVK" "$root" 'consumer generation' 'UNKNOWN'
    fi
  done
  root="$(fixture resource-set-owner-references)"
  cp "$WORK/controller-template-resources-kind/k8s/providers/prod/apps/resource-set.yaml" "$root/k8s/providers/prod/apps/resource-set.yaml"
  yq -i '.spec.resources[0].apiVersion = "v1" | .spec.resources[0].kind = "ConfigMap" | .spec.resources[0].spec = null | del(.spec.resources[0].spec) |
    .spec.resources[0].data = {"ordinary":"<< inputs.id >>"} |
    .spec.resources[0].metadata.ownerReferences = [{"apiVersion":"<< inputs.apiVersion >>","kind":"<< inputs.kind >>","name":"<< inputs.name >>","uid":"<< inputs.uid >>"}]' "$root/k8s/providers/prod/apps/resource-set.yaml"
  printf '  - resource-set.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'literal ResourceSet object GVKs retain templated metadata references and data' "$root" '2 consumer(s)'
  root="$(fixture partial-resource-set-types)"
  mkdir "$WORK/partial-resource-set-types-bin"
  cat >"$WORK/partial-resource-set-types-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$*" in *'strenv(CONSUMER_LITERAL_KIND)'*'ResourceSet'*) exit 2 ;; *'ResourceSet'*'strenv(CONSUMER_LITERAL_KIND)'*) exit 2 ;; esac
SH
  chmod +x "$WORK/partial-resource-set-types-bin/yq"
  REAL_YQ="$(command -v yq)" PATH="$WORK/partial-resource-set-types-bin:$PATH" \
    expect_refusal 'partial ResourceSet type output cannot attest a complete census' "$root" 'could not read ResourceSet object types' 'UNKNOWN'
}

regression_platform_source_aliases() {
  local placement root source_file namespace url boundary want escaped_path escaped_name
  for placement in direct cross-root reverse-root overlay resources steps default-namespace explicit-namespace unknown-namespace trailing-slash unresolved-url; do
    root="$(fixture "platform-alias-$placement")"
    mkdir -p "$root/k8s/bases/hidden"
    cp "$root/k8s/bases/apps/alpha/oci-repository.yaml" "$root/k8s/bases/hidden/consumer.yml"
    yq -i '.metadata.name = "hidden"' "$root/k8s/bases/hidden/consumer.yml"
    printf 'resources:\n  - consumer.yml\n' >"$root/k8s/bases/hidden/kustomization.yaml"
    source_file="$root/k8s/providers/prod/apps/platform-alias.yaml"
    namespace=flux-system
    url=oci://ghcr.io/devantler-tech/platform/manifests
    case "$placement" in
      cross-root|reverse-root) source_file="$root/k8s/providers/prod/infrastructure/platform-alias.yaml" ;;
      overlay) source_file="$root/k8s/clusters/prod/platform-alias.yaml" ;;
      explicit-namespace) namespace=other ;;
      trailing-slash) url="$url/" ;;
      unresolved-url)
        # shellcheck disable=SC2016 # Flux resolves this literal variable, not the shell.
        url='oci://ghcr.io/${REPOSITORY}/manifests' ;;
    esac
    cat >"$source_file" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: platform-extra
  namespace: $namespace
spec:
  interval: 1m
  url: $url
YAML
    printf '  - platform-alias.yaml\n' >>"${source_file%/*}/kustomization.yaml"
    if [ "$placement" = unknown-namespace ]; then
      yq -i 'del(.metadata.namespace)' "$source_file"
    fi
    cat >"$root/k8s/providers/prod/apps/extra-root.yaml" <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: extra
  namespace: flux-system
spec:
  interval: 1m
  path: bases/hidden
  prune: true
  sourceRef:
    kind: OCIRepository
    name: platform-extra
YAML
    case "$placement" in
      reverse-root) yq eval-all -i '[.] | reverse | .[] | split_doc' "$root/k8s/clusters/prod/flux-kustomizations.yaml" ;;
      default-namespace) yq -i 'del(.metadata.namespace)' "$root/k8s/providers/prod/apps/extra-root.yaml" ;;
      explicit-namespace) yq -i '.spec.sourceRef.namespace = "other"' "$root/k8s/providers/prod/apps/extra-root.yaml" ;;
      resources|steps)
        yq -i '{"apiVersion":"fluxcd.controlplane.io/v1","kind":"ResourceSet","metadata":{"name":"roots","namespace":"flux-system"},"spec":{"resources":[.]}}' "$root/k8s/providers/prod/apps/extra-root.yaml"
        if [ "$placement" = steps ]; then
          yq -i '.spec.steps = [{"resources":.spec.resources}] | del(.spec.resources)' "$root/k8s/providers/prod/apps/extra-root.yaml"
        fi ;;
    esac
    printf '  - extra-root.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    if [ "$placement" = unresolved-url ]; then
      expect_refusal 'an unresolved artifact URL cannot hide a yml consumer' "$root" 'Flux substitution'
    else
      expect_refusal "a $placement platform-artifact alias cannot hide a yml consumer" "$root" 'platform-artifact source' 'UNKNOWN'
    fi
  done
  root="$(fixture unused-platform-alias)"
  cp "$WORK/platform-alias-direct/k8s/providers/prod/apps/platform-alias.yaml" "$root/k8s/providers/prod/apps/platform-alias.yaml"
  printf '  - platform-alias.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'an unused platform alias creates no unseen root' "$root" '2 consumer(s)'
  root="$(fixture foreign-platform-alias)"
  cp "$WORK/platform-alias-direct/k8s/providers/prod/apps/platform-alias.yaml" "$root/k8s/providers/prod/apps/platform-alias.yaml"
  cp "$WORK/platform-alias-direct/k8s/providers/prod/apps/extra-root.yaml" "$root/k8s/providers/prod/apps/extra-root.yaml"
  yq -i '.spec.url = "oci://ghcr.io/devantler-tech/another/manifests"' "$root/k8s/providers/prod/apps/platform-alias.yaml"
  printf '  - platform-alias.yaml\n  - extra-root.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a literal external-artifact alias stays outside this checkout' "$root" '2 consumer(s)'
  root="$(fixture other-namespace-platform-alias)"
  cp "$WORK/foreign-platform-alias/k8s/providers/prod/apps/platform-alias.yaml" "$root/k8s/providers/prod/apps/platform-alias.yaml"
  cp "$WORK/foreign-platform-alias/k8s/providers/prod/apps/extra-root.yaml" "$root/k8s/providers/prod/apps/extra-root.yaml"
  yq -i '.spec.url = "oci://ghcr.io/devantler-tech/platform/manifests"' "$root/k8s/providers/prod/apps/platform-alias.yaml"
  yq -i '.metadata.namespace = "another"' "$root/k8s/providers/prod/apps/extra-root.yaml"
  printf '  - platform-alias.yaml\n  - extra-root.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
  expect_pass 'a literal unrelated namespace does not match an artifact alias' "$root" '2 consumer(s)'

  # Root labels are path data. Shell escape decoding must neither drop a later
  # render nor change the label of the render that follows an artifact alias.
  for escaped_name in 'apps\c-tail' 'apps\t-tail' 'apps\n-tail'; do
    escaped_path="providers/prod/$escaped_name"
    root="$(fixture "literal-alias-$escaped_name")"
    cp "$WORK/platform-alias-cross-root/k8s/providers/prod/infrastructure/platform-alias.yaml" "$root/k8s/providers/prod/infrastructure/platform-alias.yaml"
    cp "$WORK/platform-alias-cross-root/k8s/providers/prod/apps/extra-root.yaml" "$root/k8s/providers/prod/apps/extra-root.yaml"
    printf '  - platform-alias.yaml\n' >>"$root/k8s/providers/prod/infrastructure/kustomization.yaml"
    printf '  - extra-root.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    mv "$root/k8s/providers/prod/apps" "$root/k8s/$escaped_path"
    LITERAL_ROOT="$escaped_path" yq -i '(select(.metadata.name == "apps") | .spec.path) = strenv(LITERAL_ROOT)' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
    expect_refusal "a literal $escaped_name root cannot omit an artifact alias" "$root" "production render $escaped_path" 'platform-artifact source' 'UNKNOWN'

    root="$(fixture "literal-unused-$escaped_name")"
    cp "$WORK/unused-platform-alias/k8s/providers/prod/apps/platform-alias.yaml" "$root/k8s/providers/prod/apps/platform-alias.yaml"
    printf '  - platform-alias.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    mv "$root/k8s/providers/prod/apps" "$root/k8s/$escaped_path"
    LITERAL_ROOT="$escaped_path" yq -i '(select(.metadata.name == "apps") | .spec.path) = strenv(LITERAL_ROOT)' "$root/k8s/clusters/prod/flux-kustomizations.yaml"
    expect_pass "an unused alias in a literal $escaped_name root remains valid" "$root" '2 consumer(s)'
  done

  # A reader can print complete-looking rows and then fail. Neither alias
  # collection nor reference collection may turn that partial output into PASS.
  for boundary in aliases references; do
    root="$(fixture "partial-platform-$boundary")"
    cp "$WORK/unused-platform-alias/k8s/providers/prod/apps/platform-alias.yaml" "$root/k8s/providers/prod/apps/platform-alias.yaml"
    printf '  - platform-alias.yaml\n' >>"$root/k8s/providers/prod/apps/kustomization.yaml"
    mkdir "$WORK/partial-platform-$boundary-bin"
    cat >"$WORK/partial-platform-$boundary-bin/yq" <<'SH'
#!/usr/bin/env bash
"$REAL_YQ" "$@" || exit $?
case "$PARTIAL_PLATFORM:$*" in
  aliases:*'sub("/+$", "")'*) exit 2 ;;
  references:*'strenv(CONSUMER_ALIAS_INCLUDE_ROOT)'*) exit 2 ;;
esac
SH
    chmod +x "$WORK/partial-platform-$boundary-bin/yq"
    want='could not read platform-artifact source aliases'
    [ "$boundary" != references ] || want='could not read platform-artifact source alias references'
    REAL_YQ="$(command -v yq)" PARTIAL_PLATFORM="$boundary" PATH="$WORK/partial-platform-$boundary-bin:$PATH" \
      expect_refusal "partial platform $boundary output is not an attestation" "$root" "$want" 'UNKNOWN'
  done
}

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = inline-documents ]; then
  regression_inline_document_bounds
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = inline-replacements ]; then
  regression_inline_replacement_paths
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = legacy-loaders ]; then
  regression_builtin_legacy_paths
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = loaders ]; then
  regression_published_loader_inputs
  regression_builtin_legacy_paths
  regression_inline_replacement_paths
  regression_inline_document_bounds
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = closure ]; then
  regression_published_closure
  regression_published_loader_inputs
  regression_builtin_legacy_paths
  regression_inline_replacement_paths
  regression_inline_document_bounds
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = identity ]; then
  regression_consumer_identities
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = aliases ]; then
  regression_platform_source_aliases
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = templates ]; then
  regression_controller_template_findings
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = chains ]; then
  regression_runtime_chain_findings
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = native ]; then
  regression_native_admission_findings
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

if [ "${CONSUMER_CONSERVATION_REGRESSION:-}" = controller ]; then
  regression_controller_findings
  printf '\n%d failure(s)\n' "$failures"
  [ "$failures" -eq 0 ]
  exit
fi

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
case "$3" in *'| (.kind // "" | tostring)'*) exit 2 ;; esac
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

regression_published_closure
regression_published_loader_inputs
regression_builtin_legacy_paths
regression_inline_replacement_paths
regression_inline_document_bounds
regression_consumer_identities
regression_latest_findings
regression_controller_findings
regression_native_admission_findings
regression_runtime_chain_findings
regression_controller_template_findings
regression_platform_source_aliases

printf '\n%d failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
