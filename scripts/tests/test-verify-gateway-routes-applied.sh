#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/verify-gateway-routes-applied.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

# The fake serves fixture files and refuses any call without the bounded request
# timeout, so an unbounded read fails the test rather than a production deploy.
fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[[ "$1 $2 $3" == "--context admin@prod --request-timeout=30s" ]] || {
  printf 'unexpected or unbounded kubectl call: %s\n' "$*" >&2
  exit 91
}
shift 3

if [[ "$#" -eq 5 && "$1" == get && "$3 $4 $5" == "-A -o json" ]]; then
  kind="${2%.gateway.networking.k8s.io}"
  # Every poll reads the GatewayClasses first, so they count the polls.
  poll=0
  if [[ -f "${FAKE_DIR}/polls" ]]; then
    poll="$(<"${FAKE_DIR}/polls")"
  fi
  if [[ "${kind}" == gatewayclasses ]]; then
    poll=$((poll + 1))
    printf '%s\n' "${poll}" >"${FAKE_DIR}/polls"
  fi
  if [[ "${kind}" == httproutes && ",${FAKE_FAIL_POLLS:-}," == *",${poll},"* ]]; then
    printf 'The connection to the server was refused\n' >&2
    exit 1
  fi
  if [[ -f "${FAKE_DIR}/${kind}.${poll}.json" ]]; then
    cat "${FAKE_DIR}/${kind}.${poll}.json"
  else
    cat "${FAKE_DIR}/${kind}.json"
  fi
  exit 0
fi

if [[ "$*" == "-n kube-system get pods -l io.cilium/app=operator --field-selector=status.phase=Running -o name" ]]; then
  [[ "${FAKE_FAIL_PODS:-false}" != true ]] || { printf 'pods are forbidden\n' >&2; exit 1; }
  cat "${FAKE_DIR}/pods.txt"
  exit 0
fi

if [[ "$#" -eq 6 && "$1 $2 $3" == "-n kube-system logs" && "$5 $6" == "-c cilium-operator" ]]; then
  [[ "${FAKE_FAIL_LOGS:-false}" != true ]] || { printf 'logs are unavailable\n' >&2; exit 1; }
  cat "${FAKE_DIR}/logs/${4#pod/}"
  exit 0
fi

printf 'unexpected kubectl arguments: %s\n' "$*" >&2
exit 92
EOF
chmod +x "${fake_kubectl}"

readonly cilium='io.cilium/gateway-controller'
readonly https='{"group":"gateway.networking.k8s.io","kind":"Gateway","namespace":"kube-system","name":"platform","sectionName":"https"}'
readonly apex='{"group":"gateway.networking.k8s.io","kind":"Gateway","namespace":"kube-system","name":"platform","sectionName":"https-apex"}'
readonly healthy_log='level=info msg="Checking for required and optional GatewayAPI resources" module=operator.operator-controlplane.leader-lifecycle.gateway-api
level=info msg="Starting EventSource" controller=gateway controllerGroup=gateway.networking.k8s.io'
readonly failed_log='level=info msg="Checking for required and optional GatewayAPI resources" module=operator.operator-controlplane.leader-lifecycle.gateway-api
level=error msg="Required GatewayAPI resources are not found, please refer to docs for installation instructions" module=operator.operator-controlplane.leader-lifecycle.gateway-api'

# A status condition; with no generation argument it carries none.
condition() {
  local type="$1" status="$2" reason="$3" generation="${4:-}"
  jq -cn --arg type "${type}" --arg status "${status}" --arg reason "${reason}" --arg generation "${generation}" \
    '{type: $type, status: $status, reason: $reason}
     + (if $generation == "" then {} else {observedGeneration: ($generation | tonumber)} end)'
}

# A status parent for <parentRef> from <controller> carrying the given conditions.
status_parent() {
  local ref="$1" controller="$2"
  shift 2
  jq -cn --argjson ref "${ref}" --arg controller "${controller}" \
    '{parentRef: $ref, controllerName: $controller, conditions: [$ARGS.positional[] | fromjson]}' \
    --args "$@"
}

