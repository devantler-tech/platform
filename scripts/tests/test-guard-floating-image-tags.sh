#!/usr/bin/env bash
#
# Pins the floating-image-tag guard's verdict in all THREE directions (#3755).
#
#   exit 0  every rendered image is digest-pinned or names a non-floating tag
#   exit 1  an unexcepted floating image, or a stale exception row
#   exit 2  the guard could not check
#
# The first case runs the guard against the REAL committed tree and exception list,
# so a floating tag merged anywhere the render reaches fails here. Every other case
# is a fixture tree that isolates one condition.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-floating-image-tags.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <k8s-root> [exceptions-file]
  local exceptions="${2:-$scratch/no-exceptions.tsv}"
  if GUARD_OUT="$(FLOATING_IMAGE_TAG_EXCEPTIONS="$exceptions" "$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

assert_rc() { # <label> <expected-rc>
  assertions=$((assertions + 1))
  if [ "$2" = "$GUARD_RC" ]; then
    printf '  ok   %s (exit %s)\n' "$1" "$GUARD_RC"
  else
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$GUARD_RC"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  # A here-string, not a pipe: under pipefail an early `grep -q` match SIGPIPEs the
  # writer and inverts the verdict.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: output did not contain %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

: >"$scratch/no-exceptions.tsv"
digest="sha256:$(printf 'a%.0s' $(seq 1 64))"
tree_count=0

# A tree with one cluster overlay whose Flux Kustomization points at apps/test, which
# renders workload.yaml. Echoes the k8s root.
tree() {
  tree_count=$((tree_count + 1))
  local root="$scratch/tree-$tree_count/k8s"
  mkdir -p "$root/clusters/test" "$root/apps/test"
  printf '%s\n' \
    'apiVersion: kustomize.config.k8s.io/v1beta1' \
    'kind: Kustomization' \
    'resources:' \
    '  - flux-kustomization.yaml' >"$root/clusters/test/kustomization.yaml"
  printf '%s\n' \
    'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
    'kind: Kustomization' \
    'metadata:' \
    '  name: apps' \
    '  namespace: flux-system' \
    'spec:' \
    '  path: ./apps/test' >"$root/clusters/test/flux-kustomization.yaml"
  printf '%s\n' \
    'apiVersion: kustomize.config.k8s.io/v1beta1' \
    'kind: Kustomization' \
    'resources:' \
    '  - workload.yaml' >"$root/apps/test/kustomization.yaml"
  printf '%s' "$root"
}

deployment() { # <root> <image>
  printf '%s\n' \
    'apiVersion: apps/v1' \
    'kind: Deployment' \
    'metadata:' \
    '  name: web' \
    '  namespace: demo' \
    'spec:' \
    '  selector:' \
    '    matchLabels: {app: web}' \
    '  template:' \
    '    metadata:' \
    '      labels: {app: web}' \
    '    spec:' \
    '      containers:' \
    '        - name: web' \
    "          image: '$2'" >"$1/apps/test/workload.yaml"
}

echo "== the committed tree renders no floating image =="
run_guard "$repo_root/k8s" "$repo_root/scripts/floating-image-tag-exceptions.tsv"
assert_rc "committed tree and exception list" 0

echo "== accepted images =="
for image in "nginx:main@$digest" "nginx@$digest" 'nginx:1.27.0' 'localhost:5000/team/app:1.2.3' 'ghcr.io/org/app:v2.0.0-rc.1' 'postgres:17.2-alpine' 'app:v1' 'app:2026.09.13'; do
  root="$(tree)"
  deployment "$root" "$image"
  run_guard "$root"
  assert_rc "accepts $image" 0
done

echo "== floating images are refused =="
# The channel names below are the ones no fixed name list anticipates: only a tag
# that looks like a version passes.
for image in nginx:main nginx:master nginx:latest nginx:edge nginx:nightly nginx:dev nginx:develop nginx localhost:5000/team/app \
  nginx:stable nginx:canary nginx:release nginx:prod app:feature-branch app:v app:1.2.x; do
  root="$(tree)"
  deployment "$root" "$image"
  run_guard "$root"
  assert_rc "refuses $image" 1
done
root="$(tree)"
deployment "$root" nginx:main
run_guard "$root"
assert_rc "names the offender" 1
assert_contains "names the workload" "Deployment/demo/web"
assert_contains "names the image" "floating image nginx:main"
assert_contains "names the fix" "Pin each image by digest"

echo "== a floating init container is refused =="
root="$(tree)"
printf '%s\n' \
  'apiVersion: apps/v1' \
  'kind: StatefulSet' \
  'metadata:' \
  '  name: db' \
  '  namespace: demo' \
  'spec:' \
  '  serviceName: db' \
  '  selector:' \
  '    matchLabels: {app: db}' \
  '  template:' \
  '    metadata:' \
  '      labels: {app: db}' \
  '    spec:' \
  '      initContainers:' \
  '        - name: migrate' \
  '          image: ghcr.io/org/migrate:edge' \
  '      containers:' \
  '        - name: db' \
  "          image: 'postgres:17.2@$digest'" >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "floating init container" 1
assert_contains "names the init image" "StatefulSet/demo/db runs floating image ghcr.io/org/migrate:edge"

echo "== a floating CronJob container is refused =="
root="$(tree)"
printf '%s\n' \
  'apiVersion: batch/v1' \
  'kind: CronJob' \
  'metadata:' \
  '  name: prune' \
  '  namespace: demo' \
  'spec:' \
  '  schedule: "0 * * * *"' \
  '  jobTemplate:' \
  '    spec:' \
  '      template:' \
  '        spec:' \
  '          restartPolicy: Never' \
  '          containers:' \
  '            - name: prune' \
  '              image: busybox:latest' >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "floating CronJob container" 1

echo "== a HelmRelease-free vendored bundle is read =="
root="$(tree)"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: ServiceAccount' \
  'metadata:' \
  '  name: approver' \
  '  namespace: demo' \
  '---' \
  'apiVersion: apps/v1' \
  'kind: Deployment' \
  'metadata:' \
  '  name: approver' \
  '  namespace: demo' \
  'spec:' \
  '  selector:' \
  '    matchLabels: {app: approver}' \
  '  template:' \
  '    metadata:' \
  '      labels: {app: approver}' \
  '    spec:' \
  '      containers:' \
  '        - name: approver' \
  '          image: ghcr.io/vendor/approver:main' >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "vendored bundle with a floating image" 1
assert_contains "names the vendored workload" "Deployment/demo/approver"

echo "== a floating ReplicationController container is refused =="
root="$(tree)"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: ReplicationController' \
  'metadata:' \
  '  name: legacy' \
  '  namespace: demo' \
  'spec:' \
  '  replicas: 1' \
  '  selector: {app: legacy}' \
  '  template:' \
  '    metadata:' \
  '      labels: {app: legacy}' \
  '    spec:' \
  '      containers:' \
  '        - name: legacy' \
  '          image: nginx:latest' >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "floating ReplicationController container" 1
assert_contains "names the ReplicationController" "ReplicationController/demo/legacy runs floating image nginx:latest"

# The Flux Kustomization, not the bare directory, decides what production receives.
flux_spec() { # <root> <line>... — appends lines under the Flux Kustomization's spec
  local root="$1"
  shift
  printf '%s\n' "$@" >>"$root/clusters/test/flux-kustomization.yaml"
}

echo "== Flux Kustomization images are applied =="
root="$(tree)"
deployment "$root" 'nginx:1.27.0'
flux_spec "$root" '  images:' '    - name: nginx' '      newTag: latest'
run_guard "$root"
assert_rc "a Flux images entry retagging a pinned image to latest" 1
assert_contains "names the retagged image" "floating image nginx:latest"

root="$(tree)"
deployment "$root" nginx:main
flux_spec "$root" '  images:' '    - name: nginx' "      digest: $digest"
run_guard "$root"
assert_rc "a Flux images entry pinning a floating image by digest" 0

echo "== Flux Kustomization patches are applied =="
root="$(tree)"
deployment "$root" 'nginx:1.27.0'
flux_spec "$root" \
  '  patches:' \
  '    - target: {kind: Deployment, name: web}' \
  '      patch: |' \
  '        - op: replace' \
  '          path: /spec/template/spec/containers/0/image' \
  '          value: nginx:canary'
run_guard "$root"
assert_rc "a Flux patch swapping in a floating image" 1
assert_contains "names the patched image" "Deployment/demo/web runs floating image nginx:canary"

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
flux_spec "$root" \
  '  patchesStrategicMerge:' \
  '    - apiVersion: apps/v1' \
  '      kind: Deployment' \
  '      metadata: {name: web, namespace: demo}'
run_guard "$root"
assert_rc "a deprecated Flux patch field this guard does not apply" 2

echo "== Flux Kustomization components are applied =="
root="$(tree)"
deployment "$root" 'nginx:1.27.0'
mkdir -p "$root/components/sidecar"
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1alpha1' \
  'kind: Component' \
  'resources:' \
  '  - pod.yaml' >"$root/components/sidecar/kustomization.yaml"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: Pod' \
  'metadata:' \
  '  name: side' \
  '  namespace: demo' \
  'spec:' \
  '  containers:' \
  '    - name: side' \
  '      image: busybox:stable' >"$root/components/sidecar/pod.yaml"
flux_spec "$root" '  components:' '    - ../../components/sidecar'
run_guard "$root"
assert_rc "a Flux component adding a floating image" 1
assert_contains "names the component workload" "Pod/demo/side runs floating image busybox:stable"

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
# The escaping component exists and renders a clean workload, so only the
# containment check can refuse it.
mkdir -p "$root/../outside-component"
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1alpha1' \
  'kind: Component' \
  'resources:' \
  '  - pod.yaml' >"$root/../outside-component/kustomization.yaml"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: Pod' \
  'metadata:' \
  '  name: outside' \
  '  namespace: demo' \
  'spec:' \
  '  containers:' \
  '    - name: outside' \
  "      image: 'busybox:1.37.0'" >"$root/../outside-component/pod.yaml"
flux_spec "$root" '  components:' '    - ../../../outside-component'
run_guard "$root"
assert_rc "a Flux component that leaves the root" 2

echo "== Flux Kustomization targetNamespace is applied =="
root="$(tree)"
deployment "$root" nginx:main
flux_spec "$root" '  targetNamespace: other'
printf 'Deployment/demo/web\tnginx:main\t#3755\tfixture exception for the untransformed name\n' >"$scratch/pre-namespace.tsv"
run_guard "$root" "$scratch/pre-namespace.tsv"
assert_rc "an exception keyed to the namespace Flux replaces" 1
assert_contains "names the workload in its target namespace" "Deployment/other/web runs floating image nginx:main"

echo "== the clusters/base template is not a cluster =="
root="$(tree)"
deployment "$root" 'nginx:1.27.0'
mkdir -p "$root/clusters/base"
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1beta1' \
  'kind: Kustomization' \
  'resources:' \
  '  - missing.yaml' >"$root/clusters/base/kustomization.yaml"
run_guard "$root"
assert_rc "skips clusters/base" 0

echo "== exceptions =="
root="$(tree)"
deployment "$root" nginx:main
printf 'Deployment/demo/web\tnginx:main\t#3755\tfixture exception\n' >"$scratch/used.tsv"
run_guard "$root" "$scratch/used.tsv"
assert_rc "an excepted floating image" 0

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
run_guard "$root" "$scratch/used.tsv"
assert_rc "a stale exception row" 1
assert_contains "names the stale row" "stale exception: Deployment/demo/web nginx:main"

root="$(tree)"
deployment "$root" nginx:main
printf 'Deployment/demo/other\tnginx:main\t#3755\tnames another workload\n' >"$scratch/other.tsv"
run_guard "$root" "$scratch/other.tsv"
assert_rc "an exception for another workload does not cover this one" 1

printf 'Deployment/demo/web\tnginx:main\t\tno issue\n' >"$scratch/no-issue.tsv"
run_guard "$root" "$scratch/no-issue.tsv"
assert_rc "an exception row without an issue" 2

printf 'Deployment/demo/web\tnginx:main\t#3755\t\n' >"$scratch/no-reason.tsv"
run_guard "$root" "$scratch/no-reason.tsv"
assert_rc "an exception row without a reason" 2

printf 'Deployment/demo/web nginx:main #3755 spaces, not tabs\n' >"$scratch/no-tabs.tsv"
run_guard "$root" "$scratch/no-tabs.tsv"
assert_rc "an exception row without tab separators" 2

run_guard "$root" "$scratch/does-not-exist.tsv"
assert_rc "a missing exception list" 2

echo "== images that cannot be classified =="
# shellcheck disable=SC2016 # a literal `${IMAGE}`: an unresolved Flux substitution
for image in '${IMAGE}' 'nginx@sha256:abc' 'nginx@sha512:abc'; do
  root="$(tree)"
  deployment "$root" "$image"
  run_guard "$root"
  assert_rc "cannot classify $image" 2
done
root="$(tree)"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: Pod' \
  'metadata:' \
  '  name: empty' \
  '  namespace: demo' \
  'spec:' \
  '  containers:' \
  '    - name: empty' >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "a container without an image" 2
root="$(tree)"
deployment "$root" 'nginx:1.27.0 extra'
run_guard "$root"
assert_rc "an image line with an extra field" 2

echo "== anti-vacuity and render failures =="
root="$scratch/no-clusters/k8s"
mkdir -p "$root/clusters"
run_guard "$root"
assert_rc "no cluster overlay" 2

run_guard "$scratch/does-not-exist"
assert_rc "a missing root" 2

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1beta1' \
  'kind: Kustomization' \
  'resources: []' >"$root/clusters/test/kustomization.yaml"
run_guard "$root"
assert_rc "an overlay that names no Flux Kustomization" 2

root="$(tree)"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: ConfigMap' \
  'metadata:' \
  '  name: settings' \
  '  namespace: demo' >"$root/apps/test/workload.yaml"
run_guard "$root"
assert_rc "a cluster that renders no workload image" 2

root="$(tree)"
printf '%s\n' \
  'apiVersion: kustomize.config.k8s.io/v1beta1' \
  'kind: Kustomization' \
  'resources:' \
  '  - missing.yaml' >"$root/apps/test/kustomization.yaml"
run_guard "$root"
assert_rc "a Flux path that does not render" 2

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
sed 's|./apps/test|./apps/elsewhere|' "$root/clusters/test/flux-kustomization.yaml" >"$root/clusters/test/flux.tmp"
mv "$root/clusters/test/flux.tmp" "$root/clusters/test/flux-kustomization.yaml"
run_guard "$root"
assert_rc "a Flux path that does not exist" 2

root="$(tree)"
deployment "$root" 'nginx:1.27.0'
# The escaping path exists and renders a clean workload, so only the escape check
# can refuse it.
mkdir -p "$root/../outside"
cp "$root/apps/test/kustomization.yaml" "$root/apps/test/workload.yaml" "$root/../outside/"
sed 's|./apps/test|../outside|' "$root/clusters/test/flux-kustomization.yaml" >"$root/clusters/test/flux.tmp"
mv "$root/clusters/test/flux.tmp" "$root/clusters/test/flux-kustomization.yaml"
run_guard "$root"
assert_rc "a Flux path that leaves the root" 2

echo
if [ "$assertions" -eq 0 ]; then
  echo "no assertions ran" >&2
  exit 1
fi
if [ "$failures" -gt 0 ]; then
  printf '%d of %d assertion(s) failed\n' "$failures" "$assertions" >&2
  exit 1
fi
printf 'all %d assertion(s) passed\n' "$assertions"
