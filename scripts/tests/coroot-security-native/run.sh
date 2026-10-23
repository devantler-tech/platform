#!/usr/bin/env bash
# Hosted-only real binary/API/browser integration. All listeners bind loopback;
# every sample, database, container and screenshot belongs to this invocation.
set -Eeuo pipefail
trap 'echo "Native Coroot integration failed at line ${LINENO}" >&2' ERR
[ "${CI:-}" = true ] || {
  echo 'Run this browser integration only in hosted CI.' >&2
  exit 2
}
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
component="${root}/k8s/bases/infrastructure/coroot/components/security-progress"
artifacts="${NATIVE_ARTIFACT_DIR:?NATIVE_ARTIFACT_DIR is required}"
mkdir -p "${artifacts}"
artifacts="$(cd "${artifacts}" && pwd)"
tmp="$(mktemp -d)"
chmod 755 "${tmp}"
prefix="coroot-native-$$"
containers=("${prefix}-coroot" "${prefix}-prometheus" "${prefix}-metrics")
cleanup() {
  result=$?
  trap - EXIT
  for name in "${containers[@]}"; do
    docker logs "${name}" >"${artifacts}/${name##"${prefix}"-}.log" 2>&1 || true
    docker rm -f "${name}" >/dev/null 2>&1 || true
  done
  rm -rf "${tmp}"
  exit "${result}"
}
trap cleanup EXIT

: "${COROOT_IMAGE:?}" "${PROMETHEUS_IMAGE:?}" "${SERVER_IMAGE:?}" "${PLAYWRIGHT_VERSION:?}"
base=http://127.0.0.1:18080
prom=http://127.0.0.1:19090
printf '%s\n' 'SYNTHETIC INTEGRATION EVIDENCE ONLY: no production resources, credentials or health claims.' >"${artifacts}/README.txt"
# Verified against coroot/coroot v1.22.2 Dockerfile, config/flags.go,
# config/bootstrap.go, main.go and front/src/main.js. Do not guess API routes.
for image in "${COROOT_IMAGE}" "${PROMETHEUS_IMAGE}" "${SERVER_IMAGE}"; do
  docker pull "${image}"
done
docker image inspect "${COROOT_IMAGE}" --format '{{json .Config.Entrypoint}}' |
  jq -e '. == ["/usr/bin/coroot"]' >/dev/null
docker image inspect "${COROOT_IMAGE}" "${PROMETHEUS_IMAGE}" "${SERVER_IMAGE}" \
  --format '{{json .RepoDigests}}' >"${artifacts}/image-digests.jsonl"

mkdir "${tmp}/metrics"
sed "s/@SNAPSHOT_TIMESTAMP@/$(date +%s)/g" \
  "${root}/scripts/tests/coroot-security-native/synthetic.prom" >"${tmp}/metrics/metrics"
cat >"${tmp}/prometheus.yml" <<'YAML'
global:
  scrape_interval: 5s
  scrape_timeout: 2s
scrape_configs:
  - job_name: synthetic-security-fixture
    static_configs:
      - targets: [127.0.0.1:18081]
YAML
chmod 755 "${tmp}/metrics"
chmod 644 "${tmp}/metrics/metrics" "${tmp}/prometheus.yml"
docker run --detach --name "${prefix}-metrics" --network host --read-only --user 65532:65532 \
  --volume "${tmp}/metrics:/metrics:ro" "${SERVER_IMAGE}" \
  httpd -f -p 127.0.0.1:18081 -h /metrics >/dev/null
docker run --detach --name "${prefix}-prometheus" --network host --read-only --user 65532:65532 \
  --tmpfs /prometheus:rw,uid=65532,gid=65532,mode=700 \
  --volume "${tmp}/prometheus.yml:/etc/prometheus/native.yml:ro" "${PROMETHEUS_IMAGE}" \
  --config.file=/etc/prometheus/native.yml --storage.tsdb.path=/prometheus \
  --web.listen-address=127.0.0.1:19090 >/dev/null
docker run --detach --name "${prefix}-coroot" --network host --read-only \
  --tmpfs /data:rw,uid=65534,gid=65534,mode=700 --tmpfs /tmp:rw,uid=65534,gid=65534,mode=700 \
  "${COROOT_IMAGE}" --listen=127.0.0.1:18080 --grpc-disabled --data-dir=/data \
  --global-prometheus-url="${prom}" --global-refresh-interval=5s \
  --auth-anonymous-role=Admin --disable-usage-statistics --do-not-check-for-updates >/dev/null