# A status parent whose Accepted and ResolvedRefs conditions are True at the given generations.
applied_at() {
  local ref="$1" controller="$2" generation="$3" resolved="${4:-$3}"
  status_parent "${ref}" "${controller}" \
    "$(condition Accepted True Accepted "${generation}")" \
    "$(condition ResolvedRefs True ResolvedRefs "${resolved}")"
}

# route <namespace/name> <generation> <parentRefs JSON array> <status parents JSON array>
route() {
  jq -cn --arg id "$1" --argjson generation "$2" --argjson refs "$3" --argjson parents "$4" \
    '{metadata: {namespace: ($id | split("/")[0]), name: ($id | split("/")[1]), generation: $generation},
      spec: {parentRefs: $refs}, status: {parents: $parents}}'
}

# scenario <name> [class for kube-system/platform]: a fixture directory with
# GatewayClasses, Gateways, no routes, and two healthy running operator pods.
scenario() {
  local dir="${tmp_dir}/$1" platform_class="${2:-cilium}"
  mkdir -p "${dir}/logs"
  jq -n --arg cilium "${cilium}" '{items: [
    {metadata: {name: "cilium"}, spec: {controllerName: $cilium}},
    {metadata: {name: "other"}, spec: {controllerName: "example.com/other"}}]}' >"${dir}/gatewayclasses.json"
  jq -n --arg class "${platform_class}" '{items: [
    {metadata: {namespace: "kube-system", name: "platform"}, spec: {gatewayClassName: $class}},
    {metadata: {namespace: "other", name: "gw"}, spec: {gatewayClassName: "other"}}]}' >"${dir}/gateways.json"
  printf '{"items":[]}\n' >"${dir}/httproutes.json"
  printf 'pod/cilium-operator-a\npod/cilium-operator-b\n' >"${dir}/pods.txt"
  printf '%s\n' "${healthy_log}" >"${dir}/logs/cilium-operator-a"
  printf '%s\n' "${healthy_log}" >"${dir}/logs/cilium-operator-b"
  printf '%s\n' "${dir}"
}

# routes <file>: the route objects on stdin become that file's route list.
routes() {
  jq -s '{items: .}' >"$1"
}

run() {
  local dir="$1"
  shift
  set +e
  output="$(env \
    GATEWAY_ROUTES_KUBECTL_BIN="${fake_kubectl}" \
    GATEWAY_ROUTES_TIMEOUT_SECONDS=0 \
    GATEWAY_ROUTES_INTERVAL_SECONDS=0 \
    FAKE_DIR="${dir}" \
    "$@" bash "${script}" 2>&1)"
  status=$?
  set -e
}

fail() {
  printf 'FAIL %s: %s\n--- output ---\n%s\n' "${case_name}" "$1" "${output}" >&2
  exit 1
}

expect_status() {
  [[ "${status}" -eq "$1" ]] || fail "expected exit $1, got ${status}"
}

expect_line() {
  grep -Fxq -- "$1" <<<"${output}" || fail "missing line: $1"
}

expect_text() {
  grep -Fq -- "$1" <<<"${output}" || fail "missing text: $1"
}

expect_no_text() {
  if grep -Fq -- "$1" <<<"${output}"; then
    fail "unexpected text: $1"
  fi
}

# Every Cilium parent is applied at its current generation and the operators
# started cleanly. The route on another controller's Gateway and the route
# without parents are not counted.
case_name='all applied'
dir="$(scenario applied)"
{
  route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]"
  route web/multi 5 "[${https},${apex}]" \
    "[$(applied_at "${https}" "${cilium}" 5),$(applied_at "${apex}" "${cilium}" 5)]"
  # The API server may omit group and kind; the namespace defaults to the route's.
  route kube-system/defaulted 2 '[{"name":"platform","sectionName":"https"}]' \
    "[$(applied_at '{"name":"platform","sectionName":"https"}' "${cilium}" 2)]"
  route other/elsewhere 7 '[{"namespace":"other","name":"gw"}]' '[]'
  route web/mesh 1 '[]' '[]'
} | routes "${dir}/httproutes.json"
run "${dir}"
expect_status 0
expect_line '✅ The gateway applied all 4 HTTPRoute parent attachments at their current generation, and no Cilium operator failed to start its Gateway API controller.'

