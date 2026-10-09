#!/usr/bin/env bash

# Exercise the deployed Crossview login sensor (#3315) against stubbed
# /api/auth/check answers and captured Alertmanager deliveries, and pin the
# wiring that lets it reach both. Only network calls, sleeps and in-pod mount
# paths are replaced; the script under test is the one the CronJob runs.
#
# The broken-state body below is the one Crossview served during the August 2026
# outage, recorded on #3315. test-crossview-login-alerter-runtime.sh reproduces
# that state against the real pinned app and database.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-crossview-login-alerter.yaml"
readonly app_policy="${root_dir}/k8s/providers/hetzner/apps/crossview/cilium-network-policy-login-alerter.yaml"
readonly alertmanager_policy="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/alertmanager/cilium-network-policy.yaml"
readonly watcher="${root_dir}/k8s/bases/components/coroot-cronjob-failure-alert/cron-job-cronjob-failure-alert.yaml"
readonly watcher_role="${root_dir}/k8s/bases/components/coroot-cronjob-failure-alert/role-cronjob-failure-alert-observability.yaml"
readonly alerting_doc="${root_dir}/docs/dr/alerting.md"
work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$1"; }

for tool in jq kubectl yq; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

# The runtime fixture configures only an ephemeral hosted daemon. Exercise the
# actual command boundary, including failed setup, without touching this host.
mkdir -p "${work_dir}/cache-bin"
cat >"${work_dir}/cache-bin/sudo" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"${CACHE_CASE}/calls"
case "$1" in
  test)
    [ "${CACHE_MODE:-}" != test-failed ] || exit 2
    if [ "$2" = ! ]; then test ! -e "${CACHE_CASE}/original.json"
    else test -e "${CACHE_CASE}/original.json"; fi ;;
  cat) cat "${CACHE_CASE}/original.json" ;;
  dockerd) [ "${CACHE_MODE:-}" != invalid-daemon ] ;;
  mkdir) : ;;
  install)
    [ "${CACHE_MODE:-}" != install-failed ] || exit 1
    cp "$4" "${CACHE_CASE}/installed.json" ;;
  systemctl) [ "${CACHE_MODE:-}" != restart-failed ] ;;
  *) exit 93 ;;
esac
STUB
cat >"${work_dir}/cache-bin/docker" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker-%s\n' "$1" >>"${CACHE_CASE}/calls"
case "$1" in
  ps)
    [ "$2" = -aq ] || exit 95
    [ "${CACHE_MODE:-}" != list-failed ] || exit 1
    [ "${CACHE_MODE:-}" != occupied ] || printf 'fixture-container\n' ;;
  info)
    [ "${CACHE_MODE:-}" != readback-failed ] || exit 1
    if [ "${CACHE_MODE:-}" = missing-mirror ] ||
      [ ! -e "${CACHE_CASE}/installed.json" ] ||
      ! grep -qx systemctl "${CACHE_CASE}/calls"; then printf '[]\n'
    else printf '["https://mirror.gcr.io/"]\n'; fi ;;
  *) exit 94 ;;
esac
STUB
chmod +x "${work_dir}/cache-bin/sudo" "${work_dir}/cache-bin/docker"
cache_case() {
  cache_dir="${work_dir}/cache-$1"
  mkdir -p "${cache_dir}"
  : >"${cache_dir}/calls"
  if [ "$#" -ge 2 ]; then printf '%s\n' "$2" >"${cache_dir}/original.json"; fi
}
cache_run() {
  cache_rc=0
  PATH="${work_dir}/cache-bin:${PATH}" CACHE_CASE="${cache_dir}" CACHE_MODE="${1:-}" \
    GITHUB_ACTIONS="${4:-true}" RUNNER_ENVIRONMENT="${2:-github-hosted}" RUNNER_OS="${3:-Linux}" \
    bash "${root_dir}/scripts/tests/test-crossview-login-alerter-runtime.sh" --configure-registry-cache \
    >"${cache_dir}/output" 2>&1 || cache_rc=$?
}
cache_case preserve '{"debug":false,"log-driver":"local","registry-mirrors":["https://mirror.example.invalid","https://mirror.gcr.io"]}'
cache_run
[ "${cache_rc}" = 0 ] || fail 'hosted cache setup must succeed'
jq -e '.debug == false and .["log-driver"] == "local" and
  .["registry-mirrors"] == ["https://mirror.gcr.io","https://mirror.example.invalid"]' \
  "${cache_dir}/installed.json" >/dev/null || fail 'cache setup must preserve unrelated settings and mirrors'
