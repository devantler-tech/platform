#!/usr/bin/env bash
#
# Pins the PodDisruptionBudget selector guard's verdict in all THREE directions (#3596).
#
#   exit 0  every rendered budget selects a rendered workload, or carries a reviewed exception
#   exit 1  a budget selects no rendered workload, or an exception row is stale or does not hold
#   exit 2  the guard could not check
#
# Every case is a fixture tree that isolates one condition. Charts come from a `pull` stand-in that
# packages a local fixture chart, so no case reaches a registry; `template` runs the real
# controller renderer and `kubectl kustomize` is real. The committed tree is checked by the guard
# itself in CI, where its charts can be pulled.
#
# The fixture chart renders a Deployment the way an upstream chart does: its pod labels come from
# the release name and a value, and from version 2.0.0 it renames one of them, which is the label
# change that detaches a budget on a chart bump.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-pdb-selector-match.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

for tool in jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'FAIL: %s is required\n' "$tool" >&2
    exit 1
  }
done

failures=0
assertions=0

if [ -z "${CONTROLLER_HELM:-}" ]; then
  CONTROLLER_HELM="$scratch/controller-helm"
  "$repo_root/scripts/build-controller-helm.sh" "$CONTROLLER_HELM" || exit 1
fi

# `pull` packages the fixture chart named by the reference instead of reaching a registry, and
# records each pull. Every other subcommand runs the real renderer.
mkdir -p "$scratch/bin" "$scratch/charts"
export HELM_SHIM_REAL="$CONTROLLER_HELM" HELM_SHIM_CHARTS="$scratch/charts" HELM_SHIM_LOG="$scratch/pulls.log"
cat >"$scratch/bin/controller-helm" <<'EOF'
#!/usr/bin/env bash
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
  # A registry that is briefly unreachable: the first HELM_SHIM_FAIL_FIRST pulls of a run fail.
  if [ "$(grep -c . "$HELM_SHIM_LOG")" -le "${HELM_SHIM_FAIL_FIRST:-0}" ]; then
    printf 'Error: connection reset by peer\n' >&2
    exit 1
  fi
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
chmod +x "$scratch/bin/controller-helm"

chart="$scratch/charts/web"
mkdir -p "$chart/templates"
printf '%s\n' 'apiVersion: v2' 'name: web' 'version: 1.0.0' >"$chart/Chart.yaml"
printf '%s\n' 'replicas: 2' 'name: web' 'kind: Deployment' 'installOnlyLabel: false' 'kubeLabel: false' 'podLabels: {}' 'job: false' \
  'canary: false' 'canaryOnUpgradeOnly: false' 'hook: false' >"$chart/values.yaml"
# The chart's own Flagger Canary over the workload it renders.
cat >"$chart/templates/canary.yaml" <<'EOF'
{{- if and .Values.canary (or .Release.IsUpgrade (not .Values.canaryOnUpgradeOnly)) }}
apiVersion: flagger.app/v1beta1
kind: Canary
metadata:
  name: {{ .Release.Name }}
spec:
  targetRef:
    apiVersion: apps/v1
    kind: {{ .Values.kind }}
    name: {{ .Release.Name }}
{{- end }}
EOF
# A workload that is a Helm hook: Helm runs it around the install, it is not part of the release's
# manifest, and it does not serve.
cat >"$chart/templates/hook.yaml" <<'EOF'
{{- if .Values.hook }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-hook
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
spec:
  selector:
    matchLabels:
      app: hook
  template:
    metadata:
      labels:
        app: hook
    spec:
      containers:
        - name: hook
          image: registry.example.invalid/web:1.0.0
{{- end }}
EOF
cat >"$chart/templates/workload.yaml" <<'EOF'
apiVersion: apps/v1
kind: {{ .Values.kind }}
metadata:
  name: {{ .Release.Name }}
spec:
  {{- if ne .Values.kind "DaemonSet" }}
  replicas: {{ .Values.replicas }}
  {{- end }}
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ .Values.name }}{{ if semverCompare ">=2.0.0" .Chart.Version }}-v2{{ end }}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{ .Values.name }}{{ if semverCompare ">=2.0.0" .Chart.Version }}-v2{{ end }}
        app.kubernetes.io/instance: {{ .Release.Name }}
        helm.toolkit.fluxcd.io/name: {{ .Release.Name }}
        {{- if and .Values.installOnlyLabel (not .Release.IsUpgrade) }}
        phase: install
        {{- end }}
        {{- if .Values.kubeLabel }}
        kube: {{ .Capabilities.KubeVersion.Version }}
        {{- end }}
        {{- with .Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    spec:
      containers:
        - name: web
          image: registry.example.invalid/web:1.0.0
EOF
cat >"$chart/templates/job.yaml" <<'EOF'
{{- if .Values.job }}
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ .Release.Name }}-provision
spec:
  schedule: "0 * * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            app: provision
        spec:
          restartPolicy: Never
          containers:
            - name: provision
              image: registry.example.invalid/web:1.0.0
{{- end }}
EOF

mkdir -p "$scratch/charts/broken/templates"
printf '%s\n' 'apiVersion: v2' 'name: broken' 'version: 1.0.0' >"$scratch/charts/broken/Chart.yaml"
printf '%s\n' '{{ fail "this chart cannot render" }}' >"$scratch/charts/broken/templates/fail.yaml"

: >"$scratch/no-exceptions.tsv"

