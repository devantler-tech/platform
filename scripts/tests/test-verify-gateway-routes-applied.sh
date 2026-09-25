#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/verify-gateway-routes-applied.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 7 && "$1 $2 $3" == "--context admin@prod get" && "$5 $6 $7" == "-A -o json" ]] || {
  printf 'unexpected kubectl arguments: %s\n' "$*" >&2
  exit 91
}
kind="${4%.gateway.networking.k8s.io}"

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
EOF
chmod +x "${fake_kubectl}"

readonly cilium='io.cilium/gateway-controller'
readonly https='{"group":"gateway.networking.k8s.io","kind":"Gateway","namespace":"kube-system","name":"platform","sectionName":"https"}'
readonly apex='{"group":"gateway.networking.k8s.io","kind":"Gateway","namespace":"kube-system","name":"platform","sectionName":"https-apex"}'

# A status parent for <parentRef> whose conditions carry the given observedGenerations.
status_parent() {
  local ref="$1" controller="$2"
  shift 2
  jq -cn --argjson ref "${ref}" --arg controller "${controller}" \
    '{parentRef: $ref, controllerName: $controller,
      conditions: [$ARGS.positional[] | {type: "Accepted", status: "True", observedGeneration: tonumber}]}' \
    --args "$@"
}

# route <namespace/name> <generation> <parentRefs JSON array> <status parents JSON array>
route() {
  jq -cn --arg id "$1" --argjson generation "$2" --argjson refs "$3" --argjson parents "$4" \
    '{metadata: {namespace: ($id | split("/")[0]), name: ($id | split("/")[1]), generation: $generation},
      spec: {parentRefs: $refs}, status: {parents: $parents}}'
}

# scenario <name> [class controller for kube-system/platform]: a fixture
# directory with GatewayClasses, Gateways and an empty route list.
scenario() {
  local dir="${tmp_dir}/$1" platform_class="${2:-cilium}"
  mkdir -p "${dir}"
  jq -n --arg cilium "${cilium}" '{items: [
    {metadata: {name: "cilium"}, spec: {controllerName: $cilium}},
    {metadata: {name: "other"}, spec: {controllerName: "example.com/other"}}]}' >"${dir}/gatewayclasses.json"
  jq -n --arg class "${platform_class}" '{items: [
    {metadata: {namespace: "kube-system", name: "platform"}, spec: {gatewayClassName: $class}},
    {metadata: {namespace: "other", name: "gw"}, spec: {gatewayClassName: "other"}}]}' >"${dir}/gateways.json"
  printf '{"items":[]}\n' >"${dir}/httproutes.json"
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

# Every Cilium parent is applied at its current generation. The route aimed at
# another controller's Gateway and the route without parents are not counted.
case_name='all applied'
dir="$(scenario applied)"
{
  route web/site 3 "[${https}]" "[$(status_parent "${https}" "${cilium}" 3 3)]"
  route web/multi 5 "[${https},${apex}]" \
    "[$(status_parent "${https}" "${cilium}" 5 5),$(status_parent "${apex}" "${cilium}" 5)]"
  # The API server may omit group and kind; the namespace defaults to the route's.
  route kube-system/defaulted 2 '[{"name":"platform","sectionName":"https"}]' \
    "[$(status_parent '{"name":"platform","sectionName":"https"}' "${cilium}" 2)]"
  route other/elsewhere 7 '[{"namespace":"other","name":"gw"}]' '[]'
  route web/mesh 1 '[]' '[]'
} | routes "${dir}/httproutes.json"
run "${dir}"
expect_status 0
expect_line '✅ The gateway applied all 4 HTTPRoute parent attachments at their current generation.'

# The #4198 shape: the controller stopped applying, so status stays one generation behind.
case_name='frozen gateway'
dir="$(scenario frozen)"
{
  route web/site 3 "[${https}]" "[$(status_parent "${https}" "${cilium}" 3 3)]"
  route auth/app 9 "[${https}]" "[$(status_parent "${https}" "${cilium}" 8 8)]"
} | routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_text '::error::The gateway has not applied these HTTPRoutes at their current generation after 0s'
expect_line '  auth/app parent=kube-system/platform/https generation=9 applied=8'
expect_no_text 'web/site'
expect_text 'Required GatewayAPI resources are not found'
expect_text 'k8s/bases/infrastructure/controllers/cilium/helm-release.yaml'

# The lowest condition decides what the controller has applied.
case_name='one stale condition'
dir="$(scenario stale-condition)"
route web/site 4 "[${https}]" "[$(status_parent "${https}" "${cilium}" 4 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  web/site parent=kube-system/platform/https generation=4 applied=3'

# A new parent the controller never wrote status for is not applied.
case_name='missing status parent'
dir="$(scenario missing-parent)"
route web/multi 2 "[${https},${apex}]" "[$(status_parent "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  web/multi parent=kube-system/platform/https-apex generation=2 applied=none'
expect_no_text 'parent=kube-system/platform/https generation'

# Status for another section of the same Gateway does not cover this one.
case_name='different section'
dir="$(scenario other-section)"
route web/site 2 "[${apex}]" "[$(status_parent "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  web/site parent=kube-system/platform/https-apex generation=2 applied=none'

# A port is part of the parent reference too.
case_name='different port'
dir="$(scenario other-port)"
ported="$(jq -c '. + {port: 443}' <<<"${https}")"
route web/site 2 "[${ported}]" "[$(status_parent "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  web/site parent=kube-system/platform/https generation=2 applied=none'

# Only Cilium's status counts for a Cilium Gateway.
case_name='other controller status'
dir="$(scenario other-controller)"
route web/site 2 "[${https}]" "[$(status_parent "${https}" example.com/other 2)]" |
  routes "${dir}/httproutes.json"
run "${dir}"
expect_status 1
expect_line '  web/site parent=kube-system/platform/https generation=2 applied=none'

# A route that catches up before the deadline passes: the first poll lags and
# the second has applied it.
case_name='catching up'
dir="$(scenario catching-up)"
route web/site 3 "[${https}]" "[$(status_parent "${https}" "${cilium}" 2)]" |
  routes "${dir}/httproutes.1.json"
route web/site 3 "[${https}]" "[$(status_parent "${https}" "${cilium}" 3)]" |
  routes "${dir}/httproutes.json"
run "${dir}" GATEWAY_ROUTES_TIMEOUT_SECONDS=30
expect_status 0
expect_line '✅ The gateway applied all 1 HTTPRoute parent attachments at their current generation.'
[[ "$(<"${dir}/polls")" == 2 ]] || fail "expected two polls, got $(<"${dir}/polls")"

# A read that fails once is retried.
case_name='transient read failure'
dir="$(scenario transient)"
route web/site 3 "[${https}]" "[$(status_parent "${https}" "${cilium}" 3)]" |
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

printf 'ok — the deploy fails when the gateway stops applying HTTPRoutes\n'
