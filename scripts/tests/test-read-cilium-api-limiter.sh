#!/usr/bin/env bash
# Offline behavioural controls for the reviewed production diagnostic (#4329).
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
mkdir -p "${work_dir}/bin"
cat >"${work_dir}/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${FIXTURE}/calls"
[[ "$1 $2 $3 $4" == '--context admin@prod --request-timeout 15s' ]] || exit 92
shift 4
case "$*" in
  'get daemonset cilium --namespace kube-system --output json') cat "${FIXTURE}/ds.json" ;;
  'get pods --namespace kube-system --selector k8s-app=cilium --output json')
    if [[ -f "${FIXTURE}/read-once" && -f "${FIXTURE}/after.json" ]]; then
      cat "${FIXTURE}/after.json"
    else cat "${FIXTURE}/pods.json"; fi
    touch "${FIXTURE}/read-once"
    ;;
  'exec --namespace kube-system cilium-abcde --container cilium-agent -- cilium-dbg metrics list --output json --match-pattern ^cilium_api_limiter_')
    if [[ -f "${FIXTURE}/stderr" ]]; then
      cat "${FIXTURE}/stderr" >&2
      exit "$(cat "${FIXTURE}/exit-code")"
    fi
    if [[ -f "${FIXTURE}/denied" ]]; then
      printf 'PRIVATE_ERROR https://198.51.100.9:6443\n' >&2
      exit 1
    fi
    cat "${FIXTURE}/metrics.json"
    ;;
  'exec --namespace kube-system cilium-abcde --container cilium-agent -- cilium-dbg metrics list --output json --match-pattern ^cilium_version')
    if [[ -f "${FIXTURE}/control-denied" ]]; then
      printf 'PRIVATE_ERROR https://198.51.100.9:6443\n' >&2
      exit 1
    fi
    cat "${FIXTURE}/control.json"
    ;;
  'exec --namespace kube-system cilium-fghij --container cilium-agent -- cilium-dbg metrics list --output json --match-pattern ^cilium_api_limiter_')
    # second-late.json, when present, replaces the response from the third sample on.
    printf 'read\n' >>"${FIXTURE}/second-reads"
    if [[ -f "${FIXTURE}/second-late.json" && "$(wc -l <"${FIXTURE}/second-reads")" -ge 3 ]]; then
      cat "${FIXTURE}/second-late.json"
    else cat "${FIXTURE}/second-metrics.json"; fi
    ;;
  'exec --namespace kube-system cilium-fghij --container cilium-agent -- cilium-dbg metrics list --output json --match-pattern ^cilium_version')
    cat "${FIXTURE}/control.json"
    ;;
  *) touch "${FIXTURE}/unexpected"; exit 93 ;;
esac
FAKE
cat >"${work_dir}/bin/sleep" <<'FAKE'
#!/usr/bin/env bash
[[ "$*" == 15 ]] || exit 94
FAKE
chmod +x "${work_dir}/bin/kubectl" "${work_dir}/bin/sleep"

