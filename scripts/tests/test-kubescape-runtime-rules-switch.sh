#!/usr/bin/env bash
# Prove the production switch for Kubescape's default runtime-detection rules (#4968).
#
# The kubescape-operator chart renders the node-agent's `default-rules` Rules object and its
# cluster-wide `all-rules-all-pods` RuntimeRuleAlertBinding only when `alertCRD.installDefault`
# is true, and defaults that value to false. Production never set it, so runtime detection ran
# with zero rules: the node agent profiled workloads but checked nothing, and an empty alert
# route read exactly like a quiet cluster. This test makes that state visible and the switch safe:
#
#   1. Production must explicitly install the initial port-forward baseline when runtime detection
#      is enabled. An enabled detector with no rules is a failure, never a quiet cluster.
#   2. The pinned chart is rendered with production's values and post-renderers in BOTH states,
#      as an install and as an upgrade, whichever state is declared.
#   3. Off must render no rule or binding object. On must render exactly the default Rules object
#      and the cluster-wide binding, each containing only the enabled, profile-independent
#      port-forward rule by ID, and cover the same namespaces the scanner does.
#   4. On may differ from off by those two objects only, so turning the switch on changes no
#      RBAC, workload or CRD.
#
# Each check also runs against a mutated render, to prove it fails for its own reason.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash "$root/scripts/generate-kubescape-rule-schema.sh" --check
scratch="$(mktemp -d "${TMPDIR:-/tmp}/kubescape-runtime-rules.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# The declared switch: "true" or "false", or the reason it is not a boolean.
declared_switch() {
  jq -r '.spec.values.alertCRD.installDefault |
    if type == "boolean" then tostring else "undeclared (\(type))" end' "$1"
}

production_baseline_enabled() {
  jq -e '.spec.values.capabilities.runtimeDetection == "enable" and
    .spec.values.alertCRD.installDefault == true' "$1" >/dev/null
}

# Problems with an "off" render ($1: a JSON array of every rendered object).
off_problems() {
  jq -r '.[] | select(.kind == "Rules" or .kind == "RuntimeRuleAlertBinding") |
    "off renders \(.kind) \(.metadata.name)"' "$1"
}

# Problems with an "on" render ($1: rendered objects, $2: the release's excludeNamespaces).
on_problems() {
  jq -r --arg exclude "$2" '
    [.[] | select(.kind == "Rules")] as $rules |
    [.[] | select(.kind == "RuntimeRuleAlertBinding")] as $bindings |
    if ($rules | length) != 1 then "on renders \($rules | length) Rules objects, not 1"
    elif ($bindings | length) != 1 then "on renders \($bindings | length) bindings, not 1"
    else
      $rules[0] as $set | $bindings[0] as $binding |
      [$set.spec.rules[]? | select(.enabled == true) | .id] as $enabled |
      [$binding.spec.rules[]? | .ruleID] as $bound |
      (if $set.metadata.name == "default-rules" and $set.metadata.namespace == "kubescape" then empty
       else "the Rules object is not kubescape/default-rules" end),
      (if ($set.spec | keys) == ["rules"] and ($set.spec.rules | length) == 1
       then empty else "the baseline must contain exactly one rule definition and no temporary state" end),
      (if $binding.metadata.name == "all-rules-all-pods" and $binding.metadata.namespace == null then empty
       else "the binding is not the cluster-wide all-rules-all-pods" end),
      (if any($bound[]; IN($enabled[])) then empty else "the binding names no enabled rule" end),
      (if $binding.spec.rules == [{ruleID:"R2001"}] and
          $binding.metadata.labels["devantler.tech/runtime-rule-stage"] == "port-forward-baseline"
       then empty else "the binding is not the bounded port-forward baseline" end),
      (if [$set.spec.rules[]? | select(.id == "R2001" and .enabled == true and
            .profileDependency == 2 and .expressions.ruleExpression == [{
              eventType:"k8s-admission", expression:"event.Kind == \"PodPortForwardOptions\""}])] | length == 1
       then empty else "the port-forward definition is missing, disabled or profile-dependent" end),
      (if $binding.spec.namespaceSelector.matchExpressions == [{
            key: "kubernetes.io/metadata.name", operator: "NotIn", values: ($exclude | split(","))}]
       then empty else "the binding does not cover exactly the scanned namespaces" end)
    end' "$1"
}