grep -q '^systemctl$' "${cache_dir}/calls" || fail 'the daemon must restart before cache readback'
cache_case absent
cache_run
[ "${cache_rc}" = 0 ] || fail 'an absent daemon file must be initialized'
for config in 'not-json' '[]' 'null' '{} {}' '{"registry-mirrors":null}' '{"registry-mirrors":"bad"}' '{"registry-mirrors":[7]}'; do
  cache_case malformed "${config}"
  cache_run
  [ "${cache_rc}" != 0 ] || fail 'malformed daemon configuration must fail'
  [ ! -e "${cache_dir}/installed.json" ] || fail 'malformed configuration must not be installed'
done
for mode in occupied list-failed test-failed invalid-daemon install-failed restart-failed readback-failed missing-mirror; do
  cache_case "${mode}" '{}'
  cache_run "${mode}"
  [ "${cache_rc}" != 0 ] || fail "${mode} must fail instead of admitting the runtime fixture"
done
for runner in self-hosted ''; do
  cache_case not-hosted '{}'
  cache_run '' "${runner:-local}"
  [ "${cache_rc}" != 0 ] || fail 'non-hosted daemon changes must be refused'
  [ ! -s "${cache_dir}/calls" ] || fail 'non-hosted refusal must precede all daemon commands'
done
cache_case not-linux '{}'
cache_run '' github-hosted macOS
[ "${cache_rc}" != 0 ] && [ ! -s "${cache_dir}/calls" ] || fail 'non-Linux daemon changes must be refused'
cache_case not-actions '{}'
cache_run '' github-hosted Linux false
[ "${cache_rc}" != 0 ] && [ ! -s "${cache_dir}/calls" ] || fail 'local daemon changes must be refused'
pass 'hosted registry cache preserves configuration and fails closed without changing local daemons'

# The earlier changes-job fixture builds a pinned Docker Hub toolbox too. Its
# hosted setup must finish before any build; failed setup must stop the fixture.
cache_runtime_run() {
  cache_rc=0
  PATH="${work_dir}/cache-bin:${PATH}" CACHE_CASE="${cache_dir}" CACHE_MODE="${1:-}" \
    GITHUB_ACTIONS="${2:-true}" RUNNER_ENVIRONMENT=github-hosted RUNNER_OS=Linux \
    bash "${root_dir}/scripts/tests/test-wedding-backup-denial-runtime.sh" \
    >"${cache_dir}/output" 2>&1 || cache_rc=$?
}
cache_case wedding-hosted '{}'
cache_runtime_run
[ "${cache_rc}" = 94 ] || fail 'the hosted Wedding fixture must reach the stubbed build only after setup'
awk '/^docker-info$/ { readback=1 } /^docker-build$/ { if (!readback) exit 1; built=1 } END { if (!built) exit 1 }' \
  "${cache_dir}/calls" || fail 'the Wedding build must follow cache readback'
cache_case wedding-setup-failed '{}'
cache_runtime_run invalid-daemon
if [ "${cache_rc}" = 0 ] || grep -qx docker-build "${cache_dir}/calls"; then
  fail 'failed cache setup must stop the Wedding fixture before its build'
fi
cache_case wedding-local '{}'
cache_runtime_run '' false
[ "${cache_rc}" = 94 ] && [ ! -e "${cache_dir}/installed.json" ] ||
  fail 'a local Wedding fixture must build without reconfiguring Docker'
pass 'both hosted runtime fixtures configure the cache before pulling pinned images'