wait_json() {
  local url=$1 expression=$2 output=$3
  for ((attempt = 0; attempt < 90; attempt++)); do
    if curl --silent --fail --connect-timeout 2 --max-time 5 "${url}" >"${output}" &&
      jq -e "${expression}" "${output}" >/dev/null; then
      return
    fi
    sleep 1
  done
  echo "Timed out waiting for native fixture: ${url}" >&2
  return 1
}
wait_native_panel() {
  local panel=$1 output=$2
  # prom/http.go truncates range endpoints to the refresh step. Instant-query
  # readiness can precede the first labeled sample at that truncated endpoint.
  # Wait for the actual Coroot chart, preserving the same strict assertion used
  # below, rather than assuming a fixed startup delay is sufficient.
  for ((attempt = 0; attempt < 30; attempt++)); do
    if curl --silent --fail --connect-timeout 2 --max-time 5 --get \
      --data-urlencode "query=${panel}" --data-urlencode 'from=now-5m' --data-urlencode 'to=now' \
      "${base}/api/project/${pid}/panel/data" >"${output}" &&
      jq -e '.chart.series | type == "array" and any(.[]?; ([.data[]? | select(type == "number")] | length) >= 2)' \
        "${output}" >/dev/null; then
      return
    fi
    sleep 1
  done
  echo 'Timed out waiting for actual Coroot range-query fixture series' >&2
  return 1
}
wait_json "${prom}/api/v1/query?query=platform_security_policy_total" \
  '.status == "success" and (.data.result | length) == 5' "${tmp}/prom-ready.json"
wait_json "${base}/api/user" \
  '.anonymous == true and .role == "Admin" and (.projects | length) == 1 and .projects[0].name == "default"' \
  "${artifacts}/user.json"
# v1.22.2 has no --version option. main.go logs the compiled version before
# loading configuration; assert that real startup line after API readiness.
docker logs "${prefix}-coroot" 2>&1 | grep -E 'version: 1\.22\.2([[:space:]]|$)' \
  >"${artifacts}/coroot-version.txt"
# db/project.go PrometheusConfig adds this selector when using the global
# backend. A successful unscoped Prometheus scrape alone cannot prove that the
# project's actual dashboard queries can read the fixture.
project_id="$(jq -er '.projects[0].id | select(test("^[A-Za-z0-9_-]+$"))' "${artifacts}/user.json")"
sed "s/@SNAPSHOT_TIMESTAMP@/$(date +%s)/g" \
  "${root}/scripts/tests/coroot-security-native/synthetic.prom" |
  COROOT_URL="${base}" COROOT_PROJECT=default sh "${component}/bind-project-metrics.sh" \
    >"${tmp}/metrics/next"
chmod 644 "${tmp}/metrics/next"
mv "${tmp}/metrics/next" "${tmp}/metrics/metrics"
wait_json "${prom}/api/v1/query?query=platform_security_policy_total%7Bcoroot_project_id%3D%22${project_id}%22%7D" \
  '.status == "success" and (.data.result | length) == 5' "${artifacts}/prom-project-ready.json"
pid="$(jq -r '.projects[0].id | @uri' "${artifacts}/user.json")"
collection="${base}/api/project/${pid}/dashboards"
wait_json "${collection}" '.data | type == "array"' "${tmp}/list-ready.json"
wait_native_panel "$(jq -c '.[0].config.groups[0].panels[0]' "${component}/dashboards.json")" \
  "${artifacts}/native-range-ready.json"

code="$(curl --silent --show-error --max-time 30 --output "${tmp}/unmanaged-id" --write-out '%{http_code}' \
  --header 'content-type: application/json' --data '{"action":"create","name":"Synthetic unmanaged dashboard","description":"Synthetic operator-owned state; must survive reconciliation."}' "${collection}")"
[ "${code}" = 201 ]
unmanaged_id="$(jq -sRr 'rtrimstr("\n") | @uri' "${tmp}/unmanaged-id")"
curl --silent --show-error --fail --max-time 30 --header 'content-type: application/json' \
  --data '{"name":"Synthetic unmanaged dashboard","config":{"groups":[]}}' "${collection}/${unmanaged_id}" >/dev/null
curl --silent --show-error --fail --max-time 30 "${collection}/${unmanaged_id}" >"${tmp}/unmanaged-before.json"

for pass in 1 2; do
  COROOT_URL="${base}" COROOT_PROJECT=default DASHBOARDS_FILE="${component}/dashboards.json" \
    sh "${component}/reconcile.sh" >"${artifacts}/reconcile-${pass}.log" 2>&1
  curl --silent --show-error --fail --max-time 30 "${collection}" >"${artifacts}/dashboards-${pass}.json"
  jq -e --slurpfile desired "${component}/dashboards.json" '
    .data as $live | ($live | length) == 4 and all($desired[0][];
      . as $wanted | any($live[]; .name == $wanted.name and .description == $wanted.description))
  ' "${artifacts}/dashboards-${pass}.json" >/dev/null
done
jq -S '.data | sort_by(.id)' "${artifacts}/dashboards-1.json" >"${tmp}/first.json"
jq -S '.data | sort_by(.id)' "${artifacts}/dashboards-2.json" >"${tmp}/second.json"
cmp "${tmp}/first.json" "${tmp}/second.json"
curl --silent --show-error --fail --max-time 30 "${collection}/${unmanaged_id}" >"${tmp}/unmanaged-after.json"
jq -e --slurpfile before "${tmp}/unmanaged-before.json" '.data == $before[0].data' "${tmp}/unmanaged-after.json" >/dev/null