reset_fixture() {
  rm -rf "${work_dir}/fixture"
  mkdir "${work_dir}/fixture"
  fixture="${work_dir}/fixture"
  jq -n '{metadata:{uid:"11111111-1111-1111-1111-111111111111",generation:1},
    spec:{template:{spec:{containers:[{name:"cilium-agent",image:"quay.io/cilium/cilium:v1.20.4"}]}}},
    status:{observedGeneration:1,desiredNumberScheduled:1,updatedNumberScheduled:1,numberAvailable:1,numberMisscheduled:0}}' >"${fixture}/ds.json"
  jq -n '{items:[{metadata:{name:"cilium-abcde",uid:"22222222-2222-2222-2222-222222222222",
    namespace:"kube-system",ownerReferences:[{kind:"DaemonSet",name:"cilium",uid:"11111111-1111-1111-1111-111111111111",controller:true}]},
    spec:{nodeName:"PRIVATE_NODE",containers:[{name:"cilium-agent",image:"quay.io/cilium/cilium:v1.20.4"}]},
    status:{phase:"Running",containerStatuses:[{name:"cilium-agent",ready:true,restartCount:0,
      containerID:"containerd://PRIVATE_CONTAINER",imageID:"quay.io/cilium/cilium@sha256:PRIVATE_DIGEST",state:{running:{startedAt:"2026-10-26T20:00:00Z"}}}]}}]}' >"${fixture}/pods.json"
  jq -n '[
    {n:"processing_duration_seconds",v:["mean","estimated"]},
    {n:"wait_duration_seconds",v:["mean","max","min"]},
    {n:"requests_in_flight",v:["in-flight","limit"]},
    {n:"rate_limit",v:["limit","burst"]},
    {n:"adjustment_factor",v:[""]}
    ] | [ .[] | .n as $n | .v[] | {name:("cilium_api_limiter_"+$n),labels:({api_call:"endpoint-create"} + (if . == "" then {} else {value:.} end)),value:0.125} ]
    + [{name:"cilium_api_limiter_processed_requests_total",labels:{api_call:"endpoint-create",outcome:"success",return_code:"200"},value:300},
       {name:"cilium_api_limiter_processed_requests_total",labels:{api_call:"endpoint-create",outcome:"fail",return_code:"500"},value:2},
       {name:"PRIVATE_METRIC",labels:{private:"PRIVATE_LABEL"},value:999}]
    | map(if .labels.value=="min" or .labels.value=="in-flight" then del(.value) else . end)' >"${fixture}/metrics.json"
  jq -n '[{name:"cilium_version",labels:{version:"PRIVATE_VERSION"}}]' >"${fixture}/control.json"
}
# Adds a second agent, whose limiter read comes from second-metrics.json.
add_second_agent() {
  jq '.status.desiredNumberScheduled=2 | .status.updatedNumberScheduled=2 | .status.numberAvailable=2' \
    "${fixture}/ds.json" >"${fixture}/new.json"
  mv "${fixture}/new.json" "${fixture}/ds.json"
  jq '.items += [.items[0] | .metadata.name="cilium-fghij" | .metadata.uid="44444444-4444-4444-4444-444444444444" |
    .spec.nodeName="PRIVATE_NODE_TWO"]' "${fixture}/pods.json" >"${fixture}/new.json"
  mv "${fixture}/new.json" "${fixture}/pods.json"
  cp "${fixture}/metrics.json" "${fixture}/second-metrics.json"
}
run_read() {
  rc=0
  output="$(FIXTURE="${fixture}" PATH="${work_dir}/bin:${PATH}" bash "${root_dir}/scripts/read-cilium-api-limiter.sh" "$@" 2>&1)" || rc=$?
  if [[ -f "${fixture}/unexpected" ]]; then printf 'FAIL: unexpected command\n' >&2; exit 1; fi
  if [[ "${output}" == *PRIVATE* || "${output}" == *198.51.100.9* || "${output}" == *cilium-abcde* || "${output}" == *cilium-fghij* ]]; then
    printf 'FAIL: private fixture data escaped\n' >&2; exit 1
  fi
}
require_inconclusive() {
  run_read --context admin@prod
  [[ "${rc}" -eq 3 && "${output}" == INCONCLUSIVE:* && "${output}" != *MEASURED* ]] || {
    printf 'FAIL: expected INCONCLUSIVE: %s (%s)\n' "${output}" "${rc}" >&2; exit 1;
  }
}

reset_fixture
run_read --context admin@prod
[[ "${rc}" -eq 0 && "${output}" == *MEASURED* && "${output}" == *processed_requests_total*302* ]] || {
  printf 'FAIL: passing control: %s (%s)\n' "${output}" "${rc}" >&2; exit 1;
}
[[ "$(grep -c '^--context.* exec ' "${fixture}/calls")" -eq 5 ]] || exit 1
# An omitted value is upstream's zero, and is published as one.
[[ "$(printf '%s\n' "${output}" | grep '^{' | jq -s '[.[].metrics[] | select(.metric=="cilium_api_limiter_wait_duration_seconds" and .stat=="min") | .value] | unique | tojson' -r)" == '[0]' ]] || {
  printf 'FAIL: omitted zero was not published as zero: %s\n' "${output}" >&2; exit 1;
}

# The reader accepts only the audited agent version, including the previous one.
reset_fixture
sed -i.bak 's|cilium:v1\.20\.4|cilium:v1.20.3|' "${fixture}/ds.json" "${fixture}/pods.json"
require_inconclusive
[[ ! -f "${fixture}/calls" ]] || ! grep -q ' exec ' "${fixture}/calls" || {
  printf 'FAIL: an unaudited agent version reached a metric read\n' >&2; exit 1;
}

# Usage must reach neither Kubernetes nor sleep.
for args in '' '--context' '--context reader@prod' '--context admin@prod --window 1'; do
  reset_fixture
  # shellcheck disable=SC2086
  run_read ${args}
  [[ "${rc}" -eq 1 && ! -f "${fixture}/calls" ]] || exit 1
done

for mutation in '.status.numberAvailable=0' '.status.observedGeneration=0' '.status.desiredNumberScheduled=0'; do
  reset_fixture
  jq "${mutation}" "${fixture}/ds.json" >"${fixture}/new.json"
  mv "${fixture}/new.json" "${fixture}/ds.json"
  require_inconclusive
