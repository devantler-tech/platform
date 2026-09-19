#!/usr/bin/env bash
# The privileged-RBAC policy excludes roles by the exact names their charts render.
# Those charts render in-cluster through Flux, so nothing else evaluates their RBAC
# before admission: a chart bump that renames an excluded role, broadens one, or adds
# a privileged one would pass its own PR and then fail or silently widen the cluster.
# This test renders each chart the way production does: from the production
# controllers overlay, with the production Flux substitutions, at the production
# Kubernetes version, as both an install and an upgrade. It checks the rendered RBAC
# four ways:
#   1. the Enforce policy passes every role it evaluates;
#   2. the policy's exclusions split exactly into chart-rendered roles and an explicit
#      list of roles no chart renders;
#   3. every excluded chart role still grants exactly its reviewed rules, and an
#      aggregated one still collects exactly its reviewed contributors, whether a
#      chart renders them or this repository commits them;
#   4. the excluded Kubernetes built-in aggregated roles, admin and edit, still collect
#      exactly their reviewed contributors from the ClusterRoles charts render and this
#      repository commits. ClusterRoles a controller creates at runtime are out of
#      scope: nothing here renders them.
# Anything the render cannot model the way Flux does fails closed.
# Run with UPDATE_BASELINE=1 to record reviewed grants after checking the diff.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/audit-privileged-rbac.yaml"
overlay="${repo_root}/k8s/providers/hetzner/infrastructure/controllers"
baseline="${repo_root}/tests/chart-rendered-rbac-policy/excluded-role-grants.json"
builtin_baseline="${repo_root}/tests/chart-rendered-rbac-policy/builtin-aggregate-contributors.json"
prod_config="${repo_root}/ksail.prod.yaml"
# Flux substitutes from these ConfigMaps in this order, later entries winning, and
# then from the SOPS-encrypted variables-cluster Secret, which this test cannot read.
variable_sources=(
  "${repo_root}/k8s/bases/bootstrap/config-map.yaml"
  "${repo_root}/k8s/clusters/prod/bootstrap/config-map.yaml"
)

# The HelmReleases, as the production overlay builds them, whose rendered roles the
# policy may exclude.
releases=(crossplane kro ksail-operator longhorn velero)

# Exclusions for roles no chart here renders: Kubernetes built-ins and roles this
# repository commits. Every other policy exclusion must be rendered by a chart above.
non_chart_exclusions='[
  "ClusterRole||admin",
  "ClusterRole||cluster-admin",
  "ClusterRole||edit",
  "ClusterRole||kro-tenant-rgd",
  "ClusterRole||system:aggregate-to-edit",
  "ClusterRole||system:controller:clusterrole-aggregation-controller"
]'

# Flux substitutions the watched HelmReleases may carry. Each sets only a replica
# count, backup storage or an OIDC setting. They resolve to their production values
# where a ConfigMap above holds one; a value held only in the encrypted Secret
# renders as a placeholder, which is why every token must be reviewed as unable to
# change which roles render or what they grant. Any other token fails closed.
allowed_tokens='[
  "dex_client_secret",
  "domain",
  "ksail_operator_replicas",
  "longhorn_csi_attacher_replicas",
  "longhorn_csi_provisioner_replicas",
  "longhorn_csi_resizer_replicas",
  "longhorn_csi_snapshotter_replicas",
  "longhorn_replica_count",
  "longhorn_ui_replicas",
  "r2_bucket",
  "r2_endpoint",
  "r2_prefix_velero",
  "r2_region",
  "velero_replicas"
]'

for tool in awk helm jq kubectl kyverno yq; do
  command -v "$tool" >/dev/null || {
    printf 'FAIL: %s is required\n' "$tool" >&2
    exit 1
  }
done

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# Prints a failure message and exits the test.
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

kube_version="$(yq -r '.spec.cluster.kubernetesVersion' "$prod_config")"
case "$kube_version" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) fail "could not read spec.cluster.kubernetesVersion from ksail.prod.yaml (got '${kube_version}')" ;;
esac