readonly pod='.spec.jobTemplate.spec.template.spec'
readonly container="${pod}.containers[] | select(.name == \"alerter\")"
env_value() { yq -r "${container} | .env[] | select(.name == \"$1\") | .value" "${manifest}"; }
script_body="$(yq -r "${container} | .command[2]" "${manifest}")"
readonly script_body
if [ -z "${script_body}" ] || [ "${script_body}" = null ]; then
  fail 'could not extract the sensor script'
fi

# ---- Wiring ----------------------------------------------------------------
# Render what Flux applies, so a file that exists but is not referenced fails.
render() { kubectl kustomize "${root_dir}/$1" | yq ea -o=json -I=0 '[.]'; }
render k8s/providers/hetzner/infrastructure >"${work_dir}/prod-infrastructure.json"
render k8s/providers/hetzner/apps >"${work_dir}/prod-apps.json"
render k8s/providers/docker/infrastructure >"${work_dir}/local-infrastructure.json"
render k8s/providers/docker/apps >"${work_dir}/local-apps.json"

jq -e '[.[] | select(.kind == "CronJob" and .metadata.namespace == "observability"
  and .metadata.name == "crossview-login-alerter")] | length == 1' \
  "${work_dir}/prod-infrastructure.json" >/dev/null ||
  fail 'the production infrastructure layer must deploy the sensor'
jq -e '[.[] | select(.kind == "CiliumNetworkPolicy" and .metadata.namespace == "crossview"
  and .metadata.name == "allow-login-alerter")] | length == 1' \
  "${work_dir}/prod-apps.json" >/dev/null ||
  fail 'the production apps layer must admit the sensor to Crossview'
for layer in local-infrastructure local-apps; do
  jq -e '[.[] | select(.metadata.name == "crossview-login-alerter"
    or .metadata.name == "allow-login-alerter")] | length == 0' \
    "${work_dir}/${layer}.json" >/dev/null ||
    fail "the local provider (${layer}) has no Crossview or Alertmanager, so it must not carry the sensor"
done
pass 'the sensor and its network path deploy to production only'

label="$(yq -r '.spec.jobTemplate.spec.template.metadata.labels.app' "${manifest}")"
[ "${label}" = crossview-login-alerter ] || fail "unexpected sensor pod label '${label}'"
policy_json() { yq -o=json "$1" "${app_policy}" | jq -cS .; }
[ "$(policy_json '.spec.endpointSelector')" = \
  '{"matchLabels":{"app.kubernetes.io/component":"app","app.kubernetes.io/name":"crossview"}}' ] ||
  fail 'the Crossview policy must select only the app pods'
[ "$(policy_json '.spec')" = \
  "{\"endpointSelector\":{\"matchLabels\":{\"app.kubernetes.io/component\":\"app\",\"app.kubernetes.io/name\":\"crossview\"}},\"ingress\":[{\"fromEndpoints\":[{\"matchLabels\":{\"app\":\"${label}\",\"k8s:io.kubernetes.pod.namespace\":\"observability\"}}],\"toPorts\":[{\"ports\":[{\"port\":\"3001\",\"protocol\":\"TCP\"}]}]}]}" ] ||
  fail 'the Crossview policy must admit only the sensor pod label to port 3001, and allow nothing else'
yq -e ".spec.ingress[] | select(.fromEndpoints[0].matchLabels.app == \"${label}\"
  and .fromEndpoints[0].matchLabels.\"k8s:io.kubernetes.pod.namespace\" == \"observability\"
  and .toPorts[0].ports[0].port == \"9093\")" "${alertmanager_policy}" >/dev/null ||
  fail 'Alertmanager must admit the sensor pod label to its v2 API'
for target in \
  'http://crossview-service.crossview.svc.cluster.local.:80/api/auth/check' \
  'http://alertmanager-0.alertmanager-headless.kubescape.svc.cluster.local.:9093' \
  'http://alertmanager-1.alertmanager-headless.kubescape.svc.cluster.local.:9093'; do
  grep -Fq "${target}" <<<"${script_body}" || fail "target is not an absolute cluster DNS name: ${target}"