done
for mutation in '.items=[]' '.items[0].status.containerStatuses[0].ready=false' \
  '.items[0].metadata.ownerReferences[0].uid="foreign"' '.items[0].metadata.deletionTimestamp="2026-10-27T00:00:00Z"' \
  '.items[0].spec.containers[0].image="quay.io/cilium/cilium:v1.19.0"'; do
  reset_fixture
  jq "${mutation}" "${fixture}/pods.json" >"${fixture}/new.json"
  mv "${fixture}/new.json" "${fixture}/pods.json"
  require_inconclusive
done
for mutation in '.items[0].metadata.uid="33333333-3333-3333-3333-333333333333"' \
  '.items[0].status.containerStatuses[0].restartCount=1' '.items[0].spec.nodeName="PRIVATE_REPLACEMENT"'; do
  reset_fixture
  jq "${mutation}" "${fixture}/pods.json" >"${fixture}/after.json"
  require_inconclusive
done
for mutation in '.[0].value=-1' '.[0].value=null' '.[0].value="PRIVATE_VALUE"' 'map(if (.name|endswith("processed_requests_total")) then del(.value) else . end)' '.[0].labels.value="PRIVATE_VALUE"' \
  '.[0].labels.private="PRIVATE_LABEL"' '.[0].name="cilium_api_limiter_wait_duration_seconds"' \
  '.[0].labels.api_call="endpoint-delete"' '.[0].value=1e300' '.[0]=.[1]' '.[0:9]' '[]'; do
  reset_fixture
  jq "${mutation}" "${fixture}/metrics.json" >"${fixture}/new.json"
  mv "${fixture}/new.json" "${fixture}/metrics.json"
  require_inconclusive
done
reset_fixture
printf '{malformed' >"${fixture}/metrics.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent metric read failed (INVALID_JSON).' ]] || {
  printf 'FAIL: malformed JSON was not classified: %s\n' "${output}" >&2; exit 1;
}
# Successful CLI exits can still carry no measurement. Keep those distinct from
# malformed JSON, but refuse all of them before publishing any samples.
while IFS='|' read -r expected response; do
  reset_fixture
  printf '%b' "${response}" >"${fixture}/metrics.json"
  require_inconclusive
  [[ "${output}" == "INCONCLUSIVE: an agent metric read failed (${expected})." ]] || {
    printf 'FAIL: expected %s response classification: %s\n' "${expected}" "${output}" >&2; exit 1;
  }
done <<'RESPONSES'
EMPTY_OUTPUT|
EMPTY_OUTPUT|\n \t\n
MULTIPLE_JSON_VALUES|[]\n[]
MULTIPLE_JSON_VALUES|null\n[]
MULTIPLE_JSON_VALUES|[]\nnull
INVALID_JSON|[]\nPRIVATE_MALFORMED
RESPONSES
# A null limiter read with a live registry is an agent that has no series yet. It is
# reported as such and still publishes no samples.
unobserved_one='INCONCLUSIVE: 1 of 1 agents report no endpoint-create limiter series (LIMITER_UNOBSERVED).'
for response in 'null' ' \n null \t\n'; do
  reset_fixture
  printf '%b' "${response}" >"${fixture}/metrics.json"
  require_inconclusive
  [[ "${output}" == "${unobserved_one}" ]] || {
    printf 'FAIL: null read with a live registry: %s\n' "${output}" >&2; exit 1;
  }
