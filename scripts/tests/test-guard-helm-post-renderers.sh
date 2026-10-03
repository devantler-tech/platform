#!/usr/bin/env bash
#
# Pins the HelmRelease post-renderer guard's verdict in all THREE directions (#3581).
#
#   exit 0  every checked release's post-renderers apply to its chart's rendered output
#   exit 1  a post-renderer does not apply
#   exit 2  the guard could not check
#
# The first case runs the guard against the REAL committed tree with the checkout itself as the
# base, which proves it finds the repository's post-rendered releases and renders none of them when
# nothing changed. Every other case is a fixture tree in its own git repository. Charts come from a
# `helm pull` stand-in that packages a local fixture chart, so no case reaches a registry; `helm
# template` and `kubectl kustomize` are the real tools.
#
# The fixture chart renders its Deployment the way flux-operator 0.50.0 does: a container
# securityContext, and no pod-level securityContext at all. #3577's exact post-renderer, a JSON-6902
# `add` beneath that absent pod-level object, must fail; #3580's, which moved the pod-level field to
# a strategic-merge patch, must pass.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-helm-post-renderers.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

for tool in git helm jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'FAIL: %s is required\n' "$tool" >&2
    exit 1
  }
done

failures=0
assertions=0

# `helm pull` packages the fixture chart named by the reference instead of reaching a registry, and
# records each pull. Every other subcommand runs the real helm.
mkdir -p "$scratch/bin" "$scratch/charts"
export HELM_SHIM_REAL HELM_SHIM_CHARTS="$scratch/charts" HELM_SHIM_LOG="$scratch/pulls.log"
HELM_SHIM_REAL="$(command -v helm)"
cat >"$scratch/bin/helm" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = version ] && [ -n "${HELM_SHIM_VERSION:-}" ]; then
  printf '%s\n' "$HELM_SHIM_VERSION"
  exit 0
fi
if [ "${1:-}" = pull ]; then
  shift
  ref='' repo='' version='' destination=''
  while [ "$#" -gt 0 ]; do
    case $1 in
      --destination) destination="$2"; shift 2 ;;
      --version) version="$2"; shift 2 ;;
      --repo) repo="$2"; shift 2 ;;
      -*) shift ;;
      *) ref="$1"; shift ;;
    esac
  done
  printf '%s|%s|%s\n' "$ref" "$repo" "$version" >>"$HELM_SHIM_LOG"
  name="${ref##*/}"
  name="${name%%@*}"
  [ -d "$HELM_SHIM_CHARTS/$name" ] || { printf 'Error: chart "%s" not found\n' "$name" >&2; exit 1; }
  if [ -n "$version" ]; then
    exec "$HELM_SHIM_REAL" package "$HELM_SHIM_CHARTS/$name" --version "$version" --destination "$destination"
  fi
  exec "$HELM_SHIM_REAL" package "$HELM_SHIM_CHARTS/$name" --destination "$destination"
fi
exec "$HELM_SHIM_REAL" "$@"
EOF
chmod +x "$scratch/bin/helm"

# The fixture chart. Below 0.50.0 it also renders a pod-level securityContext, so a chart bump to
# 0.50.0 removes the parent an unchanged JSON-6902 add relies on.
chart="$scratch/charts/flux-operator"
mkdir -p "$chart/templates"
printf '%s\n' 'apiVersion: v2' 'name: flux-operator' 'version: 0.50.0' >"$chart/Chart.yaml"
printf '%s\n' 'replicas: 1' >"$chart/values.yaml"
cat >"$chart/templates/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ if .Values.useReleaseName }}{{ .Release.Name }}{{ else }}flux-operator{{ end }}
  labels:
    app.kubernetes.io/name: flux-operator