done
pass 'both network policies admit exactly the sensor pod, on absolute DNS names'

yq -e '.metadata.annotations."kustomize.toolkit.fluxcd.io/substitute" == "disabled"' "${manifest}" >/dev/null ||
  fail 'Flux substitution would blank the shell variables the script reads'
yq -e "${pod}.automountServiceAccountToken == false" "${manifest}" >/dev/null ||
  fail 'the sensor needs no Kubernetes API access, so it must not mount a token'
yq -e "${container} | .securityContext.readOnlyRootFilesystem == true" "${manifest}" >/dev/null ||
  fail 'the sensor must not need a writable root filesystem'
scratch="$(yq -r "${container} | .volumeMounts[] | select(.mountPath == \"/tmp\") | .name" "${manifest}")"
yq -e "${pod}.volumes[] | select(.name == \"${scratch}\") | .emptyDir.sizeLimit != null" "${manifest}" >/dev/null ||
  fail 'the sensor scratch space must be a bounded emptyDir'
[ "$(yq -r "${pod}.securityContext.fsGroup" "${manifest}")" = "$(yq -r "${pod}.securityContext.runAsUser" "${manifest}")" ] ||
  fail 'the non-root sensor must be able to write its scratch volume'
pass 'the sensor runs read-only, without a token, on bounded scratch'

# curl restarts --max-time on every retry, so only --retry-max-time bounds a call.
curl_calls="$(grep -c 'curl -sS' <<<"${script_body}")"
[ "${curl_calls}" = 2 ] || fail "expected one read and one delivery call, found ${curl_calls} curl calls"
# shellcheck disable=SC2016 # matches the script's literal "$BODY"
grep -Fq -- '--max-time 10 --retry-max-time 30 -o "$BODY"' <<<"${script_body}" ||
  fail 'each read must bound its retries to 30s'
grep -Fq -- '--max-time 10 --retry-max-time 20' <<<"${script_body}" ||
  fail 'each delivery must bound its retries to 20s'
samples="$(env_value SAMPLES)"
interval="$(env_value SAMPLE_INTERVAL_SECONDS)"
deadline="$(yq -r '.spec.jobTemplate.spec.activeDeadlineSeconds' "${manifest}")"
# --retry-max-time stops only NEW attempts, so one call can run its retry budget
# plus one more --max-time attempt: 30+10 per read, 20+10 per Alertmanager peer.
worst=$((samples * 40 + (samples - 1) * interval + 2 * 30))
[ "${deadline}" -gt "${worst}" ] || fail "the Job deadline (${deadline}s) must cover the worst case (${worst}s)"
# One healthy read ends a run; a page needs every read over this window false,
# so a planned database restart (volume detach, reattach, start) is quiet.
if [ "${samples}" -lt 3 ] || [ $(((samples - 1) * interval)) -lt 300 ]; then
  fail 'an alert must need at least three false reads spanning five minutes'
fi
pass "the deadline covers the worst case (${worst}s of ${deadline}s) and the alert needs a sustained answer"

watch_entry="$(yq -r '.spec.jobTemplate.spec.template.spec.containers[0].env[] | select(.name == "WATCH") | .value' "${watcher}" |
  tr ' ' '\n' | grep '^observability/crossview-login-alerter:' || true)"
[ "${watch_entry}" = 'observability/crossview-login-alerter:2:3600' ] ||
  fail 'the sensor must be on the CronJob failure detector watch list, or a broken sensor reads as a healthy login'
yq -e '.rules[] | select(.resources[] == "cronjobs") | .resourceNames[] | select(. == "crossview-login-alerter")' \
  "${watcher_role}" >/dev/null || fail 'the failure detector must be allowed to read the sensor CronJob'
[ "$(yq -r '.spec.failedJobsHistoryLimit' "${manifest}")" -ge 2 ] ||
  fail 'the sensor must retain at least as many failed Jobs as the detector threshold'