# Test every actual configured panel through Coroot, not direct PromQL alone.
# The two 1-day net-change panels legitimately have no history in this short
# fixture. All other panels must return real chart series, including unknowns.
dashboard_index=0
while IFS= read -r definition; do
  dashboard_index=$((dashboard_index + 1))
  name="$(jq -r '.name' <<<"${definition}")"
  id="$(jq -r --arg name "${name}" '.data[] | select(.name == $name) | .id | @uri' "${artifacts}/dashboards-2.json")"
  curl --silent --show-error --fail --max-time 30 "${collection}/${id}" >"${artifacts}/dashboard-${dashboard_index}.json"
  jq -e --argjson desired "${definition}" '.data.name == $desired.name and .data.config == $desired.config' \
    "${artifacts}/dashboard-${dashboard_index}.json" >/dev/null
  panel_index=0
  while IFS= read -r panel; do
    panel_index=$((panel_index + 1))
    printf 'Checking native dashboard %s panel %s: %s\n' \
      "${dashboard_index}" "${panel_index}" "$(jq -r '.name' <<<"${panel}")"
    output="${artifacts}/panel-${dashboard_index}-${panel_index}.json"
    curl --silent --show-error --fail --max-time 30 --get --data-urlencode "query=${panel}" \
      --data-urlencode 'from=now-5m' --data-urlencode 'to=now' \
      "${base}/api/project/${pid}/panel/data" >"${output}"
    if jq -e '.name | startswith("Daily net change")' <<<"${panel}" >/dev/null; then
      jq -e 'type == "object"' "${output}" >/dev/null
    else
      jq -e '.chart.series | type == "array" and length > 0' "${output}" >/dev/null
    fi
  done < <(jq -c '.config.groups[].panels[]' <<<"${definition}")
  printf 'Native API dashboard %s: %s; validated panels=%s\n' \
    "${dashboard_index}" "${name}" "${panel_index}" >>"${artifacts}/README.txt"
done < <(jq -c '.[]' "${component}/dashboards.json")

# Complete every configured native API check before any browser work, so a
# frontend bootstrap failure cannot hide evidence from later dashboards.
dashboard_index=0
while IFS= read -r definition; do
  dashboard_index=$((dashboard_index + 1))
  name="$(jq -r '.name' <<<"${definition}")"
  id="$(jq -r --arg name "${name}" '.data[] | select(.name == $name) | .id | @uri' "${artifacts}/dashboards-2.json")"
  panel_count="$(jq '[.config.groups[].panels[]] | length' <<<"${definition}")"
  screenshot="${artifacts}/dashboard-${dashboard_index}.png"
  har="${artifacts}/dashboard-${dashboard_index}.har"
  # A selector timeout exits the CLI before its HAR is flushed. Preserve one
  # actual rendered page and its full network evidence first, so a frontend
  # failure can be diagnosed without weakening the canvas requirement below.
  if [ "${dashboard_index}" = 1 ]; then
    DEBUG=pw:protocol npx --yes "playwright@${PLAYWRIGHT_VERSION}" screenshot --browser chromium \
      --lang en-US --viewport-size '1600,1200' --full-page --timeout 30000 --wait-for-timeout 10000 \
      --save-har "${artifacts}/dashboard-1-diagnostic.har" \
      "${base}/p/${pid}/dashboards/${id}?from=now-5m&to=now" \
      "${artifacts}/dashboard-1-diagnostic.png" 2>"${artifacts}/dashboard-1-protocol.log"
  fi
  # CLI only: no authored browser JavaScript and no local desktop automation.
  # The hosted runner's en-US@posix default is not a valid Intl language tag.
  npx --yes "playwright@${PLAYWRIGHT_VERSION}" screenshot --browser chromium \
    --lang en-US --viewport-size '1600,1200' --full-page --timeout 60000 \
    --wait-for-selector '.dashboard .grid-stack-item canvas' --wait-for-timeout 5000 \
    --save-har "${har}" --save-har-glob '**/api/project/**' \
    "${base}/p/${pid}/dashboards/${id}?from=now-5m&to=now" "${screenshot}"
  [ -s "${screenshot}" ]
  # HAR validates the requests made by the actual page. Screenshot pixels still
  # need human inspection; a successful CLI exit alone is not visual acceptance.
  jq -e --argjson expected "${panel_count}" '
    [.log.entries[] | select(.request.url | contains("/panel/data"))] as $panels
    | ($panels | length) >= $expected and all($panels[]; .response.status == 200)
  ' "${har}" >/dev/null
  printf 'Dashboard %s: %s; native panels=%s; screenshot=%s\n' \
    "${dashboard_index}" "${name}" "${panel_count}" "${screenshot##*/}" >>"${artifacts}/README.txt"
done < <(jq -c '.[]' "${component}/dashboards.json")
printf '%s\n' 'Native API persistence and browser capture completed. One-day trend panels intentionally lack fixture history; inspect screenshots before accepting UI quality.' >>"${artifacts}/README.txt"
