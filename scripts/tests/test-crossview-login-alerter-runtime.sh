#!/usr/bin/env bash

# Reproduce the #3315 outage against the real pinned Crossview app and its
# bundled PostgreSQL, and run the deployed login sensor, in its own pinned
# image, against it:
#
#   1. a freshly bootstrapped app: the sensor reports healthy;
#   2. the database replaced by an empty one under the running app, which is the
#      August 2026 trigger (an emptyDir lost with its pod): `/api/health` still
#      answers 200 while the schema is gone, and the sensor alerts;
#   3. the app restarted: it re-runs its schema bootstrap and the sensor is
#      healthy again, so restarting is what restores login;
#   4. the database stopped: the sensor never reports health;
#   5. the same database started again with its data: the app recovers without a
#      restart, which is why the runbook checks the database before rolling.
#
# Images, configuration and the Service contract come from rendering the pinned
# chart with the production values, so a chart bump re-proves the sensor against
# the new app. The HelmRelease post-renderers are not applied: they harden the
# containers and gate the app's init container on markers that the database's
# init hook writes into any fresh data directory, which
# test-crossview-postgres-durable-storage.sh and
# test-crossview-coroot-postgres-integration.sh pin. What is reproduced here is
# the app's own bootstrap, which is what the outage and its fix turn on. Every
# container runs on an internal Docker network with no route out; only the image
# pulls use the network. Requires docker.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-crossview-login-alerter.yaml"
readonly app_policy="${root_dir}/k8s/providers/hetzner/apps/crossview/cilium-network-policy-login-alerter.yaml"
work_dir="$(mktemp -d)"
readonly work_dir
readonly suffix="$$"
readonly net="crossview-login-${suffix}"
readonly db="crossview-db-${suffix}"
readonly app="crossview-app-${suffix}"
readonly probe="crossview-probe-${suffix}"

# shellcheck disable=SC2317,SC2329 # Invoked by EXIT.
cleanup() {
  docker rm -f "${app}" "${db}" "${probe}" >/dev/null 2>&1 || true
  docker network rm "${net}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  if docker inspect "${app}" >/dev/null 2>&1; then
    printf -- '--- crossview app log (last 40 lines)\n' >&2
    docker logs --tail 40 "${app}" >&2 2>&1 || true
  fi
  exit 1
}
pass() { printf 'ok: %s\n' "$1"; }

for tool in docker helm jq kubectl yq; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

# ---- Render the pinned chart with the production values ----------------------
kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps" | yq ea -o=json '[.]' |
  jq -e '.[] | select(.kind == "HelmRelease" and .metadata.namespace == "crossview"
    and .metadata.name == "crossview")' >"${work_dir}/release.json" ||
  fail 'the production apps overlay must carry the Crossview HelmRelease'
helm pull "$(jq -r '.spec.chart.spec.chart' "${work_dir}/release.json")" \
  --repo "$(yq -r '.spec.url' "${root_dir}/k8s/bases/apps/crossview/helm-repository.yaml")" \
  --version "$(jq -r '.spec.chart.spec.version' "${work_dir}/release.json")" \
  --destination "${work_dir}" >/dev/null
# Resolve the numeric Flux substitutions Helm must consume, as the durable
# storage test does; string substitutions stay literal and are harmless here.
yq -o=json '.data' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml" >"${work_dir}/variables.json"
jq --slurpfile variables "${work_dir}/variables.json" '.spec.values | walk(
  if type == "string" and test("^\\$\\{[a-z_]+:=[0-9]+\\}$") then
    capture("^\\$\\{(?<name>[a-z_]+):=(?<fallback>[0-9]+)\\}$") |
    ($variables[0][.name] // .fallback) | tonumber
  else . end)' "${work_dir}/release.json" >"${work_dir}/values.json"
if ! helm template crossview "${work_dir}"/crossview-*.tgz --namespace crossview \
  --values "${work_dir}/values.json" >"${work_dir}/chart.yaml" 2>"${work_dir}/helm.err"; then
  cat "${work_dir}/helm.err" >&2
  fail 'the pinned chart must render with the production values'
fi
yq ea -o=json '[.]' "${work_dir}/chart.yaml" >"${work_dir}/chart.json"