done
# Series for other calls only are the same state, and need no control read.
reset_fixture
jq -n '[{name:"cilium_api_limiter_rate_limit",labels:{api_call:"endpoint-list",value:"limit"},value:1}]' >"${fixture}/metrics.json"
require_inconclusive
[[ "${output}" == "${unobserved_one}" ]] || {
  printf 'FAIL: other calls only: %s\n' "${output}" >&2; exit 1;
}
! grep -q 'match-pattern ^cilium_version' "${fixture}/calls" || exit 1
# A stable agent proven to have no series is counted beside the complete agent, with no values.
reset_fixture
add_second_agent
printf 'null' >"${fixture}/second-metrics.json"
run_read --context admin@prod
[[ "${rc}" -eq 0 && "${output}" == *'PARTIAL: five complete samples for 1 of 2 stable ready agents; 1 report no endpoint-create limiter series;'* && "${output}" != *MEASURED* ]] || {
  printf 'FAIL: partial result: %s (%s)\n' "${output}" "${rc}" >&2; exit 1;
}
[[ "$(printf '%s\n' "${output}" | grep '^{' | jq -s -r '([.[].agent] | unique | tojson) + " " + (length | tostring)')" == '[1] 5' ]] || {
  printf 'FAIL: partial samples: %s\n' "${output}" >&2; exit 1;
}
# Five limiter reads each, plus a control read for each null response.
[[ "$(grep -c '^--context.* exec ' "${fixture}/calls")" -eq 15 ]] || {
  printf 'FAIL: partial result read count\n' >&2; exit 1;
}
# A series that appears or disappears between samples is neither complete nor absent.
reset_fixture
add_second_agent
printf 'null' >"${fixture}/second-metrics.json"
cp "${fixture}/metrics.json" "${fixture}/second-late.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent gained or lost its endpoint-create limiter series during collection.' ]] || {
  printf 'FAIL: series appeared mid-run: %s\n' "${output}" >&2; exit 1;
}
reset_fixture
add_second_agent
printf 'null' >"${fixture}/second-late.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent gained or lost its endpoint-create limiter series during collection.' ]] || {
  printf 'FAIL: series vanished mid-run: %s\n' "${output}" >&2; exit 1;
}
# A partial result still requires the same stable roster.
reset_fixture
add_second_agent
printf 'null' >"${fixture}/second-metrics.json"
jq '.items[1].metadata.uid="33333333-3333-3333-3333-333333333333"' "${fixture}/pods.json" >"${fixture}/after.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: agent identity or container state changed during collection.' ]] || {
  printf 'FAIL: replaced agent in a partial result: %s\n' "${output}" >&2; exit 1;
}
reset_fixture
add_second_agent
printf 'null' >"${fixture}/metrics.json"
printf 'null' >"${fixture}/second-metrics.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: 2 of 2 agents report no endpoint-create limiter series (LIMITER_UNOBSERVED).' ]] || {
  printf 'FAIL: all agents unobserved: %s\n' "${output}" >&2; exit 1;
}
# Two complete agents still measure.
reset_fixture
add_second_agent
run_read --context admin@prod
[[ "${rc}" -eq 0 && "${output}" == *MEASURED* ]] || {
  printf 'FAIL: two complete agents: %s (%s)\n' "${output}" "${rc}" >&2; exit 1;
}
[[ "$(grep -c '^--context.* exec ' "${fixture}/calls")" -eq 10 ]] || exit 1
# A replaced agent also has no series, so it must not be reported as unobserved.
reset_fixture
printf 'null' >"${fixture}/metrics.json"
jq '.items[0].metadata.uid="33333333-3333-3333-3333-333333333333"' "${fixture}/pods.json" >"${fixture}/after.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: agent identity or container state changed during collection.' ]] || {
  printf 'FAIL: replaced agent: %s\n' "${output}" >&2; exit 1;
}
# A control read that cannot show a live registry keeps the null a read failure.
while IFS='|' read -r expected response; do
  reset_fixture
  printf 'null' >"${fixture}/metrics.json"
  printf '%b' "${response}" >"${fixture}/control.json"
  require_inconclusive
  [[ "${output}" == "INCONCLUSIVE: an agent metric read failed (JSON_NULL; control ${expected})." ]] || {
    printf 'FAIL: expected control %s: %s\n' "${expected}" "${output}" >&2; exit 1;
  }
done <<'CONTROLS'
JSON_NULL|null
EMPTY_OUTPUT|
INVALID_JSON|{malformed
REGISTRY_EMPTY|[]
REGISTRY_EMPTY|[{"name":"PRIVATE_METRIC"}]
CONTROLS
reset_fixture
printf 'null' >"${fixture}/metrics.json"
touch "${fixture}/control-denied"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent metric read failed (JSON_NULL; control COMMAND_FAILED).' ]] || {
  printf 'FAIL: denied control: %s\n' "${output}" >&2; exit 1;
}
# Only well-formed series for other calls show that no endpoint-create series exists.
while IFS= read -r response; do
  reset_fixture
  printf '%s' "${response}" >"${fixture}/metrics.json"
  require_inconclusive
  [[ "${output}" == 'INCONCLUSIVE: a complete finite metric set could not be established.' ]] || {
    printf 'FAIL: unproven unobserved claim: %s (%s)\n' "${output}" "${response}" >&2; exit 1;
  }