# The production substitution variables, later sources overriding earlier ones.
for source in "${variable_sources[@]}"; do
  yq -o=json '.data // {}' "$source"
done |
  jq -s -r 'add | to_entries[]
    | select((.value | type) == "string" and (.value | test("[\t\n]") | not))
    | "\(.key)\t\(.value)"' >"${scratch}/variables.tsv"
[ -s "${scratch}/variables.tsv" ] || fail "no production substitution variables were read"

# Resolves Flux substitutions as production does: a ConfigMap variable wins, then an
# inline ${name:=default}, then a placeholder for a value held only in the Secret.
substitute() {
  awk -F '\t' '
    FNR == NR { vars[$1] = substr($0, length($1) + 2); next }
    {
      line = $0
      out = ""
      while (match(line, /[$][{][A-Za-z0-9_]+(:=[^}]*)?[}]/)) {
        token = substr(line, RSTART + 2, RLENGTH - 3)
        name = token
        value = "placeholder"
        split_at = index(token, ":=")
        if (split_at > 0) {
          name = substr(token, 1, split_at - 1)
          value = substr(token, split_at + 2)
        }
        if (name in vars) value = vars[name]
        out = out substr(line, 1, RSTART - 1) value
        line = substr(line, RSTART + RLENGTH)
      }
      print out line
    }' "${scratch}/variables.tsv" "$1"
}

kubectl kustomize "$overlay" >"${scratch}/overlay-raw.yaml"

for name in "${releases[@]}"; do
  unexpected="$(yq -o=json "select(.kind == \"HelmRelease\" and .metadata.name == \"${name}\")" \
    "${scratch}/overlay-raw.yaml" |
    { grep -oE '\$\{[A-Za-z0-9_]+' || true; } | sed 's/^\${//' | sort -u |
    jq -R -s -r --argjson allowed "$allowed_tokens" 'split("\n") | map(select(length > 0)) - $allowed | .[]')"
  [ -z "$unexpected" ] ||
    fail "${name}: Flux substitutions this test cannot resolve safely: ${unexpected//$'\n'/, }; list them in allowed_tokens only if they cannot affect RBAC"
done

substitute "${scratch}/overlay-raw.yaml" >"${scratch}/overlay.yaml"