# A stable ID must resolve against the installed Rules object; names may change.
unbound_rule_ids() {
  jq -r '
    [.[] | select(.kind == "Rules") | .spec.rules[]? | .id] as $ids |
    .[] | select(.kind == "RuntimeRuleAlertBinding") | .spec.rules[]? | .ruleID |
    select(IN($ids[]) | not)' "$1"
}

# Every rendered object except the two the switch owns, in a stable order. The chart generates
# fresh TLS material on every render, so base64-encoded PEM values ("-----BEGIN" encodes to
# "LS0tLS1CRUdJTi") are masked; everything else must match byte for byte.
without_rule_objects() {
  jq -S 'map(select(.kind != "Rules" and .kind != "RuntimeRuleAlertBinding")) |
    walk(if type == "string" and startswith("LS0tLS1CRUdJTi") then "<generated PEM>" else . end) |
    sort_by(.kind, (.metadata.namespace // ""), .metadata.name)' "$1"
}

# Require a check to report a problem containing the expected reason.
expect_problem() {
  local reason="$1" output
  shift
  output="$("$@")" || fail "a negative control errored: $*"
  [[ "$output" == *"$reason"* ]] || fail "a negative control did not report '$reason': $output"
}

release="$root/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml"
version="$(yq -er '.spec.chart.spec.version' "$release")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'the chart must have an exact version'
export HELM_CACHE_HOME="$scratch/cache" HELM_CONFIG_HOME="$scratch/config"
export HELM_DATA_HOME="$scratch/data" HELM_PLUGINS="$scratch/plugins"
helm pull kubescape-operator --repo https://kubescape.github.io/helm-charts/ \
  --version "$version" --destination "$scratch" >/dev/null

kubectl kustomize "$root/k8s/providers/hetzner/infrastructure/controllers" |
  yq ea -o=json -I=0 'select(.kind == "HelmRelease" and .metadata.name == "kubescape")' - |
  jq -e -s 'if length == 1 then .[0] else error("expected one effective release") end' >"$scratch/release.json"

declared="$(declared_switch "$scratch/release.json")"
[[ "$declared" == true || "$declared" == false ]] ||
  fail "production must declare alertCRD.installDefault as true or false, not $declared"
jq 'del(.spec.values.alertCRD.installDefault)' "$scratch/release.json" >"$scratch/undeclared.json"
[[ "$(declared_switch "$scratch/undeclared.json")" == "undeclared (null)" ]] ||
  fail 'the declaration check accepted a missing switch'
jq '.spec.values.alertCRD.installDefault = "false"' "$scratch/release.json" >"$scratch/undeclared.json"
[[ "$(declared_switch "$scratch/undeclared.json")" == "undeclared (string)" ]] ||
  fail 'the declaration check accepted a string switch'

production_baseline_enabled "$scratch/release.json" ||
  fail 'production runtime detection and its installed rule baseline must be enabled'
for mutation in '.spec.values.capabilities.runtimeDetection = "disable"' \
                '.spec.values.alertCRD.installDefault = false'; do
  jq "$mutation" "$scratch/release.json" >"$scratch/disabled-baseline.json"
  if production_baseline_enabled "$scratch/disabled-baseline.json"; then
    fail 'the production baseline check accepted a disabled detector or rule set'
  fi
done
exclude="$(jq -er '.spec.values.excludeNamespaces | select(type == "string" and . != "")' "$scratch/release.json")" ||
  fail 'the release must declare the namespaces the scanner excludes'
jq -e '.spec.postRenderers | select(length > 0)' "$scratch/release.json" >"$scratch/renderers.json"
count="$(jq length "$scratch/renderers.json")"
kube_version="$(yq -er '.spec.cluster.kubernetesVersion' "$root/ksail.prod.yaml")"
controller_helm="${CONTROLLER_HELM:-$scratch/controller-helm}"
if [[ -z "${CONTROLLER_HELM:-}" ]]; then
  bash "$root/scripts/build-controller-helm.sh" "$controller_helm"
fi
[[ "$("$controller_helm" version --template '{{ printf "%#v" . }}')" == 'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.8", KubeClientVersion:"v1.36"}' ]] ||
  fail 'the audited controller renderer is required'

# Render the chart with one switch state and apply the release's post-renderers as Flux does.
# $1: true|false, $2: install|upgrade. Writes every rendered object to $scratch/$1-$2.json.
render() {
  local state="$1" mode="$2" i renderer
  jq --argjson on "$state" '.spec.values | .clusterName = "runtime-rules-fixture" |
    .alertCRD.installDefault = $on' "$scratch/release.json" >"$scratch/values.json"
  if [[ "$mode" == upgrade ]]; then
    "$controller_helm" template kubescape "$scratch/kubescape-operator-$version.tgz" \
      --namespace kubescape --kube-version "$kube_version" --values "$scratch/values.json" \
      --is-upgrade >"$scratch/rendered.yaml"
  else
    "$controller_helm" template kubescape "$scratch/kubescape-operator-$version.tgz" \
      --namespace kubescape --kube-version "$kube_version" --values "$scratch/values.json" \
      >"$scratch/rendered.yaml"
  fi
  for ((i = 0; i < count; i++)); do
    renderer="$scratch/$state-$mode-renderer-$i"
    mkdir "$renderer"
    cp "$scratch/rendered.yaml" "$renderer/rendered.yaml"
    jq --argjson i "$i" '
      {apiVersion:"kustomize.config.k8s.io/v1beta1",kind:"Kustomization",resources:["rendered.yaml"]} +
      .[$i].kustomize
    ' "$scratch/renderers.json" >"$renderer/kustomization.yaml"
    kubectl kustomize "$renderer" >"$scratch/next.yaml"
    mv "$scratch/next.yaml" "$scratch/rendered.yaml"
  done
  yq ea -o=json -I=0 'select(. != null)' "$scratch/rendered.yaml" |
    jq -e -s 'select(length > 0)' >"$scratch/$state-$mode.json" ||
    fail "the $state $mode render is empty"
}

for mode in install upgrade; do
  render false "$mode"
  render true "$mode"
  off="$scratch/false-$mode.json"
  on="$scratch/true-$mode.json"

  problems="$(off_problems "$off")"
  [[ -z "$problems" ]] || fail "$mode: $problems"
  problems="$(on_problems "$on" "$exclude")"
  [[ -z "$problems" ]] || fail "$mode: $problems"
  without_rule_objects "$off" >"$scratch/off-rest.json"
  without_rule_objects "$on" >"$scratch/on-rest.json"
  cmp -s "$scratch/off-rest.json" "$scratch/on-rest.json" ||
    fail "$mode: turning the switch on changes more than the rule and binding objects"

  # Negative controls: each check must fail on the input it exists to refuse.
  expect_problem 'off renders Rules default-rules' off_problems "$on"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(.enabled = false) else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'names no enabled rule' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(.id += "-unknown") else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'names no enabled rule' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(if .id == "R2001" then .id = "unknown" else . end) else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'R2001' unbound_rule_ids "$scratch/mutated.json"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(.name += " (renamed)") else . end)' "$on" >"$scratch/mutated.json"
  [[ -z "$(on_problems "$scratch/mutated.json" "$exclude")" ]] ||
    fail "$mode: renaming a rule broke its stable-ID binding"
  jq 'map(if .kind == "RuntimeRuleAlertBinding" then .spec.rules = [{ruleID:"R0001"}] else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'not the bounded port-forward baseline' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "RuntimeRuleAlertBinding" then .spec.rules += [{ruleID:"R0001"}] else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'not the bounded port-forward baseline' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.rules += [.spec.rules[0] | .id = "R0001"] else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'exactly one rule definition' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.portForwardBaseline = {} else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'no temporary state' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(if .id == "R2001" then .profileDependency = 0 else . end) else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'profile-dependent' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "Rules" then .spec.rules |= map(if .id == "R2001" then .expressions.ruleExpression[0].eventType = "exec" else . end) else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'port-forward definition' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(select(.kind != "RuntimeRuleAlertBinding"))' "$on" >"$scratch/mutated.json"
  expect_problem 'renders 0 bindings' on_problems "$scratch/mutated.json" "$exclude"
  jq 'map(if .kind == "RuntimeRuleAlertBinding" then del(.spec.namespaceSelector) else . end)' "$on" >"$scratch/mutated.json"
  expect_problem 'cover exactly the scanned namespaces' on_problems "$scratch/mutated.json" "$exclude"
  jq '. + [{apiVersion:"rbac.authorization.k8s.io/v1",kind:"ClusterRole",metadata:{name:"widened"}}]' "$on" >"$scratch/mutated.json"
  without_rule_objects "$scratch/mutated.json" >"$scratch/mutated-rest.json"
  if cmp -s "$scratch/off-rest.json" "$scratch/mutated-rest.json"; then
    fail "$mode: the comparison accepted an extra object"
  fi
done

[[ -z "$(unbound_rule_ids "$scratch/true-install.json")" ]] || fail 'the binding contains unknown rule IDs'
printf 'PASS: runtime detection installs the bounded port-forward baseline; other resources and both switch states are preserved\n'