done <<'SERIES'
[{"name":"PRIVATE_METRIC","labels":{"api_call":"endpoint-list"},"value":1}]
[{"name":"cilium_api_limiter_rate_limit"}]
[{"name":"cilium_api_limiter_rate_limit","labels":null,"value":1}]
[{"name":"cilium_api_limiter_rate_limit","labels":{"call":"endpoint-create","value":"limit"}}]
[{"name":"cilium_api_limiter_rate_limit","labels":{"api_call":"endpoint-list"}},{"name":"PRIVATE_METRIC"}]
SERIES
while IFS='|' read -r expected exit_code message; do
  reset_fixture
  printf 'PRIVATE_ERROR https://198.51.100.9:6443 %s\n' "${message}" >"${fixture}/stderr"
  printf '%s\n' "${exit_code}" >"${fixture}/exit-code"
  require_inconclusive
  [[ "${output}" == "INCONCLUSIVE: an agent metric read failed (${expected})." ]] || {
    printf 'FAIL: expected %s classification: %s\n' "${expected}" "${output}" >&2; exit 1;
  }
done <<'CASES'
DEADLINE_EXCEEDED|124|context deadline exceeded
ACCESS_DENIED|1|Error from server (Forbidden): pods is forbidden
CLI_REJECTED|1|Error: unknown flag: --match-pattern
HIVE_SOCKET_UNAVAILABLE|1|dial unix /var/run/cilium/shell.sock: connect: no such file or directory
JSON_ENCODING_FAILED|1|json: unsupported value: NaN
JSON_ENCODING_FAILED|1|json: unsupported value: +Inf
TRANSPORT_FAILURE|1|error: error upgrading connection: backend unavailable
COMMAND_FAILED|1|unclassified synthetic failure
CASES
reset_fixture
printf '%8192sPRIVATE_ERROR Forbidden\n' '' >"${fixture}/stderr"
printf '1\n' >"${fixture}/exit-code"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent metric read failed (COMMAND_FAILED).' ]] || {
  printf 'FAIL: classification read beyond the private prefix bound\n' >&2; exit 1;
}
reset_fixture
touch "${fixture}/denied"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent metric read failed (COMMAND_FAILED).' ]] || exit 1
reset_fixture
printf '%1048577s' '' >"${fixture}/metrics.json"
require_inconclusive
[[ "${output}" == 'INCONCLUSIVE: an agent metric read failed (OUTPUT_LIMIT).' ]] || exit 1
workflow="${root_dir}/.github/workflows/read-cilium-api-limiter.yaml"
[[ -f "${workflow}" ]] || { printf 'FAIL: reviewed dispatch workflow missing\n' >&2; exit 1; }
[[ "$(yq -r '.on | keys | join(",")' "${workflow}")" == workflow_dispatch ]] || exit 1
[[ "$(yq -r '.permissions | length' "${workflow}")" == 0 ]] || exit 1
[[ "$(yq -r '.jobs.read.permissions | keys | join(",")' "${workflow}")" == contents ]] || exit 1
[[ "$(yq -r '.jobs.read.permissions.contents' "${workflow}")" == read ]] || exit 1
[[ "$(yq -r '.jobs.read.environment' "${workflow}")" == prod ]] || exit 1
[[ "$(yq -r '.jobs.read.steps[1].with.persist-credentials' "${workflow}")" == false ]] || exit 1
[[ "$(yq -r '.jobs.read.steps[1].with.ref' "${workflow}")" == "\${{ github.sha }}" ]] || exit 1
[[ "$(yq -r '.jobs.read.steps[-1].run' "${workflow}")" == './scripts/read-cilium-api-limiter.sh --context admin@prod' ]] || exit 1
[[ "$(yq -r '.concurrency.group' "${workflow}")" == prod-deploy ]] || exit 1
[[ "$(yq -r '.jobs.read.timeout-minutes' "${workflow}")" -ge 60 ]] || exit 1
guard="$(yq -r '.jobs.read.steps[0].run' "${workflow}")"
RUN_REF=refs/heads/main bash -c "${guard}" >/dev/null
if RUN_REF=refs/heads/unreviewed bash -c "${guard}" >/dev/null 2>&1; then exit 1; fi
guard="$(yq -r '.jobs.read.steps[2].run' "${workflow}")"
CONFIRM=read-production-cilium-api-limiter bash -c "${guard}" >/dev/null
if CONFIRM=incorrect bash -c "${guard}" >/dev/null 2>&1; then exit 1; fi
[[ "$(yq -r '.jobs.read.steps[3].env.KUBE_CONFIG' "${workflow}")" == "\${{ secrets.KUBE_CONFIG }}" ]] || exit 1
[[ "$(yq -r '.jobs.read.steps[4].run' "${workflow}")" == './scripts/use-prod-stable-api-endpoint.sh >/dev/null' ]] || exit 1
printf 'read-cilium-api-limiter: complete samples, fixed reads, privacy and failure controls passed.\n'