# The #4198 shape: the controller stopped applying, so status stays one generation behind.
case_name='frozen gateway'
dir="$(scenario frozen)"
{
  route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]"
  route auth/app 9 "[${https}]" "[$(applied_at "${https}" "${cilium}" 8)]"
} | routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_text '::error::The gateway is not serving these HTTPRoutes as declared after 0s:'
expect_line '  not-applied auth/app parent=kube-system/platform/https generation=9 applied=8'
expect_no_text 'web/site'
expect_text 'k8s/bases/infrastructure/controllers/cilium/helm-release.yaml'

# The lowest condition decides what the controller has applied.
case_name='one stale condition'
dir="$(scenario stale-condition)"
route web/site 4 "[${https}]" "[$(applied_at "${https}" "${cilium}" 4 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/site parent=kube-system/platform/https generation=4 applied=3'

# A new parent the controller never wrote status for is not applied.
case_name='missing status parent'
dir="$(scenario missing-parent)"
route web/multi 2 "[${https},${apex}]" "[$(applied_at "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/multi parent=kube-system/platform/https-apex generation=2 applied=none'
expect_no_text 'parent=kube-system/platform/https generation'

# Status for another section of the same Gateway does not cover this one.
case_name='different section'
dir="$(scenario other-section)"
route web/site 2 "[${apex}]" "[$(applied_at "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/site parent=kube-system/platform/https-apex generation=2 applied=none'

# A port is part of the parent reference too.
case_name='different port'
dir="$(scenario other-port)"
ported="$(jq -c '. + {port: 443}' <<<"${https}")"
route web/site 2 "[${ported}]" "[$(applied_at "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/site parent=kube-system/platform/https generation=2 applied=none'

# Only Cilium's status counts for a Cilium Gateway.
case_name='other controller status'
dir="$(scenario other-controller)"
route web/site 2 "[${https}]" "[$(applied_at "${https}" example.com/other 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/site parent=kube-system/platform/https generation=2 applied=none'

# A route the gateway rejected at its current generation is not being served.
case_name='rejected route'
dir="$(scenario rejected)"
route web/site 5 "[${https}]" "[$(status_parent "${https}" "${cilium}" \
  "$(condition Accepted True Accepted 5)" \
  "$(condition ResolvedRefs False RefNotPermitted 5)")]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  rejected web/site parent=kube-system/platform/https conditions=ResolvedRefs/RefNotPermitted'
expect_text 'refused by the gateway'
expect_no_text 'restart marker'

# A current rejection counts even beside a stale condition on the same parent.
case_name='rejection beside a stale condition'
dir="$(scenario rejected-mixed)"
route web/site 6 "[${https}]" "[$(status_parent "${https}" "${cilium}" \
  "$(condition Accepted False NotAllowedByListeners 6)" \
  "$(condition ResolvedRefs True ResolvedRefs 5)")]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  rejected web/site parent=kube-system/platform/https conditions=Accepted/NotAllowedByListeners'

# A rejection from an older generation is superseded once the new spec is applied.
case_name='stale rejection'
dir="$(scenario stale-rejection)"
route web/site 6 "[${https}]" "[$(status_parent "${https}" "${cilium}" \
  "$(condition Accepted True Accepted 6)" \
  "$(condition ResolvedRefs True ResolvedRefs 6)" \
  "$(condition ResolvedRefs False BackendNotFound 5)")]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  not-applied web/site parent=kube-system/platform/https generation=6 applied=5'
