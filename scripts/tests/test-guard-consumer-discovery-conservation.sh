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

printf '\n%d failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