grep -Fq 'providers/hetzner/infrastructure/coroot/cron-job-crossview-login-alerter.yaml' "${alerting_doc}" ||
  fail 'the alerting runbook must point at the sensor'
pass 'a failing sensor is reported by the CronJob failure detector'

# ---- Behaviour -------------------------------------------------------------
mkdir -p "${work_dir}/bin"
cat >"${work_dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
output='' payload='' url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output="$2"; shift 2 ;;
    -d) payload="$2"; shift 2 ;;
    -w|-H|-X|--retry|--retry-delay|--max-time|--retry-max-time) shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
case "$url" in
  http://crossview-service.crossview.svc.cluster.local.:80/api/auth/check)
    n=$(($(cat "${CASE_DIR}/reads" 2>/dev/null || echo 0) + 1))
    printf '%s\n' "$n" >"${CASE_DIR}/reads"
    answer="${CASE_DIR}/read-${n}"
    [ -e "${answer}.code" ] || answer="${CASE_DIR}/read-default"
    code="$(cat "${answer}.code")"
    if [ "$code" = refused ]; then
      printf '000'
      exit 7
    fi
    cat "${answer}.body" >"$output"
    printf '%s' "$code" ;;
  http://alertmanager-[01].alertmanager-headless.kubescape.svc.cluster.local.:9093/api/v2/alerts)
    peer="${url#http://alertmanager-}"
    peer="${peer%%.*}"
    printf '%s\n' "$payload" >"${CASE_DIR}/alerts-${peer}.json"
    [ ! -e "${CASE_DIR}/reject-${peer}" ] || exit 22 ;;
  *) exit 93 ;;
esac
STUB
cat >"${work_dir}/bin/sleep" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$1" >>"${CASE_DIR}/sleeps"
STUB
chmod +x "${work_dir}/bin/curl" "${work_dir}/bin/sleep"

readonly healthy_body='{"authMode":"session","authenticated":false,"hasAdmin":true,"hasUsers":true}'
# Verbatim from the August 2026 outage (#3315).
readonly broken_body='{"authMode":"session","authenticated":false,"hasAdmin":false,"hasUsers":false}'

# answer <n|default> <code|refused> [body]
answer() {
  printf '%s\n' "$2" >"${case_dir}/read-$1.code"
  printf '%s\n' "${3:-}" >"${case_dir}/read-$1.body"
}
new_case() {
  case_dir="${work_dir}/case-$1"
  mkdir -p "${case_dir}/tmp"
  printf '%s\n' "${script_body//\/tmp\//${case_dir}/tmp/}" >"${case_dir}/sensor.sh"
  answer default 200 "${healthy_body}"
}
run_case() {
  result=0
  PATH="${work_dir}/bin:${PATH}" CASE_DIR="${case_dir}" \
    SAMPLES="${SAMPLES_OVERRIDE-${samples}}" SAMPLE_INTERVAL_SECONDS="${interval}" \
    sh "${case_dir}/sensor.sh" >"${case_dir}/output.log" 2>&1 || result=$?
}
reads() { cat "${case_dir}/reads" 2>/dev/null || echo 0; }
no_alert() { ! ls "${case_dir}"/alerts-*.json >/dev/null 2>&1 || fail "$1 must not alert"; }

new_case healthy
run_case
[ "${result}" = 0 ] || fail 'a bootstrapped Crossview must pass'
[ "$(reads)" = 1 ] || fail 'one healthy read must end the run'
[ ! -e "${case_dir}/sleeps" ] || fail 'a healthy run must not wait'
no_alert 'a bootstrapped Crossview'
grep -q 'can read its users' "${case_dir}/output.log" || fail 'a healthy verdict must be explicit'
pass 'a bootstrapped Crossview is healthy after one read'

new_case schema-missing
answer default 200 "${broken_body}"
run_case
[ "${result}" = 0 ] || fail 'a delivered alert must succeed'
[ "$(reads)" = "${samples}" ] || fail 'every sample must be read before alerting'
expected_sleeps=''
for ((i = 1; i < samples; i++)); do expected_sleeps="${expected_sleeps}${interval} "; done
[ "$(tr '\n' ' ' <"${case_dir}/sleeps")" = "${expected_sleeps}" ] ||
  fail 'samples must be spaced by SAMPLE_INTERVAL_SECONDS'