expect_no_text 'rejected web/site'

# A parent naming a Gateway that does not exist drops the route unseen.
case_name='missing gateway'
dir="$(scenario missing-gateway)"
{
  route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]"
  route web/typo 1 '[{"namespace":"kube-system","name":"platfrom"}]' '[]'
} | routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  missing-gateway web/typo parent=kube-system/platfrom'
expect_text 'fix its parentRefs'

# A controller that never started leaves applied routes looking current, so the
# operator's own record of the failed start fails the check at once.
case_name='operator failed to start'
dir="$(scenario failed-start)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
printf '%s\n' "${failed_log}" >"${dir}/logs/cilium-operator-b"
run "${dir}" GATEWAY_ROUTES_TIMEOUT_SECONDS=30
expect_status 1
expect_text 'logged "Required GatewayAPI resources are not found"'
expect_line '  pod/cilium-operator-b'
expect_no_text 'pod/cilium-operator-a'
expect_text 'restart marker'
[[ "$(<"${dir}/polls")" == 1 ]] || fail "a failed start never recovers, so it must not wait; polled $(<"${dir}/polls") times"

case_name='unreadable operator logs'
dir="$(scenario unreadable-logs)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}" FAKE_FAIL_LOGS=true
expect_status 1
expect_text '::error::Could not evaluate the gateway'
expect_text 'logs are unavailable'
expect_no_text '✅'

case_name='unreadable operator pods'
dir="$(scenario unreadable-pods)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}" FAKE_FAIL_PODS=true
expect_status 1
expect_text 'pods are forbidden'
expect_no_text '✅'

case_name='no running operator'
dir="$(scenario no-operator)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
: >"${dir}/pods.txt"
run "${dir}"
expect_status 1
expect_text 'no running Cilium operator pod'
expect_no_text '✅'

# A route that catches up before the deadline passes: the first poll lags and
# the second has applied it.
case_name='catching up'
dir="$(scenario catching-up)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.1.json"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}" GATEWAY_ROUTES_TIMEOUT_SECONDS=30
expect_status 0
expect_text 'applied all 1 HTTPRoute parent attachments'
[[ "$(<"${dir}/polls")" == 2 ]] || fail "expected two polls, got $(<"${dir}/polls")"

# A read that fails once is retried.
case_name='transient read failure'
dir="$(scenario transient)"
route web/site 3 "[${https}]" "[$(applied_at "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}" GATEWAY_ROUTES_TIMEOUT_SECONDS=30 FAKE_FAIL_POLLS=1
expect_status 0
[[ "$(<"${dir}/polls")" == 2 ]] || fail "expected two polls, got $(<"${dir}/polls")"

# A read that keeps failing is never read as healthy.
case_name='persistent read failure'
dir="$(scenario unreadable)"
run "${dir}" FAKE_FAIL_POLLS=1
expect_status 1
expect_text '::error::Could not evaluate the gateway'
expect_text 'The connection to the server was refused'
expect_no_text '✅'

# Output that is not a route list evaluates nothing, so it cannot pass.
case_name='malformed read'
dir="$(scenario malformed)"
printf 'not json\n' >"${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_text '::error::Could not evaluate the gateway'
expect_no_text '✅'

case_name='empty read'
dir="$(scenario empty)"
: >"${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_text '::error::Could not evaluate the gateway'
expect_no_text '✅'

# No Cilium Gateway means this is not the cluster the check was written for.
case_name='no Cilium gateway'
dir="$(scenario no-cilium other)"
run "${dir}"
expect_status 1
expect_text 'no Gateway uses a GatewayClass controlled by io.cilium/gateway-controller'
expect_no_text '✅'

case_name='invalid timeout'
dir="$(scenario invalid)"
run "${dir}" GATEWAY_ROUTES_TIMEOUT_SECONDS=soon
expect_status 1
expect_text 'must be a whole-number timeout'

printf 'ok — the deploy fails when the gateway is not serving its routes as declared\n'