# A missing piece of the chart contract names itself instead of ending the step
# silently through set -e.
pick() { jq -e "$1" "${work_dir}/chart.json" || fail "the rendered chart has no match for: $1"; }
app_container="$(pick '[.[] | select(.kind == "Deployment" and .metadata.name == "crossview")
  | .spec.template.spec.containers[] | select(.name == "crossview")] | first')"
db_container="$(pick '[.[] | select(.kind == "Deployment" and .metadata.name == "crossview-postgres")
  | .spec.template.spec.containers[] | select(.name == "postgres")] | first')"
config="$(pick '[.[] | select(.kind == "ConfigMap" and .metadata.name == "crossview-config") | .data] | first')"
service="$(pick '[.[] | select(.kind == "Service" and .metadata.name == "crossview-service")] | first')"
app_labels="$(pick '[.[] | select(.kind == "Deployment" and .metadata.name == "crossview")
  | .spec.template.metadata.labels] | first')"
app_image="$(jq -r '.image' <<<"${app_container}")"
db_image="$(jq -r '.image' <<<"${db_container}")"
sensor_image="$(yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "alerter") | .image' "${manifest}")"

# ---- The sensor's address is the chart's Service, and the policy admits it ---
service_port="$(jq -r '.spec.ports[] | select(.name == "http") | .port' <<<"${service}")"
target_port="$(jq -r '.spec.ports[] | select(.name == "http") | .targetPort' <<<"${service}")"
container_port="$(jq -r '.ports[] | select(.name == "http") | .containerPort' <<<"${app_container}")"
if [ "${service_port}" != 80 ] || [ "${target_port}" != "${container_port}" ]; then
  fail "the sensor reads crossview-service:80, but the chart serves ${service_port} -> ${target_port}"
fi
[ "$(yq -r '.spec.ingress[0].toPorts[0].ports[0].port' "${app_policy}")" = "${container_port}" ] ||
  fail "the network policy must admit the sensor to the app's container port ${container_port}"
subset_of_app_labels() {
  jq -e --argjson labels "${app_labels}" 'to_entries | length > 0 and all(. as $e | $labels[$e.key] == $e.value)'
}
yq -o=json '.spec.endpointSelector.matchLabels' "${app_policy}" | subset_of_app_labels >/dev/null ||
  fail 'the network policy must select the rendered app pods'
jq '.spec.selector' <<<"${service}" | subset_of_app_labels >/dev/null ||
  fail 'crossview-service must route to the rendered app pods'
pass "the sensor reads the chart's Service and the policy admits it to port ${container_port}"

# ---- Configuration exactly as the chart wires it -----------------------------
db_password="db-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
admin_password="admin-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
session_secret="session-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
admin_username="$(pick '[.[] | select(.kind == "Secret" and .metadata.name == "crossview-secrets")
  | (.stringData.adminusername // (.data.adminusername | @base64d))] | first' | jq -r .)"
[ -n "${admin_username}" ] || fail 'the chart must name the break-glass admin'
jq -r --argjson config "${config}" --arg db "${db_password}" --arg admin "${admin_password}" \
  --arg session "${session_secret}" --arg user "${admin_username}" '
  .env[] |
  if .value != null then "\(.name)=\(.value)"
  elif .valueFrom.configMapKeyRef != null then "\(.name)=\($config[.valueFrom.configMapKeyRef.key] // "")"
  elif .valueFrom.secretKeyRef.key == "db-password" then "\(.name)=\($db)"
  elif .valueFrom.secretKeyRef.key == "admin-password" then "\(.name)=\($admin)"
  elif .valueFrom.secretKeyRef.key == "session-secret" then "\(.name)=\($session)"
  elif .valueFrom.secretKeyRef.key == "adminusername" then "\(.name)=\($user)"
  elif .valueFrom.secretKeyRef.key == "oidc-client-secret" then "\(.name)=unused-in-this-test"
  else error("unmapped env \(.name)") end' <<<"${app_container}" >"${work_dir}/app.env"
db_name="$(jq -r '.DB_NAME' <<<"${config}")"
db_user="$(jq -r '.DB_USER' <<<"${config}")"
db_host="$(jq -r '.DB_HOST' <<<"${config}")"
jq -r --arg db "${db_password}" '.env[] |
  if .name == "POSTGRES_PASSWORD" then "\(.name)=\($db)"
  elif .value != null then "\(.name)=\(.value)"
  else empty end' <<<"${db_container}" >"${work_dir}/db.env"
grep -qx "POSTGRES_DB=${db_name}" "${work_dir}/db.env" ||
  fail 'the bundled database must create the database the app connects to'

# ---- The sensor, unchanged except for the Service port and Alertmanager -----
mkdir -p "${work_dir}/sensor/bin" "${work_dir}/capture"
chmod 0777 "${work_dir}/capture"
yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "alerter") | .command[2]' "${manifest}" |
  sed 's#crossview-service.crossview.svc.cluster.local.:80/#crossview-service.crossview.svc.cluster.local.:'"${container_port}"'/#' \
    >"${work_dir}/sensor/sensor.sh"