spec:
  replicas: {{ .Values.replicas }}
  selector:
    matchLabels:
      app.kubernetes.io/name: flux-operator
  template:
    metadata:
      labels:
        app.kubernetes.io/name: flux-operator
      {{- with .Values.annotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      {{- if or (semverCompare "<0.50.0" .Chart.Version) (and .Values.podSecurityContextOnInstallOnly (not .Release.IsUpgrade)) }}
      securityContext:
        runAsNonRoot: true
      {{- end }}
      containers:
        - name: manager
          image: ghcr.io/controlplaneio-fluxcd/flux-operator:v0.50.0
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
EOF
cat >"$chart/templates/hook.yaml" <<'EOF'
{{- if .Values.hook }}
apiVersion: batch/v1
kind: Job
metadata:
  name: flux-operator-migrate
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: migrate
          image: ghcr.io/controlplaneio-fluxcd/flux-operator:v0.50.0
{{- end }}
EOF

cp -R "$chart" "$scratch/charts/revision-sensitive"
yq -i '.name = "revision-sensitive"' "$scratch/charts/revision-sensitive/Chart.yaml"
printf '%s\n' '{{ if eq .Release.Revision 1 }}' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: revision-one' '{{ end }}' \
  >"$scratch/charts/revision-sensitive/templates/revision.yaml"

run_guard() { # <tree> [guard-args...]
  local tree="$1"
  shift
  : >"$HELM_SHIM_LOG"
  if GUARD_OUT="$(PATH="$scratch/bin:$PATH" "$guard" --flux-version 2.8.8 "$@" "$tree/k8s" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

ok() { printf '  ok   %s\n' "$1"; }
bad() {
  printf '  FAIL %s\n' "$1"
  printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
  failures=$((failures + 1))
}

assert_rc() { # <label> <expected-rc>
  assertions=$((assertions + 1))
  if [ "$2" = "$GUARD_RC" ]; then ok "$1 (exit $GUARD_RC)"; else bad "$1: expected exit $2, got $GUARD_RC"; fi
}

assert_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  # A here-string, not a pipe: under pipefail an early `grep -q` match SIGPIPEs the writer.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then ok "$1"; else bad "$1: output did not contain '$2'"; fi
}

assert_not_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then bad "$1: output contained '$2'"; else ok "$1"; fi
}

assert_pulls() { # <label> <expected-count>
  assertions=$((assertions + 1))
  local count
  count="$(grep -c . "$HELM_SHIM_LOG")"
  if [ "$count" = "$2" ]; then ok "$1 ($count pull(s))"; else bad "$1: expected $2 chart pull(s), got $count: $(tr '\n' ' ' <"$HELM_SHIM_LOG")"; fi
}

assert_pulled() { # <label> <ref|repo|version>
  assertions=$((assertions + 1))
  if grep -qxF -- "$2" "$HELM_SHIM_LOG"; then ok "$1"; else bad "$1: no pull of '$2' in: $(tr '\n' ' ' <"$HELM_SHIM_LOG")"; fi
}

commit() { # <tree> <message>
  if ! git -C "$1" add -A ||
    ! git -C "$1" -c user.name=test -c user.email=test@example.invalid -c commit.gpgsign=false \
      commit -q -m "$2"; then
    printf 'FAIL: cannot commit the fixture tree\n' >&2
    exit 1
  fi
}

tree_count=0
# A fixture repository with one cluster overlay: a bootstrap layer holding the substitution
# ConfigMap, and a controllers layer, substituted from it, holding the HelmRepository. A case writes
# the HelmRelease with `release`. clusters/base names a path that does not exist, so a guard that
# rendered it as a cluster would fail every case. Sets TREE.
new_tree() {
  tree_count=$((tree_count + 1))
  TREE="$scratch/tree-$tree_count"
  local k8s="$TREE/k8s"
  mkdir -p "$k8s/clusters/test" "$k8s/clusters/base" "$k8s/bootstrap/test" "$k8s/controllers/test"
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - flux.yaml' \
    >"$k8s/clusters/test/kustomization.yaml"
  cat >"$k8s/clusters/test/flux.yaml" <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: bootstrap
  namespace: flux-system
spec:
  path: ./bootstrap/test
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: controllers
  namespace: flux-system
spec:
  path: ./controllers/test
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: variables
      - kind: Secret
        name: variables
EOF
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - flux.yaml' \
    >"$k8s/clusters/base/kustomization.yaml"
  printf '%s\n' 'apiVersion: kustomize.toolkit.fluxcd.io/v1' 'kind: Kustomization' 'metadata:' '  name: apps' \
    '  namespace: flux-system' 'spec:' '  path: ./__PROVIDER__/apps' >"$k8s/clusters/base/flux.yaml"
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - config-map.yaml' \
    '  - flux-instance.yaml' >"$k8s/bootstrap/test/kustomization.yaml"
  printf '%s\n' 'apiVersion: fluxcd.controlplane.io/v1' 'kind: FluxInstance' 'metadata:' '  name: flux' \
    '  namespace: flux-system' 'spec:' '  distribution:' '    version: 2.8.x' >"$k8s/bootstrap/test/flux-instance.yaml"
  variables 3
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' \
    '  - helm-repository.yaml' '  - helm-release.yaml' >"$k8s/controllers/test/kustomization.yaml"
  printf '%s\n' 'apiVersion: source.toolkit.fluxcd.io/v1' 'kind: HelmRepository' 'metadata:' '  name: flux-operator' \
    '  namespace: flux-system' 'spec:' '  type: oci' '  url: oci://ghcr.io/controlplaneio-fluxcd/charts' \
    >"$k8s/controllers/test/helm-repository.yaml"
  git -C "$TREE" init -q || {
    printf 'FAIL: cannot create a fixture repository\n' >&2
    exit 1
  }
}

variables() { # <operator_replicas, or "" for none>
  {
    printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: variables' '  namespace: flux-system' 'data:'
    if [ -n "$1" ]; then printf '  operator_replicas: "%s"\n' "$1"; else printf '  unrelated: "1"\n'; fi
  } >"$TREE/k8s/bootstrap/test/config-map.yaml"
}

# Writes the HelmRelease: the header at a chart version, then the case's spec lines from stdin.
release() { # [chart-version]
  {
    printf '%s\n' 'apiVersion: helm.toolkit.fluxcd.io/v2' 'kind: HelmRelease' 'metadata:' '  name: flux-operator' \
      '  namespace: flux-system' 'spec:' '  interval: 10m' '  chart:' '    spec:' '      chart: flux-operator' \
      "      version: ${1:-0.50.0}" '      sourceRef:' '        kind: HelmRepository' '        name: flux-operator'
    cat
  } >"$TREE/k8s/controllers/test/helm-release.yaml"
}

# The replica count as a Flux substitution with an inline default, the form this repository uses.
# shellcheck disable=SC2016 # a literal `${`: Flux substitutes it, not the shell
replicas='    replicas: ${operator_replicas:=1}'

# #3577's post-renderer exactly as it merged (d7613b03): the pod-level op is a JSON-6902 leaf add.
pr_3577() {
  cat <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
              name: flux-operator
            patch: |
              - op: test
                path: /spec/template/spec/containers/0/name
                value: manager
              - op: add
                path: /spec/template/spec/containers/0/securityContext/runAsUser
                value: 65532
              - op: add
                path: /spec/template/spec/containers/0/securityContext/runAsGroup
                value: 65532
              - op: add
                path: /spec/template/spec/containers/0/securityContext/seLinuxOptions
                value:
                  level: s0
              - op: add
                path: /spec/template/spec/securityContext/fsGroupChangePolicy
                value: OnRootMismatch
EOF
}

# #3580's repair exactly as it merged (7529ec47): the pod-level field is a strategic-merge patch.
pr_3580() {
  cat <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
              name: flux-operator
            patch: |
              - op: test
                path: /spec/template/spec/containers/0/name
                value: manager
              - op: add
                path: /spec/template/spec/containers/0/securityContext/runAsUser
                value: 65532
              - op: add
                path: /spec/template/spec/containers/0/securityContext/runAsGroup
                value: 65532
              - op: add
                path: /spec/template/spec/containers/0/securityContext/seLinuxOptions
                value:
                  level: s0
          - target:
              kind: Deployment
              name: flux-operator
            patch: |
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                name: flux-operator
              spec:
                template:
                  spec:
                    securityContext:
                      fsGroupChangePolicy: OnRootMismatch
EOF
}

# A JSON-6902 add beneath the pod-level securityContext of the Deployment.
pr_pod_leaf_add() {
  cat <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: add
                path: /spec/template/spec/securityContext/fsGroup
                value: 65532
EOF
}

# A JSON-6902 add beneath the pod-level securityContext of every Job, which only the hook renders.
pr_job_leaf_add() {
  cat <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Job
            patch: |
              - op: add
                path: /spec/template/spec/securityContext/runAsUser
                value: 65532
EOF
}

# A JSON-6902 test that the rendered replica count is the given number.
pr_replicas_are() { # <count>
  cat <<EOF
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: test
                path: /spec/replicas
                value: $1
EOF
}

printf 'guard-helm-post-renderers:\n'

# --- The real tree --------------------------------------------------------------------------

printf 'the committed tree, compared with itself\n'
: >"$HELM_SHIM_LOG"
if GUARD_OUT="$(PATH="$scratch/bin:$PATH" "$guard" --flux-version 2.8.8 --base HEAD "$repo_root/k8s" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc 'an unchanged tree passes' 0
assert_contains 'nothing is rendered when nothing changed' ', 0 checked'
assertions=$((assertions + 1))
if grep -Eq ', [1-9][0-9]* with post-renderers,' <<<"$GUARD_OUT"; then
  ok 'the repository'"'"'s post-rendered HelmReleases are found'
else
  bad 'the repository'"'"'s post-rendered HelmReleases are found'
fi
assert_pulls 'no chart is pulled for an unchanged tree' 0

# --- #3577 and #3580 -------------------------------------------------------------------------

printf "#3577's post-renderer (RED)\n"
new_tree
{
  printf '%s\n' '  values:' "$replicas"
  pr_3577
} | release
run_guard "$TREE"
assert_rc '#3577 fails' 1
assert_contains 'the failing release is named' 'test: HelmRelease flux-system/flux-operator'
assert_contains "kustomize's error is reported" 'add operation does not apply: doc is missing path'
assert_contains 'the unrendered path is reported' '/spec/template/spec/securityContext/fsGroupChangePolicy'
assert_pulled 'the chart is pulled at its pinned version from its OCI HelmRepository' \
  'oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator||0.50.0'

printf "#3580's repair (GREEN)\n"
new_tree
{
  printf '%s\n' '  values:' "$replicas"
  pr_3580
} | release
run_guard "$TREE"
assert_rc '#3580 passes' 0
assert_contains 'the release was rendered and applied' 'test: HelmRelease flux-system/flux-operator: 1 post-renderer(s) apply'
assert_contains 'one release was checked' ', 1 checked, all apply'

# --- Scope: only what changed since the base is rendered ------------------------------------

printf 'scope\n'
new_tree
pr_3577 | release
commit "$TREE" base
run_guard "$TREE" --base HEAD
assert_rc 'a release unchanged since the base is not rendered' 0
assert_contains 'it is counted as unchanged' '1 unchanged since HEAD, 0 checked'
assert_pulls 'no chart is pulled for it' 0

new_tree
pr_3580 | release
commit "$TREE" base
pr_3577 | release
run_guard "$TREE" --base HEAD
assert_rc 'a post-renderer changed since the base is rendered' 1
assert_contains 'the change is what fails' 'doc is missing path'

new_tree
pr_3577 | release 0.49.0
commit "$TREE" base
run_guard "$TREE" --base HEAD
assert_rc 'the base revision itself applies (the 0.49.0 chart renders the parent)' 0
pr_3577 | release 0.50.0
run_guard "$TREE" --base HEAD
assert_rc 'a chart bump under an unchanged post-renderer is rendered' 1
assert_pulled 'at the bumped version' 'oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator||0.50.0'

new_tree
{
  printf '%s\n' '  values:' "$replicas"
  pr_replicas_are 3
} | release
commit "$TREE" base
variables 4
run_guard "$TREE" --base HEAD
assert_rc 'a substitution variable the release reads, changed since the base, re-renders it' 1
assert_contains 'with the new value' '1 checked'

new_tree
pr_3580 | release
commit "$TREE" base
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: unrelated' '  namespace: flux-system' \
  >"$TREE/k8s/controllers/test/unrelated.yaml"
printf '%s\n' '  - unrelated.yaml' >>"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'a change that does not reach a post-rendered release renders nothing' 0
assert_contains 'nothing is checked' '1 unchanged since HEAD, 0 checked'

new_tree
pr_3580 | release
commit "$TREE" base
pr_3577 | release
printf '%s\n' 'this is not a kustomization: [' >"$TREE/k8s/controllers/test/kustomization.yaml"
commit "$TREE" broken
printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' \
  '  - helm-repository.yaml' '  - helm-release.yaml' >"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'a base that cannot be rendered checks everything, never less' 1
assert_contains 'and says so' 'cannot be rendered'

# --- Rendered the way Flux renders it -------------------------------------------------------

printf 'fidelity\n'
new_tree
{
  printf '%s\n' '  values:' "$replicas"
  pr_replicas_are 3
} | release
run_guard "$TREE"
assert_rc 'values are substituted from the ConfigMap the Flux Kustomization names' 0

variables ''
run_guard "$TREE"
assert_rc 'without the variable, the inline default is used instead' 1
assert_contains 'and the render carries the default' 'testing value /spec/replicas failed'

new_tree
{
  printf '%s\n' '  values:' '    podSecurityContextOnInstallOnly: true'
  pr_pod_leaf_add
} | release
run_guard "$TREE"
assert_rc 'a post-renderer that applies only to the install render fails' 1
assert_contains 'on the upgrade render' 'do not apply to the upgrade render'

new_tree
{
  printf '%s\n' '  values:' '    hook: true'
  pr_job_leaf_add
} | release
run_guard "$TREE"
assert_rc 'Flux 2.8.8 leaves hooks out of post-rendering by default' 0
{
  printf '%s\n' '  postRenderStrategy: combined' '  values:' '    hook: true'
  pr_job_leaf_add
} | release
run_guard "$TREE"
assert_rc 'Flux 2.8.8 rejects the unsupported strategy field' 2
{
  printf '%s\n' '  postRenderStrategy: nohooks' '  values:' '    hook: true'
  pr_job_leaf_add
} | release
run_guard "$TREE"
assert_rc 'even nohooks is an unsupported field on the admitted API' 2

new_tree
cat <<'EOF' | release
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              apiVersion: apps/v1
              kind: Deployment
              metadata:
                name: flux-operator
              spec:
                template:
                  spec:
                    securityContext:
                      fsGroup: 65532
    - kustomize:
        images:
          - name: ghcr.io/controlplaneio-fluxcd/flux-operator
            newTag: v0.51.0
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: add
                path: /spec/template/spec/securityContext/fsGroupChangePolicy
                value: OnRootMismatch
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: test
                path: /spec/template/spec/containers/0/image
                value: ghcr.io/controlplaneio-fluxcd/flux-operator:v0.51.0
EOF
run_guard "$TREE"
assert_rc 'post-renderers apply in order, each to the previous output, images included' 0
assert_contains 'all three applied' '3 post-renderer(s) apply'

new_tree
cat <<'EOF' | release
  valuesFrom:
    - kind: Secret
      name: operator
      valuesKey: token
      targetPath: annotations.token
  values:
    annotations:
      token: inline-must-not-win
      inline: kept
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: test
                path: /spec/template/metadata/annotations/token
                value: placeholder
              - op: test
                path: /spec/template/metadata/annotations/inline
                value: kept
EOF
run_guard "$TREE"
assert_rc 'a valuesFrom targetPath overwrites inline values and preserves its siblings' 0

# A patch relying on the inline value must fail, not receive a clean verdict
# from a chart render Flux would never produce. targetPath wins over inline
# values even though a whole valuesFrom document would merge beneath them.
new_tree
cat <<'EOF' | release
  valuesFrom:
    - kind: ConfigMap
      name: operator
      valuesKey: token
      targetPath: annotations.token
  values:
    annotations:
      token: inline-must-not-win
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: test
                path: /spec/template/metadata/annotations/token
                value: inline-must-not-win
EOF
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: operator' '  namespace: flux-system' 'data:' '  token: actual-token' >"$TREE/k8s/controllers/test/operator.yaml"
printf '%s\n' '  - operator.yaml' >>"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'a post-renderer that depends on the overwritten inline value fails' 1
assert_contains 'the overwrite fails the actual JSON test' 'test failed'

new_tree
cat <<'EOF' | release
  valuesFrom:
    - kind: Secret
      name: operator
      valuesKey: first
      targetPath: annotations.first
    - kind: ConfigMap
      name: operator
      valuesKey: second
      targetPath: annotations.second
  values:
    annotations:
      first: inline-first
      second: inline-second
      untouched: kept
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
            patch: |
              - op: test
                path: /spec/template/metadata/annotations/first
                value: placeholder
              - op: test
                path: /spec/template/metadata/annotations/second
                value: actual-second
              - op: test
                path: /spec/template/metadata/annotations/untouched
                value: kept
EOF
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: operator' '  namespace: flux-system' 'data:' '  second: actual-second' >"$TREE/k8s/controllers/test/operator.yaml"
printf '%s\n' '  - operator.yaml' >>"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'multiple targetPath references override inline values without dropping siblings' 0

# Reference order matters: setting an object path after a scalar target cannot
# be represented by the placeholder render. Do not turn that failed observation
# into a clean verdict by merging the inline object over both references.
new_tree
cat <<'EOF' | release
  valuesFrom:
    - kind: Secret
      name: operator
      valuesKey: whole
      targetPath: annotations
    - kind: Secret
      name: operator
      valuesKey: token
      targetPath: annotations.token
  values:
    annotations:
      token: inline-must-not-win
EOF
pr_3580 >>"$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'an unrepresentable targetPath reference sequence is UNKNOWN, never clean' 2
assert_contains 'the reference assembly failure is named' 'cannot assemble its values'

new_tree
cat >"$TREE/k8s/controllers/test/helm-repository.yaml" <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: flux-operator
  namespace: flux-system
spec:
  url: oci://ghcr.io/example/charts/flux-operator
  ref:
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
EOF
{
  printf '%s\n' 'apiVersion: helm.toolkit.fluxcd.io/v2' 'kind: HelmRelease' 'metadata:' '  name: flux-operator' \
    '  namespace: flux-system' 'spec:' '  interval: 10m' '  chartRef:' '    kind: OCIRepository' '    name: flux-operator'
  pr_3580
} >"$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'a chartRef to an OCIRepository is pulled by digest' 0
assert_pulled 'by its digest' \
  'oci://ghcr.io/example/charts/flux-operator@sha256:0000000000000000000000000000000000000000000000000000000000000000||'

# Sources receive their owning Kustomization's substitution, including when
# only the source's variable changed and the HelmRelease itself did not.
new_tree
pr_3577 | release
# shellcheck disable=SC2016 # Flux substitutes this literal, not the shell.
yq -i '.spec.chart.spec.version = "${chart_version}"' "$TREE/k8s/controllers/test/helm-release.yaml"
yq -i '.spec.postBuild.substitute.chart_version = "0.49.0"' "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE"
assert_rc 'a substituted chart version renders its real chart' 0

new_tree
pr_3577 | release
yq -i '.spec.chart.spec.sourceRef.kind = "OCIRepository" | del(.spec.chart)' "$TREE/k8s/controllers/test/helm-release.yaml"
yq -i '.spec.chartRef = {"kind": "OCIRepository", "name": "flux-operator"}' "$TREE/k8s/controllers/test/helm-release.yaml"
# shellcheck disable=SC2016 # Flux substitutes this literal, not the shell.
yq -i '.kind = "OCIRepository" | .spec = {"url": "oci://ghcr.io/example/charts/flux-operator", "ref": {"semver": "${chart_version}"}}' "$TREE/k8s/controllers/test/helm-repository.yaml"
yq -i '.spec.postBuild.substitute.chart_version = "0.49.0"' "$TREE/k8s/clusters/test/flux.yaml"
commit "$TREE" base
yq -i '.spec.postBuild.substitute.chart_version = "0.50.0"' "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'a source-only substitution change cannot skip a newly broken chart' 1
assert_pulled 'the resolved changed source version is pulled' 'oci://ghcr.io/example/charts/flux-operator||0.50.0'

# The ConfigMap's selected value participates in both rendering and scope.
new_tree
{
  printf '%s\n' '  valuesFrom:' '    - kind: ConfigMap' '      name: operator-values' '      valuesKey: replicas' '      targetPath: replicas'
  pr_replicas_are 3
} | release
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: operator-values' '  namespace: flux-system' 'data:' '  replicas: "3"' >"$TREE/k8s/controllers/test/operator-values.yaml"
printf '%s\n' '  - operator-values.yaml' >>"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'a ConfigMap targetPath uses its actual numeric Helm value' 0
commit "$TREE" base
yq -i '.data.unrelated = "changed"' "$TREE/k8s/controllers/test/operator-values.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'an unselected ConfigMap key does not change rendering scope' 0
assert_pulls 'an unselected ConfigMap key pulls no chart' 0
yq -i '.data.replicas = "4"' "$TREE/k8s/controllers/test/operator-values.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'a referenced ConfigMap-only change re-renders the release' 1
assert_contains 'the actual changed value breaks the post-renderer' 'testing value /spec/replicas failed'

new_tree
{
  printf '%s\n' '  valuesFrom:' '    - kind: ConfigMap' '      name: absent' '      valuesKey: replicas' '      targetPath: replicas'
  pr_3580
} | release
run_guard "$TREE"
assert_rc 'an absent required ConfigMap value is cannot-check, never a placeholder success' 2
yq -i '.spec.valuesFrom[0].optional = true' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'a genuinely missing optional ConfigMap is skipped' 0
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: absent' '  namespace: flux-system' 'data:' '  unrelated: value' >"$TREE/k8s/controllers/test/optional.yaml"
printf '%s\n' '  - optional.yaml' >>"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'optional does not ignore a missing key in an existing ConfigMap' 2

new_tree
pr_3577 | release
yq -i '.data = {}' "$TREE/k8s/bootstrap/test/config-map.yaml"
run_guard "$TREE"
assert_rc 'an empty substitution map must not swallow the HelmRelease' 1

new_tree
pr_3580 | release
commit "$TREE" base
yq -i '.spec.distribution.version = "2.9.x"' "$TREE/k8s/bootstrap/test/flux-instance.yaml"
run_guard "$TREE" --base HEAD
assert_rc 'a runtime-only change cannot be skipped as unchanged' 2

new_tree
pr_3580 | release
HELM_SHIM_VERSION=v3.19.0 run_guard "$TREE"
assert_rc 'a Helm runtime different from the controller is cannot-check' 2
if GUARD_OUT="$(PATH="$scratch/bin:$PATH" "$guard" "$TREE/k8s" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc 'a missing audited controller profile is cannot-check' 2
run_guard "$TREE" --flux-version 2.9.5
assert_rc 'an unaudited controller profile is cannot-check' 2

new_tree
{
  printf '%s\n' '  values:' '    revisionSensitive: true'
  pr_pod_leaf_add
} | release
yq -i '.spec.chart.spec.chart = "revision-sensitive"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'revision-dependent charts cannot be proven by --is-upgrade revision one' 2
# shellcheck disable=SC2016 # Helm template variables, not shell expansions.
printf '%s\n' '{{ $r := .Release }}{{ if eq $r.Revision 1 }}' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: revision-one' '{{ end }}' \
  >"$scratch/charts/revision-sensitive/templates/revision.yaml"
run_guard "$TREE"
assert_rc 'aliased Release.Revision is also historical input' 2

new_tree
pr_3580 | release
yq -i '.spec.upgrade.preserveValues = true' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'historical preserveValues cannot receive an offline clean verdict' 2
yq -i '.spec.upgrade.preserveValues = false' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'explicit false preserveValues remains supported' 0

new_tree
{
  printf '%s\n' '  targetNamespace: a-very-lengthy-target-namespace' '  values:' '    useReleaseName: true'
  pr_pod_leaf_add
} | release
yq -i '.metadata.name = "a-very-lengthy-helm-release-name" | .spec.postRenderers[0].kustomize.patches[0].target.name = "a-very-lengthy-target-namespace-a-very-l-132d54d8b62f"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'Flux shortens a long composed release name before Helm rendering' 1
assert_contains 'the hashed-name target actually matches and fails its patch' 'doc is missing path'
yq -i '.spec.releaseName = "explicit-operator" | .spec.postRenderers[0].kustomize.patches[0].target.name = "explicit-operator"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'an explicit release name is used without namespace composition' 1
assert_contains 'the explicit-name target actually matches' 'doc is missing path'

# --- Cannot check is exit 2, never clean ----------------------------------------------------

printf 'cannot check\n'
new_tree
cat <<'EOF' | release
  postRenderers:
    - kustomize:
        patchesStrategicMerge:
          - apiVersion: apps/v1
            kind: Deployment
            metadata:
              name: flux-operator
EOF
run_guard "$TREE"
assert_rc 'a post-renderer field Flux does not apply' 2

new_tree
{
  printf '%s\n' '  valuesFrom:' '    - kind: ConfigMap' '      name: operator-values'
  pr_3580
} | release
run_guard "$TREE"
assert_rc 'a valuesFrom without a targetPath' 2

new_tree
pr_3580 | release
yq -i '.spec.chart.spec.sourceRef.kind = "GitRepository"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'a chart source that is not a HelmRepository or OCIRepository' 2

new_tree
pr_3580 | release
yq -i '.spec.chart.spec.sourceRef.name = "absent"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'a chart source the cluster does not render' 2

new_tree
pr_3580 | release
yq -i '.spec.secretRef.name = "registry"' "$TREE/k8s/controllers/test/helm-repository.yaml"
run_guard "$TREE"
assert_rc 'a chart source that needs credentials' 2

new_tree
pr_3580 | release
yq -i '.spec.chart.spec.chart = "missing-chart"' "$TREE/k8s/controllers/test/helm-release.yaml"
run_guard "$TREE"
assert_rc 'a chart that cannot be pulled' 2

new_tree
pr_3580 | release
commit "$TREE" base
run_guard "$TREE" --base 0000000000000000000000000000000000000000
assert_rc 'a base revision the checkout does not hold' 2

new_tree
printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' \
  '  - helm-repository.yaml' >"$TREE/k8s/controllers/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'a tree that renders no HelmRelease at all' 2

new_tree
pr_3580 | release
printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' >"$TREE/k8s/clusters/test/kustomization.yaml"
run_guard "$TREE"
assert_rc 'a cluster overlay that names no Flux Kustomization' 2

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
printf 'PASS: the post-renderer guard fails #3577, passes #3580, renders only what changed, and refuses what it cannot render\n'
