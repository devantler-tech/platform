#!/usr/bin/env bash

# Exercise the deployed stranded-autoscaler-node sensor against stubbed API
# responses, and keep its declared pools equal to ksail.prod.yaml.
# Only network calls and in-cluster mount paths are replaced.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-stranded-autoscaler-node-alerter.yaml"
role="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cluster-role-stranded-autoscaler-node-alerter.yaml"
kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/kustomization.yaml"
ksail_config="${root_dir}/ksail.prod.yaml"
work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

container='.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "alerter")'

# ---- The declared pools are exactly ksail.prod.yaml's pools --------------
# A pool removed from ksail.prod.yaml without updating this list would leave its
# nodes unreported, and a pool added there without it would page on every node
# the autoscaler legitimately creates. Either direction fails here.
config_pools="$(yq -r '.spec.cluster.autoscaler.node.pools[].name' "${ksail_config}" | sort | tr '\n' ' ' | sed 's/ $//')"
[ -n "${config_pools}" ] || fail 'no autoscaler pools found in ksail.prod.yaml; the query path is wrong'
declared_pools="$(yq -r "${container} | .env[] | select(.name == \"DECLARED_POOLS\") | .value" "${manifest}" |
  tr ' ' '\n' | sed '/^$/d' | sort | tr '\n' ' ' | sed 's/ $//')"
[ "${declared_pools}" = "${config_pools}" ] ||
  fail "DECLARED_POOLS (${declared_pools}) must equal the pools in ksail.prod.yaml (${config_pools})"
for pool in ${config_pools}; do
  case "${pool}" in
    autoscale-*) ;;
    *) fail "pool ${pool} does not start with autoscale-, so the sensor would never see its nodes" ;;
  esac
done

# ---- Wiring and least privilege -------------------------------------------
for file in cron-job service-account cluster-role cluster-role-binding; do
  grep -Fqx "  - coroot/${file}-stranded-autoscaler-node-alerter.yaml" "${kustomization}" ||
    fail "${file} is not listed in the hetzner infrastructure kustomization"
done
[ "$(yq -o=json '.rules' "${role}" | jq -c .)" = '[{"apiGroups":[""],"resources":["nodes"],"verbs":["list"]}]' ] ||
  fail 'the ClusterRole must grant list on nodes and nothing else'
yq -e '.metadata.annotations."kustomize.toolkit.fluxcd.io/substitute" == "disabled"' "${manifest}" >/dev/null ||
  fail 'Flux substitution would blank the pod environment variables the script reads'
yq -e "${container} | .securityContext.readOnlyRootFilesystem == true" "${manifest}" >/dev/null ||
  fail 'the sensor must not need a writable root filesystem'
# curl restarts --max-time on every retry, so only --retry-max-time bounds a call.
script_body="$(yq -r "${container} | .command[2]" "${manifest}")"
curl_calls="$(grep -c 'curl -sS' <<<"${script_body}")"
bounded_calls="$(grep -c 'curl -sS.*--retry-max-time\|--max-time [0-9]* --retry-max-time' <<<"${script_body}")"
[ "${curl_calls}" -ge 2 ] && [ "${curl_calls}" = "${bounded_calls}" ] ||
  fail "every curl call must bound its whole retry sequence (${bounded_calls} of ${curl_calls})"
[ "$(yq -r '.spec.jobTemplate.spec.activeDeadlineSeconds' "${manifest}")" -ge 120 ] ||
  fail 'the Job deadline must cover the node read and both Alertmanager peers'
grep -Fq 'observability/stranded-autoscaler-node-alerter:' \
  "${root_dir}/k8s/bases/components/coroot-cronjob-failure-alert/cron-job-cronjob-failure-alert.yaml" ||
  fail 'the sensor must be on the CronJob failure detector watch list, or a broken sensor reads as a clean cluster'

# ---- Behaviour -------------------------------------------------------------
yq -r "${container} | .command[2]" "${manifest}" >"${work_dir}/sensor.sh"
mkdir -p "${work_dir}/sa" "${work_dir}/bin"
printf 'fixture-token\n' >"${work_dir}/sa/token"
printf 'fixture-ca\n' >"${work_dir}/sa/ca.crt"

cat >"${work_dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
output='' payload='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    -d) payload="$2"; shift 2 ;;
    -w|--cacert|-H|-X|--retry|--retry-delay|--max-time|--retry-max-time) shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  */api/v1/nodes\?limit=500)
    [ -n "$output" ] || exit 91
    cat "${CASE_DIR}/nodes.json" >"$output"
    cat "${CASE_DIR}/code" ;;
  *alertmanager-*/api/v2/alerts)
    printf '%s\n' "$payload" >>"${CASE_DIR}/alerts.jsonl"
    [ ! -e "${CASE_DIR}/reject-alerts" ] || exit 22 ;;
  *) exit 93 ;;
esac
STUB
chmod +x "${work_dir}/bin/curl"

node() { # name [control-plane]
  if [ "${2:-}" = cp ]; then
    jq -nc --arg n "$1" '{metadata:{name:$n,creationTimestamp:"2026-09-01T00:00:00Z",labels:{"node-role.kubernetes.io/control-plane":""}}}'
  else
    jq -nc --arg n "$1" '{metadata:{name:$n,creationTimestamp:"2026-09-01T00:00:00Z",labels:{}}}'
  fi
}