for peer in 0 1; do
  jq -e --arg observed '{"authMode":"session","hasAdmin":false,"hasUsers":false}' '
    length == 1 and
    .[0].labels == {alertname: "CrossviewLoginBroken", severity: "warning", namespace: "crossview"} and
    (.[0].annotations.description | contains($observed)) and
    (.[0].annotations.runbook | contains("crossview-postgres") and contains("platform.devantler.tech/db-bootstrap")) and
    (.[0].endsAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
  ' "${case_dir}/alerts-${peer}.json" >/dev/null || fail "alertmanager-${peer} must receive the schema alert"
done
pass 'the outage answer on every read alerts both Alertmanager peers'

new_case transient
answer 1 200 "${broken_body}"
answer default 200 "${healthy_body}"
run_case
[ "${result}" = 0 ] || fail 'a database blip under a healthy app must pass'
[ "$(reads)" = 2 ] || fail 'the first healthy read must end the run'
no_alert 'a single false read'
pass 'a single read without users does not page'

# The users table is readable even when the break-glass admin was removed, so
# sign-in works and nothing pages.
new_case admin-removed
answer default 200 '{"authMode":"session","authenticated":false,"hasAdmin":false,"hasUsers":true}'
run_case
[ "${result}" = 0 ] || fail 'readable users without the admin must pass'
[ "$(reads)" = 1 ] || fail 'a readable users table must end the run'
no_alert 'a removed admin with readable users'
pass 'a removed admin alone does not page'

# No answer is never health and never a confirmed outage: the Job fails, and the
# CronJob failure detector reports a sensor that keeps failing.
for problem in refused unavailable no-field no-users-field string-field not-json mixed; do
  new_case "${problem}"
  case "${problem}" in
    refused) answer default refused ;;
    unavailable) answer default 503 '{"error":"unavailable"}' ;;
    no-field) answer default 200 '{"authMode":"session","authenticated":false}' ;;
    no-users-field) answer default 200 '{"authMode":"session","authenticated":false,"hasAdmin":false}' ;;
    string-field) answer default 200 '{"authMode":"session","hasAdmin":"true","hasUsers":true}' ;;
    not-json) answer default 200 '<html>sign in</html>' ;;
    mixed)
      answer 1 200 "${broken_body}"
      answer 2 refused
      answer default 200 "${broken_body}" ;;
  esac
  run_case
  [ "${result}" != 0 ] || fail "${problem} must fail the Job instead of reporting a verdict"
  [ "$(reads)" = "${samples}" ] || fail "${problem} must still take every sample"
  no_alert "${problem}"
  ! grep -q 'can read its users' "${case_dir}/output.log" || fail "${problem} reported health"
done
pass 'refusals, error statuses and unrecognised bodies fail the Job without a verdict'

new_case one-peer-down
answer default 200 "${broken_body}"
touch "${case_dir}/reject-0"
run_case
[ "${result}" = 0 ] || fail 'one accepting Alertmanager peer is a delivered alert'
new_case both-peers-down
answer default 200 "${broken_body}"
touch "${case_dir}/reject-0" "${case_dir}/reject-1"
run_case
[ "${result}" != 0 ] || fail 'an alert no peer accepted must fail the Job'
pass 'delivery fails only when no Alertmanager peer accepts the alert'

for bad in 0 10 '' x; do
  new_case "bad-samples-${bad:-empty}"
  SAMPLES_OVERRIDE="${bad}" run_case
  [ "${result}" != 0 ] || fail "SAMPLES='${bad}' must be refused"
  [ "$(reads)" = 0 ] || fail "SAMPLES='${bad}' must be refused before any read"
done
pass 'an invalid sample count is refused before any read'

printf 'PASS: the Crossview login sensor alerts on a schemaless database and only then\n'