grep -Fq "crossview-service.crossview.svc.cluster.local.:${container_port}/api/auth/check" "${work_dir}/sensor/sensor.sh" ||
  fail 'could not point the sensor at the container port'
real_curl="$(docker run --rm --entrypoint /bin/sh "${sensor_image}" -c 'command -v curl')"
[ -n "${real_curl}" ] || fail 'the sensor image must provide curl'
# Alertmanager is not part of this reproduction: capture what the sensor posts.
cat >"${work_dir}/sensor/bin/curl" <<STUB
#!/bin/sh
for arg in "\$@"; do
  case "\$arg" in
    http://alertmanager-*/api/v2/alerts)
      payload=''
      while [ "\$#" -gt 0 ]; do
        case "\$1" in -d) payload="\$2"; shift 2 ;; *) shift ;; esac
      done
      peer="\${arg#http://alertmanager-}"
      printf '%s\n' "\$payload" >"/capture/alerts-\${peer%%.*}.json"
      exit 0 ;;
  esac
done
exec ${real_curl} "\$@"
STUB
chmod 0755 "${work_dir}/sensor/bin/curl"
chmod -R a+rX "${work_dir}/sensor"

samples="$(yq -r '.spec.jobTemplate.spec.template.spec.containers[] | select(.name == "alerter")
  | .env[] | select(.name == "SAMPLES") | .value' "${manifest}")"

# Run the sensor the way the CronJob does: same image, user and read-only root.
run_sensor() {
  rm -f "${work_dir}"/capture/alerts-*.json
  sensor_status=0
  docker run --rm --network "${net}" --user 65532:65532 --read-only \
    --tmpfs /tmp:size=1m,mode=1777 --cap-drop ALL --security-opt no-new-privileges \
    -v "${work_dir}/sensor:/sensor:ro" -v "${work_dir}/capture:/capture" \
    -e PATH="/sensor/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
    -e SAMPLES="${samples}" -e SAMPLE_INTERVAL_SECONDS=2 \
    --entrypoint /bin/sh "${sensor_image}" /sensor/sensor.sh \
    >"${work_dir}/sensor.log" 2>&1 || sensor_status=$?
  sed 's/^/    sensor: /' "${work_dir}/sensor.log"
}
alerted() { ls "${work_dir}"/capture/alerts-*.json >/dev/null 2>&1; }

# One request from inside the network, through the sensor image's own curl in a
# long-lived helper container.
app_get() {
  docker exec "${probe}" "${real_curl}" -sS --max-time 5 -o /dev/stdout -w '\n%{http_code}' \
    "http://crossview-service.crossview.svc.cluster.local.:${container_port}$1" 2>/dev/null || true
}
wait_for_app() {
  for _ in $(seq 1 60); do
    [ "$(app_get /api/health | tail -n 1)" = 200 ] && return 0
    sleep 2
  done
  fail 'the Crossview app never answered /api/health'
}
start_db() {
  docker run -d --name "${db}" --network "${net}" --network-alias "${db_host}" \
    --env-file "${work_dir}/db.env" "${db_image}" >/dev/null
  # Only the final server listens on TCP; the image's init-time server does not.
  for _ in $(seq 1 60); do
    db_query 'SELECT 1' >/dev/null 2>&1 && return 0
    sleep 2
  done
  fail 'the bundled PostgreSQL never accepted TCP connections'
}
db_query() {
  docker exec -e PGPASSWORD="${db_password}" "${db}" \
    psql -h 127.0.0.1 -U "${db_user}" -d "${db_name}" -tAc "$1"
}
users_table() { db_query "SELECT to_regclass('public.users') IS NOT NULL"; }