run_guard() { # <tree> [exceptions-file] [guard-args...]
  local tree="$1" exceptions="${2:-$scratch/no-exceptions.tsv}"
  shift
  [ "$#" -eq 0 ] || shift
  : >"$HELM_SHIM_LOG"
  # No wait between pull attempts: the retry is counted here, not timed.
  if GUARD_OUT="$(PDB_SELECTOR_EXCEPTIONS="$exceptions" CONTROLLER_HELM="$scratch/bin/controller-helm" \
    PDB_GUARD_PULL_BACKOFF="${PULL_BACKOFF:-0}" "$guard" "$@" "$tree/k8s" 2>&1)"; then
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

tree_count=0
# A fixture tree with one cluster overlay whose Flux Kustomization, substituting from the
# `variables` ConfigMap, points at apps/test, which renders objects.yaml. A case appends its
# objects with `add`. clusters/base names a path that does not exist, so a guard that rendered it
# as a cluster would fail every case. Sets TREE.
new_tree() {
  tree_count=$((tree_count + 1))
  TREE="$scratch/tree-$tree_count"
  local k8s="$TREE/k8s"
  mkdir -p "$k8s/clusters/test" "$k8s/clusters/base" "$k8s/apps/test"
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - flux.yaml' \
    >"$k8s/clusters/test/kustomization.yaml"
  cat >"$k8s/clusters/test/flux.yaml" <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./apps/test
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
  printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - objects.yaml' \
    >"$k8s/apps/test/kustomization.yaml"
  printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: variables' '  namespace: flux-system' \
    'data:' '  web_name: web' >"$k8s/apps/test/objects.yaml"
  # What an exception row is pinned to: a file beside the k8s root, as ksail.prod.yaml is.
  printf '%s\n' 'node:' '  version: 1.0.0' >"$TREE/pins.yaml"
}

# Appends the YAML documents on stdin to the fixture tree.
add() {
  {
    printf '%s\n' '---'
    cat
  } >>"$TREE/k8s/apps/test/objects.yaml"
}

pdb() { # <namespace> <name> [selector as flow YAML; none when omitted]
  {
    printf '%s\n' 'apiVersion: policy/v1' 'kind: PodDisruptionBudget' 'metadata:' "  name: $2" "  namespace: $1" \
      'spec:' '  maxUnavailable: 1'
    [ "$#" -lt 3 ] || printf '  selector: %s\n' "$3"
  } | add
}

workload() { # <kind> <namespace> <name> <pod labels as flow YAML> [replicas]
  {
    printf '%s\n' 'apiVersion: apps/v1' "kind: $1" 'metadata:' "  name: $3" "  namespace: $2" 'spec:'
    [ "$#" -lt 5 ] || printf '  replicas: %s\n' "$5"
    printf '%s\n' '  selector:' "    matchLabels: $4" '  template:' '    metadata:' "      labels: $4" '    spec:' \
      '      containers:' '        - name: main' '          image: registry.example.invalid/main:1.0.0'
  } | add
}

# Writes a HelmRepository and a HelmRelease of the fixture chart. The case's extra spec lines come
# from stdin.
release() { # <namespace> <name> [chart] [version]
  {
    printf '%s\n' 'apiVersion: source.toolkit.fluxcd.io/v1' 'kind: HelmRepository' 'metadata:' "  name: $2" \
      "  namespace: $1" 'spec:' '  url: https://charts.example.invalid'
    printf '%s\n' '---' 'apiVersion: helm.toolkit.fluxcd.io/v2' 'kind: HelmRelease' 'metadata:' "  name: $2" \
      "  namespace: $1" 'spec:' '  interval: 10m' '  chart:' '    spec:' "      chart: ${3:-web}" \
      "      version: ${4:-1.0.0}" '      sourceRef:' '        kind: HelmRepository' "        name: $2"
    cat
  } | add
}

# The flagger HelmRelease the guard reads Flagger's selector labels from. It is never rendered:
# no budget lives in its namespace.
flagger() { # [extra values line]
  release flagger-system flagger flagger 1.45.0 <<EOF
  values:
    meshProvider: gatewayapi:v1
${1:-}
EOF
}

canary() { # <namespace> <name> <target kind> <target name>
  add <<EOF
apiVersion: flagger.app/v1beta1
kind: Canary
metadata:
  name: $2
  namespace: $1
spec:
  targetRef:
    apiVersion: apps/v1
    kind: $3
    name: $4
EOF
}

both='{matchLabels: {app.kubernetes.io/name: web, app.kubernetes.io/instance: web}}'
instance='{matchLabels: {app.kubernetes.io/instance: web}}'
web_labels='{app.kubernetes.io/name: web, app.kubernetes.io/instance: web}'

echo "== a budget selecting a rendered manifest passes, and its perturbed selector fails =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'a selector matching a rendered Deployment' 0
assert_contains 'names the budget and what it selects' 'test: PodDisruptionBudget demo/web selects Deployment web (a rendered manifest)'
assert_pulls 'a budget the manifests satisfy pulls no chart' 0

new_tree
pdb demo web '{matchLabels: {app.kubernetes.io/name: web, app.kubernetes.io/instance: wbe}}'
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'a selector with one perturbed label' 1
assert_contains 'names the budget' 'test: PodDisruptionBudget demo/web selects no workload rendered into namespace demo'
assert_contains 'names its selector' 'selector: app.kubernetes.io/instance=wbe, app.kubernetes.io/name=web'
assert_contains 'names the nearest candidate and its labels' 'nearest:  Deployment web (a rendered manifest): app.kubernetes.io/instance=web,app.kubernetes.io/name=web — 1 of 2 selector requirement(s) match'
assert_contains 'names the fix' 'Point the selector at the labels of the pod template that serves'

for kind in StatefulSet DaemonSet ReplicaSet; do
  new_tree
  pdb demo web "$both"
  workload "$kind" demo web "$web_labels"
  run_guard "$TREE"
  assert_rc "a selector matching a rendered $kind" 0
done

new_tree
pdb demo web "$both"
workload Deployment other web "$web_labels" 2
run_guard "$TREE"
assert_rc 'the same labels in another namespace do not satisfy a budget' 1
assert_contains 'says nothing is rendered into its namespace' 'nearest:  no workload is rendered into namespace demo'

new_tree
pdb demo web "$both"
add <<'EOF'
apiVersion: batch/v1
kind: CronJob
metadata:
  name: web
  namespace: demo
spec:
  schedule: "0 * * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          labels: {app.kubernetes.io/name: web, app.kubernetes.io/instance: web}
        spec:
          restartPolicy: Never
          containers:
            - name: main
              image: registry.example.invalid/main:1.0.0
EOF
run_guard "$TREE"
assert_rc 'a budget matching only a CronJob protects nothing that serves' 1
assert_contains 'does not count the CronJob as a workload' 'nearest:  no workload is rendered into namespace demo'

echo "== a deliberately scaled-to-zero workload still satisfies its budget =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 0
run_guard "$TREE"
assert_rc 'replicas: 0 is not a finding' 0

echo "== selector semantics =="
new_tree
pdb demo web
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'a budget without a selector selects no pod' 1
assert_contains 'says why' 'has no selector, and a budget without one selects no pod'

new_tree
pdb demo web '{}'
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'an empty selector selects every pod in the namespace' 0

new_tree
pdb demo web '{}'
workload Deployment other web "$web_labels" 2
run_guard "$TREE"
assert_rc 'an empty selector in a namespace with no workload' 1
assert_contains 'describes the empty selector' 'selector: {} (every pod in the namespace)'

expression() { # <operator> [values as flow YAML]
  printf '{matchExpressions: [{key: tier, operator: %s%s}]}' "$1" "${2:+, values: $2}"
}
for arm in 'In|[web, api]|0' 'In|[api]|1' 'NotIn|[api]|0' 'NotIn|[web]|1' 'Exists||0' 'DoesNotExist||1'; do
  operator="${arm%%|*}"
  rest="${arm#*|}"
  new_tree
  pdb demo web "$(expression "$operator" "${rest%%|*}")"
  workload Deployment demo web '{tier: web}' 2
  run_guard "$TREE"
  assert_rc "matchExpressions $operator ${rest%%|*} against tier=web" "${rest##*|}"
  [ "${rest##*|}" = 0 ] || assert_contains "names the $operator expression" "selector: tier $operator"
done
new_tree
pdb demo web "$(expression DoesNotExist)"
workload Deployment demo web '{app: web}' 2
run_guard "$TREE"
assert_rc 'matchExpressions DoesNotExist against a pod without the label' 0
new_tree
pdb demo web "$(expression NotIn '[web]')"
workload Deployment demo web '{app: web}' 2
run_guard "$TREE"
assert_rc 'matchExpressions NotIn against a pod without the label' 0
new_tree
pdb demo web "$(expression Near '[web]')"
workload Deployment demo web '{tier: web}' 2
run_guard "$TREE"
assert_rc 'an operator no label selector defines' 2
assert_contains 'names the operator' 'selector operator Near'

echo "== a budget whose workload only a chart renders =="
new_tree
pdb demo web "$both"
# shellcheck disable=SC2016 # a literal `${`: Flux substitutes it, not the shell
release demo web <<'EOF'
  values:
    name: ${web_name}
EOF
run_guard "$TREE"
assert_rc 'a selector matching the chart-rendered Deployment, its label from a Flux variable' 0
assert_contains 'names the release that renders it' 'test: PodDisruptionBudget demo/web selects Deployment web (HelmRelease demo/web)'
assert_pulls 'pulls the chart once for both renders' 1

new_tree
pdb demo web "$both"
# shellcheck disable=SC2016 # a literal `${`: Flux substitutes it, not the shell
release demo web web 2.0.0 <<'EOF'
  values:
    name: ${web_name}
EOF
run_guard "$TREE"
assert_rc 'a chart bump that renames the pod label detaches the budget' 1
assert_contains 'names the chart-rendered candidate and its new labels' 'nearest:  Deployment web (HelmRelease demo/web): app.kubernetes.io/instance=web,app.kubernetes.io/name=web-v2,helm.toolkit.fluxcd.io/name=web — 1 of 2 selector requirement(s) match'

new_tree
pdb demo web "$both"
release demo web <<'EOF'
  values:
    name: ${unset_variable:=web}
EOF
run_guard "$TREE"
assert_rc 'an inline substitution default reaches the chart values' 0

new_tree
pdb demo web "$both"
release other web <<'EOF'
  targetNamespace: demo
  releaseName: web
EOF
run_guard "$TREE"
assert_rc 'a release installed into the budget namespace through targetNamespace' 0

new_tree
pdb demo web "$both"
release other web </dev/null
run_guard "$TREE"
assert_rc 'a release installed into another namespace does not satisfy the budget' 1
assert_contains 'finds nothing in the budget namespace' 'nearest:  no workload is rendered into namespace demo'
assert_pulls 'and is not rendered for it' 0

new_tree
pdb demo web '{matchLabels: {app: provision}}'
release demo web <<'EOF'
  values:
    job: true
EOF
run_guard "$TREE"
assert_rc 'a budget matching only a chart-rendered CronJob' 1
assert_contains 'lists the Deployment, not the CronJob' 'nearest:  Deployment web (HelmRelease demo/web): app.kubernetes.io/instance=web,app.kubernetes.io/name=web,helm.toolkit.fluxcd.io/name=web — 0 of 1 selector requirement(s) match'
assert_not_contains 'never lists the CronJob' 'web-provision'

new_tree
pdb demo web '{matchLabels: {kube: v1.33.1}}'
release demo web <<'EOF'
  values:
    kubeLabel: true
EOF
run_guard "$TREE" "$scratch/no-exceptions.tsv" --kube-version v1.33.1
assert_rc 'the chart renders for the Kubernetes version it is given' 0
run_guard "$TREE" "$scratch/no-exceptions.tsv" --kube-version v1.34.0
assert_rc 'and for no other' 1
assert_contains 'shows the version the chart rendered for' 'kube=v1.34.0'

echo "== post-renderers are applied before the labels are read =="
relabel() {
  cat <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
              name: web
            patch: |
              - op: replace
                path: /spec/template/metadata/labels/app.kubernetes.io~1instance
                value: patched
EOF
}
new_tree
pdb demo web '{matchLabels: {app.kubernetes.io/instance: patched}}'
relabel | release demo web
run_guard "$TREE"
assert_rc 'a selector matching the post-rendered label' 0
new_tree
pdb demo web "$instance"
relabel | release demo web
run_guard "$TREE"
assert_rc 'a selector matching only the label the post-renderer replaced' 1
assert_contains 'shows the post-rendered label' 'app.kubernetes.io/instance=patched'

echo "== the budget must hold in the install and the upgrade render =="
new_tree
pdb demo web '{matchLabels: {phase: install}}'
release demo web <<'EOF'
  values:
    installOnlyLabel: true
EOF
run_guard "$TREE"
assert_rc 'a label the chart renders only on install' 1
assert_contains 'names the render that fails' 'selects no workload rendered into namespace demo in the upgrade render'

echo "== Flagger: the primary serves, the target is scaled to zero =="
new_tree
flagger
pdb demo web "$both"
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc "#4365: a selector naming the label Flagger rewrites" 1
assert_contains 'explains that only the idle target matches' 'it matches only Deployment web, which Flagger Canary web keeps at 0 replicas between rollouts; the pods that serve belong to web-primary'
assert_contains 'names the primary and its rewritten label' 'nearest:  Deployment web-primary (the primary Flagger creates for Deployment web, HelmRelease demo/web): app.kubernetes.io/instance=web,app.kubernetes.io/name=web-primary — 1 of 2 selector requirement(s) match'
assert_contains 'lists the idle target too' 'Deployment web (HelmRelease demo/web): app.kubernetes.io/instance=web,app.kubernetes.io/name=web,helm.toolkit.fluxcd.io/name=web — 2 of 2 selector requirement(s) match, scaled to 0 by Flagger'

new_tree
flagger
pdb demo web "$instance"
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc "#4366: a selector on the label Flagger leaves intact" 0
assert_contains 'names the primary it selects' 'test: PodDisruptionBudget demo/web selects Deployment web-primary (the primary Flagger creates for Deployment web, HelmRelease demo/web)'

new_tree
flagger
pdb demo web '{matchLabels: {app.kubernetes.io/name: web-primary}}'
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a selector naming the rewritten label value' 0

new_tree
flagger
pdb demo web '{matchLabels: {helm.toolkit.fluxcd.io/name: web}}'
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Flux toolkit label, which Flagger does not copy to the primary' 1
assert_contains 'shows the primary without the toolkit label' 'nearest:  Deployment web-primary (the primary Flagger creates for Deployment web, HelmRelease demo/web): app.kubernetes.io/instance=web,app.kubernetes.io/name=web-primary — 0 of 1 selector requirement(s) match'

new_tree
flagger
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Canary over a rendered manifest: the target alone does not satisfy' 1
assert_contains 'explains the idle manifest target' 'it matches only Deployment web, which Flagger Canary web keeps at 0 replicas between rollouts'
new_tree
flagger
pdb demo web "$instance"
workload Deployment demo web "$web_labels" 2
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Canary over a rendered manifest: the primary satisfies' 0

new_tree
flagger
pdb demo web "$instance"
release demo web <<'EOF'
  values:
    kind: DaemonSet
EOF
canary demo web DaemonSet web
run_guard "$TREE"
assert_rc 'a DaemonSet target gets a primary too' 0
assert_contains 'names the DaemonSet primary' 'selects DaemonSet web-primary'

new_tree
flagger
pdb demo web '{matchLabels: {app: web}}'
workload Deployment demo web '{app: web, name: web}' 2
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'the first of the selector labels the target holds is the one rewritten' 1
assert_contains 'rewrites app, not name' 'Deployment web-primary (the primary Flagger creates for Deployment web, a rendered manifest): app=web-primary,name=web'

new_tree
flagger
pdb demo web "$both"
workload Deployment demo api "$web_labels" 2
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Canary beside a budget another workload satisfies' 0

new_tree
flagger
pdb demo web "$both"
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Canary whose target nothing renders' 2
assert_contains 'says the primary cannot be derived' 'test: PodDisruptionBudget demo/web cannot be checked: Canary demo/web targets Deployment web, which nothing rendered into the namespace produces'

new_tree
flagger
pdb demo web '{matchLabels: {tier: web}}'
workload Deployment demo web '{role: web}' 2
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a target Flagger cannot build a primary from' 2
assert_contains 'names the labels Flagger needs' 'holds none of the labels Flagger builds a primary from (app, name, app.kubernetes.io/name)'

new_tree
pdb demo web "$instance"
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a Canary with no flagger HelmRelease to read the selector labels from' 2
assert_contains 'says what is missing' 'no flagger HelmRelease'

new_tree
flagger '    selectorLabels: app.kubernetes.io/instance'
pdb demo web "$instance"
release demo web </dev/null
canary demo web Deployment web
run_guard "$TREE"
assert_rc 'a flagger HelmRelease that overrides the selector labels' 2
assert_contains 'says only the default is modelled' "models Flagger's default -selector-labels only"

new_tree
flagger
pdb demo web "$instance"
release demo web </dev/null
canary demo web Rollout web
run_guard "$TREE"
assert_rc 'a Canary target kind no primary is modelled for' 2
assert_contains 'names the kind' 'Canary demo/web targets a Rollout, which is not a kind this guard models a Flagger primary for'

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
canary other web Deployment web
run_guard "$TREE"
assert_rc 'a Canary in a namespace without a budget is not read' 0

echo "== a release the guard cannot render is never clean =="
new_tree
pdb demo web "$both"
release demo web </dev/null
yq -i '(select(.kind == "HelmRepository") | .spec.secretRef.name) = "registry-credentials"' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a chart source that needs credentials' 2
assert_contains 'still says what the rendered workloads show' 'what could be rendered: it selects no workload rendered into namespace demo'
assert_contains 'leads the entry with the budget and the cause' '  test: PodDisruptionBudget demo/web cannot be checked: HelmRelease demo/web cannot be rendered (its chart source HelmRepository/web needs credentials, and charts are pulled anonymously here)'
assert_pulls 'pulls nothing with credentials' 0

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
release demo web </dev/null
yq -i '(select(.kind == "HelmRepository") | .spec.secretRef.name) = "registry-credentials"' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'an unrenderable release beside a budget the manifests satisfy' 0

new_tree
pdb demo web "$both"
release demo web missing-chart </dev/null
run_guard "$TREE"
assert_rc 'a chart that cannot be pulled' 2
assert_contains 'says the pull failed, and after how many attempts' 'cannot pull chart missing-chart from https://charts.example.invalid at 1.0.0 after 3 attempt(s): Error: chart "missing-chart" not found'
assert_pulls 'tries a failing pull a bounded number of times' 3

new_tree
pdb demo web "$both"
release demo web broken </dev/null
run_guard "$TREE"
assert_rc 'a chart that does not render' 2
assert_contains 'says the render failed' 'its chart does not render as an install'

new_tree
pdb demo web "$both"
release demo web <<'EOF'
  valuesFrom:
    - kind: ConfigMap
      name: web-values
EOF
run_guard "$TREE"
assert_rc 'a valuesFrom entry that merges a whole values document' 2
assert_contains 'says the values cannot be stood in for' 'HelmRelease demo/web cannot be rendered (a spec.valuesFrom entry has no plain dotted targetPath'

new_tree
pdb demo web "$both"
release demo web <<'EOF'
  postRenderers:
    - kustomize:
        patches:
          - target:
              kind: Deployment
              name: web
            patch: |
              - op: add
                path: /spec/template/spec/securityContext/fsGroup
                value: 65532
EOF
run_guard "$TREE"
assert_rc 'a post-renderer that does not apply' 2
assert_contains 'says which post-renderer' 'post-renderer 1 of 1 does not apply to its install render'

new_tree
pdb demo web '{matchLabels: {app.kubernetes.io/instance: api}}'
release demo web </dev/null
release demo api broken </dev/null
run_guard "$TREE"
assert_rc 'a rendered mismatch beside an unrenderable release is undecided, not clean' 2
assert_contains 'still names the rendered candidate' 'Deployment web (HelmRelease demo/web)'
assert_contains 'and the release it could not render' 'test: PodDisruptionBudget demo/web cannot be checked: HelmRelease demo/api cannot be rendered (its chart does not render as an install'

new_tree
pdb demo web "$both"
release demo web </dev/null
release demo api broken </dev/null
run_guard "$TREE"
assert_rc 'a budget one release satisfies is decided although another cannot be rendered' 0

echo "== reviewed exceptions =="
exceptions="$scratch/exceptions.tsv"
tab="$(printf '\t')"
# Joins its arguments into one tab-separated row.
cols() {
  local IFS="$tab"
  printf '%s\n' "$*"
}
# One row for the fixture cluster, pinned to the version in the tree's pins.yaml.
row() { # <budget> <producer> <reason> [cluster] [pin] [verified version] [unrelated workloads]
  cols "${4:-test}" "$1" "$2" "${5:-file:pins.yaml:.node.version}" "${6:-1.0.0}" "${7:--}" "$3" >"$exceptions"
}

new_tree
pdb kube-system coredns '{matchLabels: {k8s-app: kube-dns}}'
row kube-system/coredns external:node-os 'The node OS installs the Deployment.'
run_guard "$TREE" "$exceptions"
assert_rc 'a budget whose pods are built outside every render' 0
assert_contains 'reports the exception, not a match, and what it was verified at' 'excepted  test: PodDisruptionBudget kube-system/coredns is excepted: its pods are built by external:node-os (The node OS installs the Deployment.); verified at file:pins.yaml:.node.version 1.0.0'

new_tree
pdb observability server '{matchLabels: {app.kubernetes.io/component: server}}'
add <<'EOT'
apiVersion: example.invalid/v1
kind: Server
metadata:
  name: server
  namespace: observability
EOT
row observability/server Server/observability/server 'The operator builds the StatefulSet from this object.'
run_guard "$TREE" "$exceptions"
assert_rc 'a producer the cluster renders' 0

new_tree
pdb observability server '{matchLabels: {app.kubernetes.io/component: server}}'
row observability/server Server/observability/server 'The operator builds the StatefulSet from this object.'
run_guard "$TREE" "$exceptions"
assert_rc 'a producer the cluster does not render' 1
assert_contains 'says the producer is missing' 'its exception names Server/observability/server, which cluster test does not render'
assert_not_contains 'a refused row is not also called stale' 'stale exception'

new_tree
pdb demo web "$both"
release demo web </dev/null
yq -i '(select(.kind == "HelmRepository") | .spec.secretRef.name) = "registry-credentials"' "$TREE/k8s/apps/test/objects.yaml"
row demo/web HelmRelease/demo/web 'The chart is private; labels read from the released chart.'
run_guard "$TREE" "$exceptions"
assert_rc 'a HelmRelease producer the guard cannot render' 0
assert_contains 'records why it could not be rendered' 'which cannot be rendered (its chart source HelmRepository/web needs credentials'

new_tree
pdb demo web '{matchLabels: {app.kubernetes.io/instance: wbe}}'
release demo web </dev/null
row demo/web HelmRelease/demo/web 'The chart is private; labels read from the released chart.'
run_guard "$TREE" "$exceptions"
assert_rc 'an exception cannot stand in for a rendered mismatch' 1
assert_contains 'says so' 'its exception names HelmRelease/demo/web, which renders here: an exception cannot stand in for a rendered mismatch'

new_tree
pdb demo web "$both"
release other web </dev/null
row demo/web HelmRelease/other/web 'The chart is private; labels read from the released chart.'
run_guard "$TREE" "$exceptions"
assert_rc 'a HelmRelease producer installed into another namespace' 1
assert_contains 'says where the release is not' 'which is not installed into namespace demo'

# The release is unrenderable and WAS attempted, for the budget of its own namespace. It still
# builds nothing in namespace demo.
new_tree
pdb demo web "$both"
pdb other web "$both"
release other web </dev/null
yq -i '(select(.kind == "HelmRepository") | .spec.secretRef.name) = "registry-credentials"' "$TREE/k8s/apps/test/objects.yaml"
{
  cols test demo/web HelmRelease/other/web file:pins.yaml:.node.version 1.0.0 - 'The chart is private.'
  cols test other/web HelmRelease/other/web file:pins.yaml:.node.version 1.0.0 - 'The chart is private.'
} >"$exceptions"
run_guard "$TREE" "$exceptions"
assert_rc 'an unrenderable release attempted for another namespace does not except a budget here' 1
assert_contains 'refuses the row for the other namespace' 'its exception names HelmRelease/other/web, which is not installed into namespace demo'
assert_contains 'and accepts it for the namespace the release installs into' 'excepted  test: PodDisruptionBudget other/web is excepted: its pods are built by HelmRelease/other/web, which cannot be rendered'
assert_not_contains 'never excepts the budget in the other namespace' 'PodDisruptionBudget demo/web is excepted'

echo "== a row applies to one cluster =="
# The row is true for cluster `test`, where nothing renders the Deployment. Cluster `second` renders
# it as a manifest, and its budget's selector names a label nothing carries.
new_tree
pdb kube-system coredns '{matchLabels: {k8s-app: kube-dns}}'
cp -R "$TREE/k8s/clusters/test" "$TREE/k8s/clusters/second"
cp -R "$TREE/k8s/apps/test" "$TREE/k8s/apps/second"
yq -i '.spec.path = "./apps/second"' "$TREE/k8s/clusters/second/flux.yaml"
row kube-system/coredns external:node-os 'The node OS installs the Deployment.'
run_guard "$TREE" "$exceptions"
assert_rc 'the same budget in a second cluster is not excepted by the first cluster row' 1
assert_contains 'judges the second cluster budget' 'second: PodDisruptionBudget kube-system/coredns selects no workload rendered into namespace kube-system'
assert_contains 'still excepts the cluster the row names' 'excepted  test: PodDisruptionBudget kube-system/coredns'
{
  cols test kube-system/coredns external:node-os file:pins.yaml:.node.version 1.0.0 - 'The node OS installs the Deployment.'
  cols second kube-system/coredns external:node-os file:pins.yaml:.node.version 1.0.0 - 'The node OS installs the Deployment.'
} >"$exceptions"
run_guard "$TREE" "$exceptions"
assert_rc 'each cluster takes its own row for the same budget' 0
assert_contains 'excepts the second cluster by its own row' 'excepted  second: PodDisruptionBudget kube-system/coredns'

yq -i '(select(.kind == "PodDisruptionBudget") | .spec.selector.matchLabels["k8s-app"]) = "nothing"' "$TREE/k8s/apps/second/objects.yaml"
cat >>"$TREE/k8s/apps/second/objects.yaml" <<'EOT'
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: coredns
  namespace: kube-system
spec:
  replicas: 2
  selector:
    matchLabels: {k8s-app: kube-dns}
  template:
    metadata:
      labels: {k8s-app: kube-dns}
    spec:
      containers:
        - name: main
          image: registry.example.invalid/main:1.0.0
EOT
row kube-system/coredns external:node-os 'The node OS installs the Deployment.'
run_guard "$TREE" "$exceptions"
assert_rc 'a second cluster that renders the Deployment, with a selector nothing carries' 1
assert_contains 'names the Deployment the second cluster renders' 'nearest:  Deployment coredns (a rendered manifest): k8s-app=kube-dns — 0 of 1 selector requirement(s) match'
assert_not_contains 'never excepts the second cluster' 'excepted  second:'

new_tree
pdb kube-system coredns '{matchLabels: {k8s-app: kube-dns}}'
row kube-system/coredns external:node-os 'The node OS installs the Deployment.' ghost
run_guard "$TREE" "$exceptions"
assert_rc 'a row for a cluster that is no overlay' 1
assert_contains 'names the cluster nothing renders' 'stale exception: the row for PodDisruptionBudget kube-system/coredns names cluster ghost, which is no overlay under'
assert_contains 'and judges the budget without the row' 'test: PodDisruptionBudget kube-system/coredns selects no workload rendered into namespace kube-system'

echo "== a row never stands in for a workload the guard can judge =="
partial='{matchLabels: {app.kubernetes.io/name: web, app.kubernetes.io/instance: wbe}}'
for producer in external:x ConfigMap/flux-system/variables; do
  new_tree
  pdb demo web "$partial"
  workload Deployment demo web "$web_labels" 2
  row demo/web "$producer" 'Something else builds the pods.'
  run_guard "$TREE" "$exceptions"
  assert_rc "a manifest matching part of the selector is not excepted by $producer" 1
  assert_contains 'names the workload it can judge' 'its exception cannot stand: cluster test renders Deployment web (a rendered manifest), which carries part of what this selector names'
  assert_not_contains 'does not report the budget as excepted' 'is excepted'
  assert_not_contains 'does not call the refused row stale' 'stale exception'
done

new_tree
pdb demo web "$partial"
release demo web </dev/null
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a chart-rendered workload matching part of the selector is not excepted' 1
assert_contains 'names the chart-rendered workload' 'its exception cannot stand: cluster test renders Deployment web (HelmRelease demo/web), which carries part of what this selector names'

new_tree
flagger
pdb demo web '{matchLabels: {app.kubernetes.io/name: web, tier: front}}'
workload Deployment demo web "$web_labels" 2
canary demo web Deployment web
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a Flagger target matching part of the selector is not excepted' 1
assert_contains 'names the idle target' 'its exception cannot stand: cluster test renders Deployment web (a rendered manifest), which carries part of what this selector names'

new_tree
pdb demo web '{matchLabels: {app: server}, matchExpressions: [{key: tier, operator: Exists}]}'
workload Deployment demo other '{tier: web}' 2
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a workload satisfying an Exists requirement is one the guard can judge' 1
new_tree
pdb demo web '{matchLabels: {app: server}, matchExpressions: [{key: tier, operator: In, values: [web]}]}'
workload Deployment demo other '{tier: web}' 2
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a workload satisfying an In requirement is one the guard can judge' 1
# NotIn and DoesNotExist hold for every workload that lacks the label, so they single none out.
new_tree
pdb demo web '{matchLabels: {app: server}, matchExpressions: [{key: tier, operator: NotIn, values: [batch]}, {key: canary, operator: DoesNotExist}]}'
workload Deployment demo other '{role: other}' 2
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a workload satisfying only NotIn and DoesNotExist is unrelated' 0

new_tree
pdb demo web "$both"
workload Deployment demo web '{role: other}' 2
row demo/web Deployment/demo/web 'The Deployment builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a rendered workload named as the producer is judged, not excepted' 1
assert_contains 'says a workload is read, never excepted' 'its exception names Deployment/demo/web, a workload this guard reads itself: a rendered workload is judged, never excepted'

new_tree
pdb demo web
row demo/web external:x 'Something else builds the pods.'
run_guard "$TREE" "$exceptions"
assert_rc 'a budget without a selector is not excepted' 1
assert_contains 'says no row can stand for it' 'its exception cannot stand: a budget without a selector selects no pod, whatever builds them'

echo "== a workload that shares a label is excepted only when the row names it =="
shared='{matchLabels: {app.kubernetes.io/part-of: stack, app.kubernetes.io/component: server}}'
new_tree
pdb demo server "$shared"
workload Deployment demo exporter '{app.kubernetes.io/part-of: stack, app.kubernetes.io/component: exporter}' 1
row demo/server external:x 'An operator builds the server.'
run_guard "$TREE" "$exceptions"
assert_rc 'an unnamed workload sharing a grouping label refuses the row' 1
assert_contains 'names the workload to review' 'its exception cannot stand: cluster test renders Deployment exporter (a rendered manifest)'
row demo/server external:x 'An operator builds the server.' test file:pins.yaml:.node.version 1.0.0 Deployment/exporter
run_guard "$TREE" "$exceptions"
assert_rc 'the row stands once it names that workload as reviewed and unrelated' 0
workload Deployment demo second '{app.kubernetes.io/part-of: stack}' 1
run_guard "$TREE" "$exceptions"
assert_rc 'a second workload sharing the label is not covered by the first name' 1
assert_contains 'names the workload the row does not list' 'its exception cannot stand: cluster test renders Deployment second (a rendered manifest)'
row demo/server external:x 'An operator builds the server.' test file:pins.yaml:.node.version 1.0.0 Deployment/exporter,Deployment/second
run_guard "$TREE" "$exceptions"
assert_rc 'the row stands when it names both' 0
row demo/server external:x 'An operator builds the server.' test file:pins.yaml:.node.version 1.0.0 Deployment/exporter,Deployment/second,Deployment/gone
run_guard "$TREE" "$exceptions"
assert_rc 'a name that no longer applies is refused' 1
assert_contains 'says which name to remove' 'its exception lists Deployment/gone as a reviewed unrelated workload, but cluster test renders no such workload carrying part of this selector into namespace demo: remove it from the row'

echo "== a row is pinned to the version its labels were verified at =="
new_tree
pdb kube-system coredns '{matchLabels: {k8s-app: kube-dns}}'
row kube-system/coredns external:node-os 'The node OS installs the Deployment.'
printf '%s\n' 'node:' '  version: 1.1.0' >"$TREE/pins.yaml"
run_guard "$TREE" "$exceptions"
assert_rc 'a file pin that moved fails until the row is verified again' 1
assert_contains 'says what moved and what to do' 'its exception was verified at 1.0.0, and file:pins.yaml:.node.version now reads 1.1.0: verify the pod labels again at the source the row names, then set the verified version of the row to 1.1.0'
assert_not_contains 'does not except the budget meanwhile' 'is excepted'
row kube-system/coredns external:node-os 'The node OS installs the Deployment.' test file:pins.yaml:.node.version 1.1.0
run_guard "$TREE" "$exceptions"
assert_rc 'and passes once the row carries the new version' 0

operator_tree() { # <chart version>
  new_tree
  pdb observability server '{matchLabels: {app.kubernetes.io/component: server}}'
  add <<'EOT'
apiVersion: example.invalid/v1
kind: Server
metadata:
  name: server
  namespace: observability
EOT
  release observability operator web "$1" </dev/null
  row observability/server Server/observability/server 'The operator builds the StatefulSet.' test HelmRelease/observability/operator 1.0.0
}
operator_tree 1.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a row pinned to the chart version the cluster renders' 0
assert_contains 'reports the pin' 'verified at HelmRelease/observability/operator 1.0.0'
operator_tree 2.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a chart bump fails until the row is verified again' 1
assert_contains 'names the chart version the cluster renders now' 'its exception was verified at 1.0.0, and HelmRelease/observability/operator now reads 2.0.0'

operator_tree 1.0.0
row observability/server Server/observability/server 'The operator builds the StatefulSet.' test HelmRelease/observability/gone 1.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a pin naming a HelmRelease the cluster does not render' 2
assert_contains 'says the pin cannot be read' "is pinned to HelmRelease/observability/gone, whose chart version cannot be read"
row observability/server Server/observability/server 'The operator builds the StatefulSet.' test file:absent.yaml:.node.version 1.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a pin naming a file that does not exist' 2
assert_contains 'names the missing file' "is pinned to 'absent.yaml' beside"
row observability/server Server/observability/server 'The operator builds the StatefulSet.' test file:pins.yaml:.node.release 1.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a pin naming a path that holds nothing' 2
assert_contains 'says the path is empty' 'is pinned to file:pins.yaml:.node.release, which holds no version'
row observability/server Server/observability/server 'The operator builds the StatefulSet.' test file:pins.yaml:.node 1.0.0
run_guard "$TREE" "$exceptions"
assert_rc 'a pin naming a mapping, not a version' 2

echo "== stale and malformed rows =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
row demo/web external:node-os 'The node OS installs the Deployment.'
run_guard "$TREE" "$exceptions"
assert_rc 'a row for a budget a rendered workload satisfies is stale' 1
assert_contains 'names the stale row' 'stale exception: test: PodDisruptionBudget demo/web is satisfied by a rendered workload, or cluster test does not render it'

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
row demo/gone external:node-os 'The node OS installs the Deployment.'
run_guard "$TREE" "$exceptions"
assert_rc 'a row for a budget its cluster does not render is stale' 1
assert_contains 'names the row for the absent budget' 'stale exception: test: PodDisruptionBudget demo/gone'

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
# Each malformed row with the column the guard must name.
malformed() { # <what the guard must say> <columns...>
  local expected="$1"
  shift
  cols "$@" >"$exceptions"
  run_guard "$TREE" "$exceptions"
  assert_rc "a malformed row ($*)" 2
  assert_contains 'names the column' "exceptions.tsv:1: $expected"
}
good_pin='file:pins.yaml:.node.version'
malformed 'a row has seven tab-separated columns (cluster, budget, producer, pin, verified version, unrelated workloads, reason), this one has 3' demo/web external:node-os reason
malformed 'a row has seven tab-separated columns (cluster, budget, producer, pin, verified version, unrelated workloads, reason), this one has 8' test demo/web external:node-os "$good_pin" 1.0.0 - reason more
malformed 'column 1 must name the cluster the row applies to' Test demo/web external:node-os "$good_pin" 1.0.0 - reason
malformed "column 1 names 'base'" base demo/web external:node-os "$good_pin" 1.0.0 - reason
malformed 'column 2 must name a PodDisruptionBudget' test web external:node-os "$good_pin" 1.0.0 - reason
malformed "'demo/web' names no producer" test demo/web node-os "$good_pin" 1.0.0 - reason
malformed "'demo/web' names no producer" test demo/web Server/server "$good_pin" 1.0.0 - reason
malformed "'demo/web' names no version pin" test demo/web external:node-os pins.yaml 1.0.0 - reason
malformed "'demo/web' names no version pin" test demo/web external:node-os 'file:../pins.yaml:.node.version' 1.0.0 - reason
malformed "'demo/web' names no version pin" test demo/web external:node-os 'file:/etc/hosts:.node' 1.0.0 - reason
malformed "'demo/web' names no version pin" test demo/web external:node-os 'file:pins.yaml:.node | "1.0.0"' 1.0.0 - reason
malformed "'demo/web' names no version pin" test demo/web external:node-os HelmRelease/web 1.0.0 - reason
malformed "'demo/web' carries no verified version" test demo/web external:node-os "$good_pin" '' - reason
malformed "'demo/web' names no reviewed-unrelated workloads" test demo/web external:node-os "$good_pin" 1.0.0 '' reason
malformed "'demo/web' names no reviewed-unrelated workloads" test demo/web external:node-os "$good_pin" 1.0.0 web reason
malformed "'demo/web' carries no reason (column 7)" test demo/web external:node-os "$good_pin" 1.0.0 - ''
{
  cols test demo/web external:node-os "$good_pin" 1.0.0 - reason
  cols test demo/web external:node-os "$good_pin" 1.0.0 - again
} >"$exceptions"
run_guard "$TREE" "$exceptions"
assert_rc 'a budget listed twice for one cluster' 2
assert_contains 'says the row repeats' "'demo/web' is listed more than once for cluster 'test'"
printf '%s\n' '# a comment' '' >"$exceptions"
run_guard "$TREE" "$exceptions"
assert_rc 'comments and blank lines are not rows' 0
run_guard "$TREE" "$scratch/absent.tsv"
assert_rc 'a missing exceptions file' 2
assert_contains 'refuses to run without the list' 'refusing to run without the reviewed disposition list'

echo "== Flux fields that change what a directory renders =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i '.spec.targetNamespace = "moved"' "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE"
assert_rc 'targetNamespace moves the budget and its workload together' 0
assert_contains 'places the budget where Flux applies it' 'PodDisruptionBudget moved/web'

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i '.spec.patches = [{"target": {"kind": "Deployment", "name": "web"}, "patch": "- op: replace\n  path: /spec/template/metadata/labels/app.kubernetes.io~1name\n  value: patched\n"}]' \
  "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE"
assert_rc 'a Flux-level patch that relabels the pod template detaches the budget' 1
assert_contains 'shows the label Flux patched in' 'app.kubernetes.io/name=patched — 1 of 2 selector requirement(s) match'

echo "== every budget is decided on its own =="
new_tree
pdb demo web "$both"
pdb demo api '{matchLabels: {app.kubernetes.io/name: api}}'
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'one detached budget beside one that holds' 1
assert_contains 'names the detached budget' 'test: PodDisruptionBudget demo/api selects no workload rendered into namespace demo'
assert_not_contains 'does not report the one that holds as detached' 'PodDisruptionBudget demo/web selects no workload'
assert_contains 'reports the count' '1 cluster(s), 2 PodDisruptionBudget(s), 0 HelmRelease chart(s) rendered'

echo "== more than one cluster =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
cp -R "$TREE/k8s/clusters/test" "$TREE/k8s/clusters/second"
cp -R "$TREE/k8s/apps/test" "$TREE/k8s/apps/second"
yq -i '.spec.path = "./apps/second"' "$TREE/k8s/clusters/second/flux.yaml"
yq -i '(select(.kind == "PodDisruptionBudget") | .spec.selector.matchLabels["app.kubernetes.io/name"]) = "api"' \
  "$TREE/k8s/apps/second/objects.yaml"
run_guard "$TREE"
assert_rc 'a detached budget in one of two clusters' 1
assert_contains 'names the cluster that fails' 'second: PodDisruptionBudget demo/web selects no workload'
assert_contains 'and still reports the one that passes' 'test: PodDisruptionBudget demo/web selects Deployment web'

echo "== cannot-check is never clean =="
new_tree
workload Deployment demo web "$web_labels" 2
run_guard "$TREE"
assert_rc 'a tree that renders no PodDisruptionBudget at all' 2
assert_contains 'refuses an empty render' 'refusing to report an empty render as clean'

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i '(select(.kind == "PodDisruptionBudget") | .apiVersion) = "policy/v1beta1"' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a budget that is not policy/v1' 2
assert_contains 'names the budget it cannot read' "renders 'policy/v1beta1 PodDisruptionBudget demo/web', which is not policy/v1 or names no namespace"

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i 'del(select(.kind == "PodDisruptionBudget") | .metadata.namespace)' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a budget that names no namespace' 2
assert_contains 'names the budget it cannot place' "renders 'policy/v1 PodDisruptionBudget /web', which is not policy/v1 or names no namespace"

new_tree
pdb demo web "$both"
printf '%s\n' 'apiVersion: kustomize.config.k8s.io/v1beta1' 'kind: Kustomization' 'resources:' '  - config-map.yaml' \
  >"$TREE/k8s/clusters/test/kustomization.yaml"
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: cluster-meta' >"$TREE/k8s/clusters/test/config-map.yaml"
run_guard "$TREE"
assert_rc 'a cluster overlay that names no Flux Kustomization' 2
assert_contains 'says the overlay is empty' 'names no Flux Kustomization'

new_tree
pdb demo web "$both"
printf '%s\n' 'not: [valid' >>"$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a layer that does not render' 2
assert_contains 'says which layer' "for cluster 'test'"

new_tree
rm -rf "$TREE/k8s/clusters"
run_guard "$TREE"
assert_rc 'a root with no clusters directory' 2
assert_contains 'says the root is wrong' "clusters' is not a directory"

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
run_guard "$TREE" "$scratch/no-exceptions.tsv" --unknown
assert_rc 'an unknown option' 2
assert_contains 'names the option' "unknown option '--unknown'"
run_guard "$TREE" "$scratch/no-exceptions.tsv" --kube-version ''
assert_rc 'an empty --kube-version' 2
assert_contains 'prints the usage' '[--kube-version <version>] <k8s-root>'

echo "== Flagger: a Canary the chart itself renders =="
new_tree
flagger
pdb demo web "$both"
release demo web <<'EOT'
  values:
    canary: true
EOT
run_guard "$TREE"
assert_rc "#4365 from a chart: a selector naming the label Flagger rewrites" 1
assert_contains 'reads the Canary out of the chart render' 'it matches only Deployment web, which Flagger Canary web keeps at 0 replicas between rollouts; the pods that serve belong to web-primary'

new_tree
flagger
pdb demo web "$instance"
release demo web <<'EOT'
  values:
    canary: true
EOT
run_guard "$TREE"
assert_rc 'a selector on the label Flagger leaves intact, the Canary from the chart' 0
assert_contains 'selects the primary of the chart-rendered Canary' 'test: PodDisruptionBudget demo/web selects Deployment web-primary (the primary Flagger creates for Deployment web, HelmRelease demo/web)'

new_tree
flagger
pdb demo web "$both"
release demo web <<'EOT'
  values:
    canary: true
    canaryOnUpgradeOnly: true
EOT
run_guard "$TREE"
assert_rc 'a Canary the chart renders only on upgrade is read from the upgrade render' 1
assert_contains 'names the render the Canary detaches the budget in' 'selects no workload rendered into namespace demo in the upgrade render'

new_tree
pdb demo web "$instance"
release demo web <<'EOT'
  values:
    canary: true
EOT
run_guard "$TREE"
assert_rc 'a chart-rendered Canary with no flagger HelmRelease to read the selector labels from' 2
assert_contains 'says what is missing for the chart Canary' 'no flagger HelmRelease'

new_tree
flagger
pdb demo web "$instance"
release demo web <<'EOT'
  values:
    canary: true
    kind: StatefulSet
EOT
run_guard "$TREE"
assert_rc 'a chart-rendered Canary over a kind no primary is modelled for' 2
assert_contains 'names the kind of the chart Canary target' 'Canary demo/web targets a StatefulSet, which is not a kind this guard models a Flagger primary for'

echo "== every directory under clusters/ is a cluster, whatever its kustomization file is called =="
second_cluster() { # <kustomization file name>
  new_tree
  pdb demo web "$both"
  workload Deployment demo web "$web_labels" 2
  cp -R "$TREE/k8s/clusters/test" "$TREE/k8s/clusters/second"
  cp -R "$TREE/k8s/apps/test" "$TREE/k8s/apps/second"
  yq -i '.spec.path = "./apps/second"' "$TREE/k8s/clusters/second/flux.yaml"
  yq -i '(select(.kind == "PodDisruptionBudget") | .spec.selector.matchLabels["app.kubernetes.io/name"]) = "api"' \
    "$TREE/k8s/apps/second/objects.yaml"
  mv "$TREE/k8s/clusters/second/kustomization.yaml" "$TREE/k8s/clusters/second/$1"
}
for file in kustomization.yml Kustomization; do
  second_cluster "$file"
  run_guard "$TREE"
  assert_rc "an overlay built from $file is rendered, and its detached budget found" 1
  assert_contains "names the cluster built from $file" 'second: PodDisruptionBudget demo/web selects no workload'
  assert_contains "counts the cluster built from $file" '2 cluster(s), 2 PodDisruptionBudget(s)'
done

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
mkdir -p "$TREE/k8s/clusters/second"
printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: stray' >"$TREE/k8s/clusters/second/config-map.yaml"
run_guard "$TREE"
assert_rc 'a directory under clusters/ with no kustomization file' 2
assert_contains 'refuses to skip it' "clusters/second' holds no kustomization.yaml, kustomization.yml or Kustomization, so it cannot be rendered as a cluster overlay — refusing to skip it"

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
printf '%s\n' 'not a cluster' >"$TREE/k8s/clusters/README.md"
run_guard "$TREE"
assert_rc 'a plain file under clusters/ is not a cluster' 0

echo "== a chart pull is tried again, a bounded number of times =="
new_tree
pdb demo web "$both"
release demo web </dev/null
HELM_SHIM_FAIL_FIRST=2
export HELM_SHIM_FAIL_FIRST
run_guard "$TREE"
assert_rc 'a registry that answers on the third attempt' 0
assert_pulls 'pulled three times' 3
HELM_SHIM_FAIL_FIRST=3
run_guard "$TREE"
assert_rc 'a registry that never answers is cannot-check' 2
assert_contains 'leads with the cause and the attempts made' '  test: PodDisruptionBudget demo/web cannot be checked: HelmRelease demo/web cannot be rendered (cannot pull chart web from https://charts.example.invalid at 1.0.0 after 3 attempt(s): Error: connection reset by peer'
assert_pulls 'stops after the bound' 3
HELM_SHIM_FAIL_FIRST=1
PDB_GUARD_PULL_ATTEMPTS=1
export PDB_GUARD_PULL_ATTEMPTS
run_guard "$TREE"
assert_rc 'PDB_GUARD_PULL_ATTEMPTS=1 allows no second attempt' 2
assert_pulls 'pulled once' 1
PDB_GUARD_PULL_ATTEMPTS=0
run_guard "$TREE"
assert_rc 'a bound of zero attempts is refused' 2
assert_contains 'names the variable' 'PDB_GUARD_PULL_ATTEMPTS must be a whole number of at least 1'
unset PDB_GUARD_PULL_ATTEMPTS HELM_SHIM_FAIL_FIRST
PULL_BACKOFF=soon
run_guard "$TREE"
assert_rc 'a wait that is not a number of seconds is refused' 2
assert_contains 'names the backoff variable' 'PDB_GUARD_PULL_BACKOFF must be a whole number of seconds'
unset PULL_BACKOFF

new_tree
pdb demo web "$both"
release demo web missing-chart </dev/null
release demo api missing-chart </dev/null
yq -i '(select(.kind == "HelmRelease" and .metadata.name == "api") | .spec.chart.spec.sourceRef.name) = "web"' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'two releases of one chart that cannot be pulled' 2
assert_pulls 'the failed pull is not repeated for the second release' 3

echo "== every budget comes back with a verdict =="
# The decision is one jq program. A stand-in for jq alters only its output, to show the guard
# refuses a decision that lost a budget or returned a verdict it does not know.
mkdir -p "$scratch/jq-bin"
JQ_SHIM_REAL="$(command -v jq)"
export JQ_SHIM_REAL
cat >"$scratch/jq-bin/jq" <<'EOT'
#!/usr/bin/env bash
if [ -n "${JQ_SHIM_FILTER:-}" ]; then
  for arg in "$@"; do
    # Only the decision reads a `canaries` file.
    if [ "$arg" = canaries ]; then
      "$JQ_SHIM_REAL" "$@" | "$JQ_SHIM_REAL" -c "$JQ_SHIM_FILTER"
      exit "${PIPESTATUS[0]}"
    fi
  done
fi
exec "$JQ_SHIM_REAL" "$@"
EOT
chmod +x "$scratch/jq-bin/jq"
new_tree
pdb demo web "$both"
pdb demo api "$both"
workload Deployment demo web "$web_labels" 2
original_path="$PATH"
PATH="$scratch/jq-bin:$PATH"
export JQ_SHIM_FILTER='.'
run_guard "$TREE"
assert_rc 'the stand-in changes nothing when it passes the decision through' 0
JQ_SHIM_FILTER='.[1:]'
run_guard "$TREE"
assert_rc 'a decision that lost a budget is never clean' 2
assert_contains 'says how many verdicts came back' 'renders 2 PodDisruptionBudget(s) but 1 of 1 verdict(s) are known — refusing to report the rest as clean'
JQ_SHIM_FILTER='.[0].status = "skipped"'
run_guard "$TREE"
assert_rc 'a verdict the guard does not know is never clean' 2
assert_contains 'counts only the verdicts it knows' 'renders 2 PodDisruptionBudget(s) but 1 of 2 verdict(s) are known — refusing to report the rest as clean'
unset JQ_SHIM_FILTER
PATH="$original_path"

echo "== what Flux and Helm leave out of, or refuse in, a release =="
new_tree
pdb demo web '{matchLabels: {app: hook}}'
release demo web <<'EOT'
  values:
    hook: true
EOT
run_guard "$TREE"
assert_rc 'a workload that is a Helm hook is not part of the release' 1
assert_contains 'lists the released Deployment' 'nearest:  Deployment web (HelmRelease demo/web)'
assert_not_contains 'never lists the hook' 'web-hook'

new_tree
pdb demo web "$both"
release demo web <<'EOT'
  upgrade:
    preserveValues: true
EOT
run_guard "$TREE"
assert_rc 'upgrade.preserveValues needs values this guard does not have' 2
assert_contains 'says why preserveValues cannot be rendered' 'HelmRelease demo/web cannot be rendered (spec.upgrade.preserveValues needs historical release values)'

new_tree
pdb demo web "$both"
release demo web <<'EOT'
  postRenderStrategy: Latest
EOT
run_guard "$TREE"
assert_rc 'postRenderStrategy is not modelled' 2
assert_contains 'says postRenderStrategy is not modelled' 'HelmRelease demo/web cannot be rendered (spec.postRenderStrategy is not modelled)'

new_tree
pdb demo web "$both"
release demo web </dev/null
yq -i '(select(.kind == "HelmRelease") | .spec.chart.spec.valuesFiles) = ["values-prod.yaml"]' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'valuesFiles are not rendered' 2
assert_contains 'says valuesFiles are not rendered' 'HelmRelease demo/web cannot be rendered (spec.chart.spec.valuesFiles is not rendered)'

new_tree
pdb demo web '{matchLabels: {app.kubernetes.io/name: api}}'
add <<'EOT'
apiVersion: v1
kind: ConfigMap
metadata:
  name: web-values
  namespace: demo
data:
  workload-name: api
EOT
release demo web <<'EOT'
  valuesFrom:
    - kind: ConfigMap
      name: web-values
      valuesKey: workload-name
      targetPath: name
EOT
run_guard "$TREE"
assert_rc 'a valuesFrom entry with a targetPath is merged from its ConfigMap' 0

echo "== substitution leaves an object annotated substitute: disabled alone =="
literal_tree() { # <annotation value>
  new_tree
  pdb demo web "$both"
  add <<EOT
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
  annotations:
    kustomize.toolkit.fluxcd.io/substitute: $1
spec:
  selector:
    matchLabels: {app.kubernetes.io/instance: web}
  template:
    metadata:
      labels: {app.kubernetes.io/name: "\${web_name}", app.kubernetes.io/instance: web}
    spec:
      containers:
        - name: main
          image: registry.example.invalid/main:1.0.0
EOT
}
literal_tree enabled
run_guard "$TREE"
assert_rc 'a label Flux substitutes satisfies the budget' 0
literal_tree disabled
run_guard "$TREE"
assert_rc 'a label Flux leaves literal does not' 1
# shellcheck disable=SC2016 # the literal the cluster would hold, not a shell expansion
assert_contains 'shows the literal label' 'app.kubernetes.io/name=${web_name} — 1 of 2 selector requirement(s) match'

echo "== chart sources =="
assert_pulled() { # <label> <ref|repo|version>
  assertions=$((assertions + 1))
  if grep -qxF -- "$2" "$HELM_SHIM_LOG"; then ok "$1"; else bad "$1: expected the pull '$2', got: $(tr '\n' ' ' <"$HELM_SHIM_LOG")"; fi
}
# A HelmRelease that takes its chart from an OCIRepository through chartRef. The reference comes
# from stdin as the lines of spec.ref.
oci_release() { # <namespace> <name> [url]
  {
    printf '%s\n' 'apiVersion: source.toolkit.fluxcd.io/v1' 'kind: OCIRepository' 'metadata:' "  name: $2" \
      "  namespace: $1" 'spec:' "  url: ${3:-oci://registry.example.invalid/charts/web}"
    cat
    printf '%s\n' '---' 'apiVersion: helm.toolkit.fluxcd.io/v2' 'kind: HelmRelease' 'metadata:' "  name: $2" \
      "  namespace: $1" 'spec:' '  interval: 10m' '  chartRef:' '    kind: OCIRepository' "    name: $2"
  } | add
}
digest='sha256:0000000000000000000000000000000000000000000000000000000000000000'
new_tree
pdb demo web "$both"
oci_release demo web <<EOT
  ref:
    digest: $digest
    tag: 9.9.9
EOT
run_guard "$TREE"
assert_rc 'a chartRef to an OCIRepository pinned by digest' 0
assert_pulled 'pulls by the digest, which wins over the tag' "oci://registry.example.invalid/charts/web@$digest||"

for ref in tag semver; do
  new_tree
  pdb demo web "$both"
  oci_release demo web <<EOT
  ref:
    $ref: 1.2.3
EOT
  run_guard "$TREE"
  assert_rc "a chartRef to an OCIRepository pinned by $ref" 0
  assert_pulled "pulls the $ref as the version" 'oci://registry.example.invalid/charts/web||1.2.3'
done

new_tree
pdb demo web "$both"
oci_release demo web <<'EOT'
  ref:
    semver: ">=1.0.0"
    semverFilter: ".*-rc.*"
EOT
run_guard "$TREE"
assert_rc 'an OCIRepository that filters semver tags' 2
assert_contains 'says helm pull cannot filter' 'OCIRepository web filters semver tags, which helm pull cannot'

new_tree
pdb demo web "$both"
oci_release demo web </dev/null
run_guard "$TREE"
assert_rc 'an OCIRepository that names no reference' 2
assert_contains 'says no reference is named' 'OCIRepository web names no digest, semver or tag'

new_tree
pdb demo web "$both"
oci_release demo web https://registry.example.invalid/charts/web <<'EOT'
  ref:
    tag: 1.2.3
EOT
run_guard "$TREE"
assert_rc 'an OCIRepository without an oci:// URL' 2
assert_contains 'says the URL is not OCI' 'OCIRepository web has no oci:// URL'

new_tree
pdb demo web "$both"
release demo web </dev/null
yq -i '(select(.kind == "HelmRepository") | .spec) = {"type": "oci", "url": "oci://registry.example.invalid/charts/"}' \
  "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a HelmRepository of type oci' 0
assert_pulled 'pulls the chart as an OCI reference under the repository URL' 'oci://registry.example.invalid/charts/web||1.0.0'

new_tree
pdb demo web "$both"
release demo web </dev/null
run_guard "$TREE"
assert_pulled 'a plain HelmRepository is pulled by chart name from its URL' 'web|https://charts.example.invalid|1.0.0'

new_tree
pdb demo web "$both"
release demo web </dev/null
yq -i '(select(.kind == "HelmRelease") | .spec.chart.spec.sourceRef.kind) = "GitRepository"' "$TREE/k8s/apps/test/objects.yaml"
run_guard "$TREE"
assert_rc 'a chart source that nothing renders' 2
assert_contains 'names the source that is missing' 'its chart source GitRepository/demo/web is not rendered in this cluster'
add <<'EOT'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: web
  namespace: demo
spec:
  url: https://git.example.invalid/web
EOT
run_guard "$TREE"
assert_rc 'a chart source of a kind helm pull cannot read' 2
assert_contains 'names the kind' 'its chart source kind GitRepository is not a HelmRepository or OCIRepository'

echo "== a release name longer than 53 characters is shortened as Flux shortens it =="
long_namespace='a-namespace-name-long-enough-to-overflow-a-release'
full_name="$long_namespace-website"
if command -v sha256sum >/dev/null 2>&1; then
  name_hash="$(printf '%s' "$full_name" | sha256sum)"
else
  name_hash="$(printf '%s' "$full_name" | shasum -a 256)"
fi
short_name="${full_name:0:40}-${name_hash:0:12}"
new_tree
pdb "$long_namespace" website "{matchLabels: {app.kubernetes.io/instance: $short_name}}"
release other website <<EOT
  targetNamespace: $long_namespace
EOT
run_guard "$TREE"
assert_rc "a selector naming the shortened release name (${#full_name} characters shortened to ${#short_name})" 0
new_tree
pdb "$long_namespace" website "{matchLabels: {app.kubernetes.io/instance: $full_name}}"
release other website <<EOT
  targetNamespace: $long_namespace
EOT
run_guard "$TREE"
assert_rc 'a selector naming the unshortened release name' 1
assert_contains 'shows the shortened name the chart rendered' "app.kubernetes.io/instance=$short_name"

echo "== more Flux fields that change what a directory renders =="
new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i '.spec.namePrefix = "pre-" | .spec.nameSuffix = "-post"' "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE"
assert_rc 'namePrefix and nameSuffix rename the budget and its workload together' 0
assert_contains 'names both as Flux applies them' 'PodDisruptionBudget demo/pre-web-post selects Deployment pre-web-post'

component_tree() { # <spec.components entry>
  new_tree
  pdb demo web "$both"
  workload Deployment demo web "$web_labels" 2
  mkdir -p "$TREE/k8s/apps/test/components/relabel" "$TREE/outside"
  cat >"$TREE/k8s/apps/test/components/relabel/kustomization.yaml" <<'EOT'
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
patches:
  - target:
      kind: Deployment
      name: web
    patch: |
      - op: replace
        path: /spec/template/metadata/labels/app.kubernetes.io~1name
        value: from-component
EOT
  cp "$TREE/k8s/apps/test/components/relabel/kustomization.yaml" "$TREE/outside/kustomization.yaml"
  COMPONENT="$1" yq -i '.spec.components = [strenv(COMPONENT)]' "$TREE/k8s/clusters/test/flux.yaml"
}
component_tree components/relabel
run_guard "$TREE"
assert_rc 'a Flux component that relabels the pod template detaches the budget' 1
assert_contains 'shows the label the component patched in' 'app.kubernetes.io/name=from-component — 1 of 2 selector requirement(s) match'
component_tree components/absent
run_guard "$TREE"
assert_rc 'a Flux component that does not exist' 2
assert_contains 'names the missing component' "component 'components/absent' named for Flux path 'apps/test' does not exist"
component_tree ../../../outside
run_guard "$TREE"
assert_rc 'a Flux component outside the root' 2
assert_contains 'says the component leaves the root' "component '../../../outside' named for Flux path 'apps/test' leaves"

for field in patchesStrategicMerge patchesJson6902; do
  new_tree
  pdb demo web "$both"
  workload Deployment demo web "$web_labels" 2
  FIELD="$field" yq -i '.spec[strenv(FIELD)] = []' "$TREE/k8s/clusters/test/flux.yaml"
  run_guard "$TREE"
  assert_rc "a Flux Kustomization that sets $field is refused" 2
  assert_contains "says $field is not applied" 'uses patchesStrategicMerge or patchesJson6902, which this guard does not apply'
done

new_tree
pdb demo web "$both"
workload Deployment demo web "$web_labels" 2
yq -i '.spec.path = "../outside"' "$TREE/k8s/clusters/test/flux.yaml"
run_guard "$TREE"
assert_rc 'a Flux path outside the root' 2
assert_contains 'says the path leaves the root' "Flux path '../outside' named by"

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
printf 'PASS: the PodDisruptionBudget selector guard passes a matching budget, fails a detached one, follows Flagger and chart renders, and refuses what it cannot decide\n'