# Keeps only the Roles and ClusterRoles of one render, placing a namespaced one in the
# release namespace when its template leaves the namespace unset, as Helm installs it.
extract_rbac() {
  local rendered="$1" namespace="$2"
  yq "select(.kind == \"Role\" or .kind == \"ClusterRole\")
      | (select(.kind == \"Role\" and (.metadata.namespace // \"\") == \"\") | .metadata.namespace) = \"${namespace}\"" \
    "$rendered"
}

# Renders one HelmRelease from the built overlay with its effective values, as both
# an install and an upgrade, into <work>/rbac.yaml, keeping only its Roles and
# ClusterRoles.
render() {
  local name="$1" work="$2"
  mkdir -p "$work"

  yq -o=json "select(.kind == \"HelmRelease\" and .metadata.name == \"${name}\")" \
    "${scratch}/overlay.yaml" >"${work}/release.json"
  [ "$(jq -s length "${work}/release.json")" -eq 1 ] ||
    fail "${name}: expected exactly one HelmRelease in the production controllers overlay"

  local chart version release namespace source_kind source_name source_namespace url
  chart="$(jq -r '.spec.chart.spec.chart' "${work}/release.json")"
  version="$(jq -r '.spec.chart.spec.version' "${work}/release.json")"
  release="$(jq -r '.spec.releaseName // .metadata.name' "${work}/release.json")"
  namespace="$(jq -r '.spec.targetNamespace // .metadata.namespace' "${work}/release.json")"
  source_kind="$(jq -r '.spec.chart.spec.sourceRef.kind' "${work}/release.json")"
  source_name="$(jq -r '.spec.chart.spec.sourceRef.name' "${work}/release.json")"
  source_namespace="$(jq -r '.spec.chart.spec.sourceRef.namespace // .metadata.namespace' "${work}/release.json")"
  [ "$source_kind" = "HelmRepository" ] || fail "${name}: chart source is not a HelmRepository"
  url="$(yq -r "select(.kind == \"HelmRepository\" and .metadata.name == \"${source_name}\"
      and .metadata.namespace == \"${source_namespace}\") | .spec.url" "${scratch}/overlay.yaml")"

  # jq and yq print "null" for a missing field, so an empty check alone passes it.
  local field
  for field in "$chart" "$version" "$release" "$namespace" "$url"; do
    if [ -z "$field" ] || [ "$field" = "null" ]; then
      fail "${name}: could not read chart, version, release, namespace and repository"
    fi
  done

  # The render skips Flux post-renderers, which is exact for RBAC only while every
  # post-renderer is a kustomize patch or image override whose patches each name an
  # explicit kind outside the RBAC API. A patch without a kind could select a role by
  # name alone, so it fails closed too.
  jq -e '
    (.spec.postRenderers // []) | all(.[];
      keys == ["kustomize"] and
      (.kustomize | keys - ["images", "patches"] | length) == 0 and
      ((.kustomize.patches // []) | all(.[];
        (.target.kind // "") as $kind |
        (.target.group // "") as $group |
        $kind != "" and
        ($kind | test("^(Cluster)?Role(Binding)?$") | not) and
        $group != "rbac.authorization.k8s.io")))
  ' "${work}/release.json" >/dev/null ||
    fail "${name}: a post-renderer is not an explicit non-RBAC kustomize patch; render it through the post-renderer"

  # Flux merges spec.valuesFrom before spec.values, and this render reads only the
  # inline values, so a release that sources values elsewhere fails closed.
  jq -e '(.spec.valuesFrom // []) | length == 0' "${work}/release.json" >/dev/null ||
    fail "${name}: spec.valuesFrom is not rendered by this test; merge it before evaluating the policy"

  jq '.spec.values // {}' "${work}/release.json" >"${work}/values.json"
  case "$url" in
    oci://*) helm pull "${url}/${chart}" --version "$version" --destination "$work" >/dev/null ;;
    *) helm pull "$chart" --repo "$url" --version "$version" --destination "$work" >/dev/null ;;
  esac

  # A chart can condition RBAC on .Release.IsUpgrade, and Flux upgrades an existing
  # release when its chart version changes, so both renders are evaluated.
  helm template "$release" "${work}/${chart}-${version}.tgz" \
    --namespace "$namespace" --kube-version "$kube_version" \
    --values "${work}/values.json" >"${work}/rendered-install.yaml"
  helm template "$release" "${work}/${chart}-${version}.tgz" \
    --namespace "$namespace" --kube-version "$kube_version" --is-upgrade \
    --values "${work}/values.json" >"${work}/rendered-upgrade.yaml"
  extract_rbac "${work}/rendered-install.yaml" "$namespace" >"${work}/rbac-install.yaml"
  extract_rbac "${work}/rendered-upgrade.yaml" "$namespace" >"${work}/rbac-upgrade.yaml"

  # The same role must grant the same thing in both renders, or the baseline could not
  # say which one it reviewed; a role present in only one render is kept.
  yq ea -o=json '[select(.kind == "Role" or .kind == "ClusterRole")]' \
    "${work}/rbac-install.yaml" "${work}/rbac-upgrade.yaml" >"${work}/rbac-both.json"
  jq -e '
    def normalise: walk(if type == "array" then sort else . end);
    group_by("\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)")
    | all(.[]; map({rules, aggregationRule, labels: .metadata.labels} | normalise) | unique | length == 1)
  ' "${work}/rbac-both.json" >/dev/null ||
    fail "${name}: a role grants different rules, aggregation or labels on install and upgrade; review both before evaluating the policy"
  jq 'unique_by("\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)")' "${work}/rbac-both.json" |
    yq -p=json -o=yaml '.[] | split_doc' >"${work}/rbac.yaml"

  local count
  count="$(yq ea '[select(.kind == "Role" or .kind == "ClusterRole")] | length' "${work}/rbac.yaml")"
  [ "$count" -gt 0 ] || fail "${name}: ${chart} ${version} rendered no Role or ClusterRole"
  printf '%s %s rendered %s roles for Kubernetes %s (install and upgrade)\n' "$chart" "$version" "$count" "$kube_version"
}

# Writes "pass fail warn error skip" from one kyverno apply run to the census file
# and returns kyverno's exit status.
census() {
  local resources="$1" output="$2" numbers="$3" status=0
  kyverno apply "$policy" --resource "$resources" >"$output" 2>&1 || status=$?
  local line
  line="$(grep -Eo 'pass: [0-9]+, fail: [0-9]+, warn: [0-9]+, error: [0-9]+, skip: [0-9]+' "$output" || true)"
  [ "$(printf '%s\n' "$line" | grep -c .)" -eq 1 ] || {
    cat "$output" >&2
    fail "kyverno apply did not report exactly one result census"
  }
  # read needs the trailing newline that tr strips, or it fails under set -e.
  printf '%s\n' "$(printf '%s' "$line" | tr -dc '0-9 ' | tr -s ' ')" >"$numbers"
  return "$status"
}

# Prints each excluded chart role's grants, normalised so order does not matter.
# An aggregated role's own rules are empty; its effective grants are the rules of
# every ClusterRole its selectors match, so those contributors are recorded too.
excluded_grants() {
  local resources="$1" committed="$2"
  jq -S -n \
    --argjson excluded "$chart_exclusions" \
    --slurpfile rendered <(yq ea -o=json '[select(.kind == "Role" or .kind == "ClusterRole")]' "$resources") \
    --slurpfile committed <(yq ea -o=json '[select(.kind == "ClusterRole")]' "$committed") '
      def normalise: walk(if type == "array" then sort else . end);
      def identity: "\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)";
      def grants: {rules: (.rules // []), aggregationRule: (.aggregationRule // null)} | normalise;
      def selects($role): ($role.metadata.labels // {}) as $labels
        | all(.matchLabels | to_entries[]; $labels[.key] == .value);
      $rendered[0] as $rendered_roles
      | ($rendered_roles + $committed[0]) as $all
      | [$rendered_roles[]
        | select(identity as $id | $excluded | index($id))
        | . as $role
        | {
          key: identity,
          value: (grants + (if .aggregationRule == null then {} else {
            contributors: ([$all[]
              | select(.kind == "ClusterRole")
              | . as $candidate
              | select(any($role.aggregationRule.clusterRoleSelectors[]; selects($candidate)))
              | {key: identity, value: grants}] | from_entries)
          } end))
        }]
      | from_entries'
}

: >"${scratch}/rbac.yaml"
for name in "${releases[@]}"; do
  work="${scratch}/${name}"
  render "$name" "$work"
  {
    printf -- '---\n'
    cat "${work}/rbac.yaml"
  } >>"${scratch}/rbac.yaml"
done

# Committed ClusterRoles, which can contribute to an aggregated excluded role.
find "${repo_root}/k8s" -name '*.yaml' ! -name '*.enc.yaml' -print0 |
  { xargs -0 grep -l '^kind: ClusterRole *$' || true; } >"${scratch}/committed-files.txt"
: >"${scratch}/committed-raw.yaml"
while IFS= read -r file; do
  {
    printf -- '---\n'
    yq 'select(.kind == "ClusterRole")' "$file"
  } >>"${scratch}/committed-raw.yaml"
done <"${scratch}/committed-files.txt"
substitute "${scratch}/committed-raw.yaml" >"${scratch}/committed.yaml"
[ "$(yq ea '[select(.kind == "ClusterRole")] | length' "${scratch}/committed.yaml")" -gt 0 ] ||
  fail "no committed ClusterRole was found; contributors to aggregated roles cannot be checked"

# The policy's exclusion inventory, as kind|namespace|name.
yq -o=json '.' "$policy" |
  jq '.spec.rules[0].preconditions.all[0].value | sort' >"${scratch}/policy-exclusions.json"
yq ea -o=json '[select(.kind == "Role" or .kind == "ClusterRole")]' "${scratch}/rbac.yaml" |
  jq '[.[] | "\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)"] | unique' >"${scratch}/rendered.json"

stale="$(jq -r --argjson listed "$non_chart_exclusions" '$listed - . | .[]' "${scratch}/policy-exclusions.json")"
[ -z "$stale" ] ||
  fail "non-chart exclusions no longer in the policy, update this test: ${stale//$'\n'/, }"
rendered_non_chart="$(jq -r --argjson listed "$non_chart_exclusions" '. as $rendered | $listed | map(select(. as $id | $rendered | index($id))) | .[]' "${scratch}/rendered.json")"
[ -z "$rendered_non_chart" ] ||
  fail "exclusions listed as non-chart are rendered by a chart, move them to the reviewed grants: ${rendered_non_chart//$'\n'/, }"

chart_exclusions="$(jq -c --argjson listed "$non_chart_exclusions" '. - $listed' "${scratch}/policy-exclusions.json")"
missing="$(jq -r --argjson expected "$chart_exclusions" '. as $rendered | $expected - $rendered | .[]' "${scratch}/rendered.json")"
[ -z "$missing" ] ||
  fail "policy exclusions no chart renders, review the exclusion or list it as non-chart: ${missing//$'\n'/, }"
[ "$(jq length <<<"$chart_exclusions")" -gt 0 ] || fail "no chart-rendered exclusion was derived from the policy"

# Contributors are resolved only through non-empty matchLabels selectors. Any other
# selector form, or an empty one that would select every ClusterRole, fails closed.
yq ea -o=json '[select(.kind == "ClusterRole")]' "${scratch}/rbac.yaml" |
  jq -e --argjson excluded "$chart_exclusions" '
    [.[] | select("ClusterRole||\(.metadata.name)" as $id | $excluded | index($id))
      | select(.aggregationRule != null) | .aggregationRule]
    | all(.[];
      keys == ["clusterRoleSelectors"] and (.clusterRoleSelectors | length) > 0 and
      all(.clusterRoleSelectors[]; keys == ["matchLabels"] and (.matchLabels | length) > 0))
  ' >/dev/null ||
  fail "an excluded aggregated role uses a selector form this test cannot resolve; only non-empty matchLabels selectors are supported"

# The Kubernetes built-in aggregated ClusterRoles the policy excludes, with every
# aggregation label whose contributors reach them. The API server fixes these
# selectors, so no manifest here declares them: admin selects aggregate-to-admin,
# edit selects aggregate-to-edit, the built-in edit carries aggregate-to-admin, and
# the built-in view carries aggregate-to-edit. So a view contributor widens edit and
# admin too, and an edit contributor widens admin.
builtin_aggregates='{
  "ClusterRole||admin": [
    "rbac.authorization.k8s.io/aggregate-to-admin",
    "rbac.authorization.k8s.io/aggregate-to-edit",
    "rbac.authorization.k8s.io/aggregate-to-view"
  ],
  "ClusterRole||edit": [
    "rbac.authorization.k8s.io/aggregate-to-edit",
    "rbac.authorization.k8s.io/aggregate-to-view"
  ]
}'

# Prints the contributors to each excluded built-in aggregated ClusterRole, with their
# grants and aggregation labels, normalised so order does not matter.
builtin_contributors() {
  local resources="$1" committed="$2"
  jq -S -n \
    --argjson aggregates "$builtin_aggregates" \
    --slurpfile rendered <(yq ea -o=json '[select(.kind == "ClusterRole")]' "$resources") \
    --slurpfile committed <(yq ea -o=json '[select(.kind == "ClusterRole")]' "$committed") '
      def normalise: walk(if type == "array" then sort else . end);
      def identity: "\(.kind)|\(.metadata.namespace // "")|\(.metadata.name)";
      def grants: {rules: (.rules // []), aggregationRule: (.aggregationRule // null)} | normalise;
      def aggregation_labels: [(.metadata.labels // {}) | to_entries[]
        | select(.key | startswith("rbac.authorization.k8s.io/aggregate-to-"))
        | "\(.key)=\(.value | tostring)"] | sort;
      ($rendered[0] + $committed[0]) as $all
      | $aggregates
      | with_entries(.value as $labels | .value = ([$all[]
          | . as $role
          | select(any($labels[]; (($role.metadata.labels // {})[.] | tostring) == "true"))
          | {key: identity, value: (grants + {labels: aggregation_labels})}]
        | from_entries))'
}

excluded_grants "${scratch}/rbac.yaml" "${scratch}/committed.yaml" >"${scratch}/grants.json"
builtin_contributors "${scratch}/rbac.yaml" "${scratch}/committed.yaml" >"${scratch}/builtin.json"
if [ "${UPDATE_BASELINE:-}" = "1" ]; then
  mkdir -p "$(dirname "$baseline")"
  cp "${scratch}/grants.json" "$baseline"
  cp "${scratch}/builtin.json" "$builtin_baseline"
  printf 'Recorded %s excluded role grants in %s and %s built-in aggregate contributor entries in %s; review the diff before committing.\n' \
    "$(jq length "$baseline")" "${baseline#"${repo_root}"/}" \
    "$(jq '[.[] | length] | add // 0' "$builtin_baseline")" "${builtin_baseline#"${repo_root}"/}"
  exit 0
fi

[ -f "$baseline" ] ||
  fail "no reviewed grants baseline; run UPDATE_BASELINE=1 bash scripts/tests/test-chart-rendered-rbac-policy.sh and review it"
if ! diff -u "$baseline" "${scratch}/grants.json" >"${scratch}/grants.diff"; then
  cat "${scratch}/grants.diff" >&2
  fail "excluded chart roles grant different rules than reviewed; review the diff above, then run UPDATE_BASELINE=1 bash scripts/tests/test-chart-rendered-rbac-policy.sh"
fi
[ "$(jq -S 'keys' "$baseline")" = "$(jq -S --argjson expected "$chart_exclusions" -n '$expected | sort')" ] ||
  fail "the grants baseline does not cover exactly the chart-rendered policy exclusions"

# Negative control: broadening one excluded role must not match the baseline, or the
# comparison above proves nothing.
jq -S 'to_entries | .[0].value.rules += [{"apiGroups": ["*"], "resources": ["*"], "verbs": ["*"]}] | from_entries' \
  "${scratch}/grants.json" >"${scratch}/broadened.json"
if diff -q "$baseline" "${scratch}/broadened.json" >/dev/null; then
  fail "a broadened excluded role still matched the reviewed grants"
fi

status=0
census "${scratch}/rbac.yaml" "${scratch}/result.txt" "${scratch}/result.census" || status=$?
read -r pass failed _ errored _ <"${scratch}/result.census"
if [ "$status" -ne 0 ] || [ "$failed" -ne 0 ] || [ "$errored" -ne 0 ]; then
  cat "${scratch}/result.txt" >&2
  fail "chart-rendered RBAC violates the privileged-RBAC policy (pass ${pass}, fail ${failed}, error ${errored})"
fi
[ "$pass" -gt 0 ] || fail "no rendered role was evaluated; the census is vacuous"

# Negative control: the same render plus one privileged role the policy does not
# exclude, as a chart bump would add. It must fail, or the check above proves nothing.
{
  cat "${scratch}/rbac.yaml"
  cat <<'EOF'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: longhorn-role-renamed
rules:
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["bind"]
EOF
} >"${scratch}/negative.yaml"
negative_status=0
census "${scratch}/negative.yaml" "${scratch}/negative.txt" "${scratch}/negative.census" || negative_status=$?
read -r _ negative_failed _ _ _ <"${scratch}/negative.census"
if [ "$negative_status" -ne 1 ] || [ "$negative_failed" -ne 1 ] ||
  ! grep -Fq 'longhorn-role-renamed' "${scratch}/negative.txt"; then
  cat "${scratch}/negative.txt" >&2
  fail "an unexcluded privileged role in the render was not refused"
fi

# The built-in aggregated roles must still be policy exclusions listed as non-chart
# roles, or their contributors are being recorded for an exclusion that is gone.
unlisted_builtin="$(jq -r --argjson aggregates "$builtin_aggregates" --argjson listed "$non_chart_exclusions" '
  . as $policy | $aggregates | keys
  | map(select(. as $id | ($policy | index($id)) == null or ($listed | index($id)) == null)) | .[]
' "${scratch}/policy-exclusions.json")"
[ -z "$unlisted_builtin" ] ||
  fail "built-in aggregated roles are no longer non-chart policy exclusions, update this test: ${unlisted_builtin//$'\n'/, }"

[ -f "$builtin_baseline" ] ||
  fail "no reviewed built-in aggregate contributors baseline; run UPDATE_BASELINE=1 bash scripts/tests/test-chart-rendered-rbac-policy.sh and review it"
if ! diff -u "$builtin_baseline" "${scratch}/builtin.json" >"${scratch}/builtin.diff"; then
  cat "${scratch}/builtin.diff" >&2
  fail "contributors to the built-in admin and edit roles differ from those reviewed; review the diff above, then run UPDATE_BASELINE=1 bash scripts/tests/test-chart-rendered-rbac-policy.sh"
fi

# Negative control: labelling an unrelated committed ClusterRole into edit must make it
# a contributor to edit and admin and change the result, or the comparison above
# proves nothing.
unrelated="$(yq ea -o=json '[select(.kind == "ClusterRole")]' "${scratch}/committed.yaml" |
  jq -r '[.[]
    | select((.metadata.labels // {}) | keys | any(startswith("rbac.authorization.k8s.io/aggregate-to-")) | not)
    | .metadata.name] | first // empty')"
[ -n "$unrelated" ] ||
  fail "every committed ClusterRole already carries an aggregation label; the negative control has no unrelated role to label"
yq "(select(.kind == \"ClusterRole\" and .metadata.name == \"${unrelated}\")
    | .metadata.labels[\"rbac.authorization.k8s.io/aggregate-to-edit\"]) = \"true\"" \
  "${scratch}/committed.yaml" >"${scratch}/committed-labelled.yaml"
builtin_contributors "${scratch}/rbac.yaml" "${scratch}/committed-labelled.yaml" >"${scratch}/builtin-labelled.json"
jq -e --arg id "ClusterRole||${unrelated}" \
  '(.["ClusterRole||edit"] | has($id)) and (.["ClusterRole||admin"] | has($id))' \
  "${scratch}/builtin-labelled.json" >/dev/null ||
  fail "labelling ${unrelated} aggregate-to-edit did not make it a contributor to edit and admin"
if diff -q "$builtin_baseline" "${scratch}/builtin-labelled.json" >/dev/null; then
  fail "a ClusterRole labelled into edit still matched the reviewed contributors"
fi

printf 'PASS: %s chart-rendered roles pass the privileged-RBAC policy; %s excluded chart roles match their reviewed grants and contributors; %s contributor entries of the built-in admin and edit roles match their review; broadened, unexcluded and newly aggregated roles fail\n' \
  "$pass" "$(jq length "$baseline")" "$(jq '[.[] | length] | add // 0' "$builtin_baseline")"