docker network create --internal "${net}" >/dev/null
docker run -d --name "${probe}" --network "${net}" --user 65532:65532 \
  --entrypoint /bin/sh "${sensor_image}" -c 'sleep 3600' >/dev/null
start_db
# As in production: non-root, read-only root, only the chart's /app/logs writable.
docker run -d --name "${app}" --network "${net}" \
  --network-alias crossview-service.crossview.svc.cluster.local \
  --user 1000:1000 --read-only --tmpfs /app/logs:mode=1777 \
  --cap-drop ALL --security-opt no-new-privileges \
  --env-file "${work_dir}/app.env" "${app_image}" >/dev/null
wait_for_app

expect_healthy() {
  run_sensor
  if [ "${sensor_status}" != 0 ] || alerted; then
    fail "$1"
  fi
  grep -q 'can read its users' "${work_dir}/sensor.log" || fail 'the healthy verdict must be explicit'
}

# 1. Bootstrapped.
printf '1. bootstrapped: /api/auth/check -> %s\n' "$(app_get /api/auth/check | head -n 1)"
[ "$(users_table)" = t ] || fail 'the app must create its users table at startup'
expect_healthy 'a freshly bootstrapped Crossview must read as healthy'
pass 'a bootstrapped Crossview reads as healthy'

# 2. The database comes back empty under the running app.
docker rm -f "${db}" >/dev/null
start_db
[ "$(users_table)" = f ] || fail 'the replacement database must start without the schema'
for _ in $(seq 1 30); do
  [ "$(app_get /api/auth/check | tail -n 1)" = 200 ] && break
  sleep 2
done
printf '2. schema gone: /api/health -> %s, /api/auth/check -> %s\n' \
  "$(app_get /api/health | tail -n 1)" "$(app_get /api/auth/check | head -n 1)"
[ "$(app_get /api/health | tail -n 1)" = 200 ] || fail 'the reproduction needs the probe to stay green, as it did in the outage'
run_sensor
[ "${sensor_status}" = 0 ] || fail 'the sensor must deliver its alert'
alerted || fail 'an empty database under a running app must alert'
jq -e 'length == 1 and .[0].labels.alertname == "CrossviewLoginBroken"
  and (.[0].annotations.description | contains("\"hasAdmin\":false"))' \
  "${work_dir}/capture/alerts-0.json" >/dev/null || fail 'the alert must carry the answer that triggered it'
pass 'an empty database alerts while /api/health stays green'

# 3. Restarting the app re-runs its bootstrap.
docker restart "${app}" >/dev/null
wait_for_app
printf '3. restarted: /api/auth/check -> %s\n' "$(app_get /api/auth/check | head -n 1)"
[ "$(users_table)" = t ] || fail 'restarting the app must recreate its users table'
expect_healthy 'a restarted Crossview must read as healthy again'
pass 'restarting the app restores the schema and clears the signal'

# 4. A database the app cannot reach is never health. Sign-in fails here too,
# so either verdict that is not health is a signal: an alert, or a failed Job
# that the CronJob failure detector reports.
docker stop "${db}" >/dev/null
printf '4. database stopped: /api/auth/check -> %s\n' "$(app_get /api/auth/check | tr '\n' ' ')"
run_sensor
if [ "${sensor_status}" = 0 ] && ! alerted; then
  fail 'an unreachable database must not read as healthy'
fi
if grep -q 'can read its users' "${work_dir}/sensor.log"; then
  fail 'an unreachable database reported readable users'
fi
if alerted; then
  printf '   -> the sensor alerted\n'
else
  printf '   -> the sensor failed the Job (exit %s)\n' "${sensor_status}"
fi
pass 'an unreachable database does not read as healthy'

# 5. The database comes back with its data: no restart is needed.
docker start "${db}" >/dev/null
for _ in $(seq 1 60); do
  db_query 'SELECT 1' >/dev/null 2>&1 && break
  sleep 2
done
[ "$(users_table)" = t ] || fail 'the restarted database must still hold the schema'
for _ in $(seq 1 30); do
  app_get /api/auth/check | head -n 1 | grep -q '"hasAdmin":true' && break
  sleep 2
done
printf '5. database back: /api/auth/check -> %s\n' "$(app_get /api/auth/check | head -n 1)"
expect_healthy 'the app must recover on its own once its database is back with its data'
pass 'a database that returns with its data needs no app restart'

printf 'PASS: the Crossview login sensor detects the #3315 outage on the real app\n'