new_case() { # name node...
  case_dir="${work_dir}/$1"; shift
  mkdir -p "${case_dir}/tmp"
  printf '200' >"${case_dir}/code"
  printf '%s\n' "$@" | jq -s '{kind:"NodeList",metadata:{},items:.}' >"${case_dir}/nodes.json"
  sed -e "s#/tmp/#${case_dir}/tmp/#g" \
    -e "s#/var/run/secrets/kubernetes.io/serviceaccount#${work_dir}/sa#g" \
    "${work_dir}/sensor.sh" >"${case_dir}/sensor.sh"
}

run_case() {
  result=0
  PATH="${work_dir}/bin:${PATH}" CASE_DIR="${case_dir}" DECLARED_POOLS="${pools:-autoscale-cx43 autoscale-cx53}" \
    KUBERNETES_SERVICE_HOST=api.example.invalid KUBERNETES_SERVICE_PORT_HTTPS=443 \
    sh "${case_dir}/sensor.sh" >"${case_dir}/output.log" 2>&1 || result=$?
}

baseline=("$(node prod-control-plane-2 cp)" "$(node prod-worker-1)")

new_case healthy "${baseline[@]}" "$(node autoscale-cx43-43ba6e06c178f438)" "$(node autoscale-cx53-0123456789abcdef)"
run_case
[ "$result" = 0 ] || fail "declared-pool nodes must succeed: $(cat "${case_dir}/output.log")"
[ ! -e "${case_dir}/alerts.jsonl" ] || fail 'declared-pool nodes must not alert'
grep -q 'every autoscaler node belongs to a declared pool' "${case_dir}/output.log" || fail 'a clean result must say so'

# The #3178 shape: a node from a pool that ksail.prod.yaml no longer declares.
new_case stranded "${baseline[@]}" "$(node autoscale-cx43-43ba6e06c178f438)" "$(node autoscale-cx33-3132cc6ce1a4b0c9)"
run_case
[ "$result" = 0 ] || fail "a stranded node must be delivered: $(cat "${case_dir}/output.log")"
[ "$(wc -l <"${case_dir}/alerts.jsonl" | tr -d ' ')" = 2 ] || fail 'every Alertmanager peer must receive the alert'
head -n1 "${case_dir}/alerts.jsonl" | jq -e 'length == 1
  and .[0].labels.alertname == "StrandedAutoscalerNode"
  and .[0].labels.node == "autoscale-cx33-3132cc6ce1a4b0c9"
  and .[0].labels.pool == "autoscale-cx33"
  and (.[0].endsAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))' >/dev/null ||
  fail 'the alert must name the stranded node and its pool, and only that node'

# A pool whose name is a prefix of a declared one must not hide behind it.
new_case prefix "${baseline[@]}" "$(node autoscale-cx4-0123456789abcdef)"
run_case
head -n1 "${case_dir}/alerts.jsonl" | jq -e '.[0].labels.pool == "autoscale-cx4"' >/dev/null ||
  fail 'pool matching must be exact, not by prefix'

# An autoscaler-prefixed name without the <pool>-<hex> shape is reported, not ignored.
new_case unparsed "${baseline[@]}" "$(node autoscale-manual)"
run_case
head -n1 "${case_dir}/alerts.jsonl" | jq -e '.[0].labels.pool == "unparsed"' >/dev/null ||
  fail 'an unparseable autoscaler node name must alert'

new_case rejected "${baseline[@]}" "$(node autoscale-cx33-3132cc6ce1a4b0c9)"
touch "${case_dir}/reject-alerts"
run_case
[ "$result" != 0 ] || fail 'an alert no peer accepted must fail the Job'

# Broken reads must fail, never report "nothing stranded".
for problem in http-error wrong-kind paginated empty no-control-plane no-pools; do
  new_case "$problem" "${baseline[@]}" "$(node autoscale-cx33-3132cc6ce1a4b0c9)"
  pools=''
  case "$problem" in
    http-error) printf '403' >"${case_dir}/code" ;;
    wrong-kind) printf '{"kind":"Status","items":[]}' >"${case_dir}/nodes.json" ;;
    paginated) jq '.metadata.continue = "next"' "${case_dir}/nodes.json" >"${case_dir}/n.json" && mv "${case_dir}/n.json" "${case_dir}/nodes.json" ;;
    empty) printf '{"kind":"NodeList","metadata":{},"items":[]}' >"${case_dir}/nodes.json" ;;
    no-control-plane) jq '.items |= map(.metadata.labels = {})' "${case_dir}/nodes.json" >"${case_dir}/n.json" && mv "${case_dir}/n.json" "${case_dir}/nodes.json" ;;
    no-pools) pools=' ' ;;
  esac
  run_case
  unset pools
  [ "$result" != 0 ] || fail "${problem} must fail the Job rather than claim a clean cluster"
  [ ! -e "${case_dir}/alerts.jsonl" ] || fail "${problem} must not deliver a partial result"
  ! grep -q 'every autoscaler node belongs' "${case_dir}/output.log" || fail "${problem} reported a clean cluster"
done

printf 'PASS: stranded autoscaler node alert\n'
