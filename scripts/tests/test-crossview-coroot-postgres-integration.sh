#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production apps overlay'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered integration'

rendered="$(kubectl kustomize "${root_dir}/k8s/providers/hetzner/apps")" ||
  fail 'the production apps overlay must render successfully'

snapshot="$(
  printf '%s\n' "${rendered}" |
    yq ea -o=json -I=0 '[.] | {
      "helmRelease": map(select(
        (.kind == "HelmRelease") and
        (.metadata.name == "crossview") and
        (.metadata.namespace == "crossview")
      ))[0],
      "password": map(select(
        (.kind == "Password") and
        (.metadata.name == "crossview-postgres-coroot-monitor") and
        (.metadata.namespace == "crossview")
      ))[0],
      "externalSecret": map(select(
        (.kind == "ExternalSecret") and
        (.metadata.name == "crossview-postgres-coroot-monitor") and
        (.metadata.namespace == "crossview")
      ))[0],
      "networkPolicy": map(select(
        (.kind == "CiliumNetworkPolicy") and
        (.metadata.name == "allow-coroot-postgres-scrape") and
        (.metadata.namespace == "crossview")
      ))[0],
      "configMap": map(select(
        (.kind == "ConfigMap") and
        (.metadata.name == "crossview-postgres-coroot-monitor-init") and
        (.metadata.namespace == "crossview")
      ))[0]
    }' -
)" || fail 'the rendered Crossview integration resources must be inspectable'
readonly snapshot

printf '%s\n' "${snapshot}" |
  yq e -e '
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape" == "true") and
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape-port" == "5432") and
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape-credentials-secret-name" == "crossview-postgres-coroot-monitor") and
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape-credentials-secret-username-key" == "username") and
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape-credentials-secret-password-key" == "password") and
    (.helmRelease.spec.values.database.podAnnotations."coroot.com/postgres-scrape-param-sslmode" == "disable") and
    ([
      (.helmRelease.spec.values.database.extraEnv // [])[] |
      select(
        (.name == "COROOT_MONITOR_PASSWORD") and
        (.valueFrom.secretKeyRef.name == "crossview-postgres-coroot-monitor") and
        (.valueFrom.secretKeyRef.key == "password")
      )
    ] | length == 1)
  ' - >/dev/null ||
  fail 'the rendered Crossview HelmRelease must configure Coroot discovery with its dedicated credential'

postgres_patch="$(
  printf '%s\n' "${snapshot}" |
    yq e -r '
      .helmRelease.spec.postRenderers[].kustomize.patches[] |
      select((.target.kind == "Deployment") and (.target.name == "crossview-postgres")) |
      select(.patch | contains("shared_preload_libraries")) |
      .patch
    ' -
)" || fail 'the rendered Crossview PostgreSQL post-render patch must be inspectable'

printf '%s\n' "${postgres_patch}" |
  yq e -e '
    (.spec.strategy.type == "Recreate") and
    ([
      .spec.template.spec.containers[] |
      select(
        (.name == "postgres") and
        (.args | length == 4) and
        (.args[0] == "-c") and
        (.args[1] == "shared_preload_libraries=pg_stat_statements") and
        (.args[2] == "-c") and
        (.args[3] == "track_io_timing=on")
      ) |
      .volumeMounts[] |
      select(
        (.name == "coroot-monitor-init") and
        (.mountPath == "/docker-entrypoint-initdb.d") and
        (.readOnly == true)
      )
    ] | length == 1) and
    ([
      .spec.template.spec.volumes[] |
      select(
        (.name == "coroot-monitor-init") and
        (.configMap.name == "crossview-postgres-coroot-monitor-init") and
        (.configMap.defaultMode == 365)
      )
    ] | length == 1)
  ' - >/dev/null ||
  fail 'the PostgreSQL post-render patch must preload statistics, enable I/O timing, and mount an executable init hook'

app_patch="$(
  printf '%s\n' "${snapshot}" |
    yq e -r '
      .helmRelease.spec.postRenderers[].kustomize.patches[] |
      select((.target.kind == "Deployment") and (.target.name == "crossview")) |
      select(.patch | contains("pg_stat_statements")) |
      .patch
    ' -
)" || fail 'the rendered Crossview rollout-safety patch must be inspectable'

printf '%s\n' "${app_patch}" |
  yq e -e '
    ([
      .spec.template.spec.initContainers[] |
      select(.name == "wait-for-db") |
      .args[] |
      select(
        contains("pg_stat_statements") and
        contains("-d postgres") and
        contains("FROM pg_catalog.pg_roles AS monitor") and
        contains("monitor.rolcanlogin") and
        contains("pg_catalog.pg_auth_members AS membership") and
        contains("granted_role.rolname =") and
        contains("pg_monitor")
      )
    ] | length == 1)
  ' - >/dev/null ||
  fail 'the Crossview app must verify the extension, login role, and pg_monitor membership before bootstrapping'

printf '%s\n' "${snapshot}" |
  yq e -e '
    (.password.spec.length == 32) and (.password.spec.symbols == 0)
  ' - >/dev/null ||
  fail 'the production overlay must generate a stable Coroot monitoring password'

printf '%s\n' "${snapshot}" |
  yq e -e '
    (.externalSecret.spec.refreshInterval == "0") and
    (.externalSecret.spec.target.name == "crossview-postgres-coroot-monitor") and
    (.externalSecret.spec.target.template.type == "kubernetes.io/basic-auth") and
    (.externalSecret.spec.target.template.data.username == "coroot_monitor") and
    (.externalSecret.spec.target.template.data.password == "{{ .password }}")
  ' - >/dev/null ||
  fail 'the generated credential must be exposed as a create-once basic-auth Secret'

printf '%s\n' "${snapshot}" |
  yq e -e '
    (.networkPolicy.spec.endpointSelector.matchLabels."app.kubernetes.io/component" == "postgres") and
    ([
      .networkPolicy.spec.ingress[] |
      select(
        ([
          .fromEndpoints[].matchLabels |
          select(
            (."k8s:io.kubernetes.pod.namespace" == "observability") and
            (."app.kubernetes.io/managed-by" == "coroot-operator") and
            (."app.kubernetes.io/part-of" == "coroot") and
            (."app.kubernetes.io/component" == "coroot-cluster-agent") and
            (."io.cilium.k8s.policy.serviceaccount" == "coroot-cluster-agent") and
            ((. | length) == 5)
          )
        ] | length == 1) and
        ([
          .toPorts[].ports[] |
          select((.port == "5432") and (.protocol == "TCP"))
        ] | length == 1)
      )
    ] | length == 1)
  ' - >/dev/null ||
  fail 'the scrape policy must allow only the Coroot cluster-agent to reach the Crossview PostgreSQL pod'

init_script="$(
  printf '%s\n' "${snapshot}" |
    yq e -r '.configMap.data."init-coroot-monitor.sh"' -
)" || fail 'the rendered PostgreSQL init hook must be inspectable'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT

printf '%s\n' "${init_script}" >"${tmp_dir}/init-coroot-monitor.sh"
cat >"${tmp_dir}/psql" <<'FAKE_PSQL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >"${CAPTURE_ARGS}"
printf '%s\n' "${COROOT_MONITOR_PASSWORD}" >"${CAPTURE_PASSWORD}"
cat >"${CAPTURE_STDIN}"
FAKE_PSQL
chmod +x "${tmp_dir}/psql"

PATH="${tmp_dir}:${PATH}" \
  CAPTURE_ARGS="${tmp_dir}/args" \
  CAPTURE_PASSWORD="${tmp_dir}/password" \
  CAPTURE_STDIN="${tmp_dir}/stdin" \
  POSTGRES_USER=postgres \
  POSTGRES_DB=crossview \
  COROOT_MONITOR_PASSWORD=test-password \
  bash "${tmp_dir}/init-coroot-monitor.sh" ||
  fail 'the rendered PostgreSQL init hook must execute successfully'

grep -q -- '--username postgres --dbname postgres' "${tmp_dir}/args" ||
  fail 'the init hook must create pg_stat_statements in the maintenance database Coroot connects to'
if grep -q -- 'test-password' "${tmp_dir}/args"; then
  fail 'the init hook must not expose the generated credential in process arguments'
fi
grep -qx -- 'test-password' "${tmp_dir}/password" ||
  fail 'the init hook must pass the generated credential to psql through its environment'
grep -Fq -- '\getenv coroot_monitor_password COROOT_MONITOR_PASSWORD' "${tmp_dir}/stdin" ||
  fail 'the init hook must import the generated credential inside psql'
grep -q -- 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements' "${tmp_dir}/stdin" ||
  fail 'the init hook must create pg_stat_statements'
grep -q -- 'CREATE ROLE coroot_monitor' "${tmp_dir}/stdin" ||
  fail 'the init hook must create the dedicated monitoring role'
grep -q -- 'GRANT pg_monitor TO coroot_monitor' "${tmp_dir}/stdin" ||
  fail 'the init hook must grant only the PostgreSQL monitoring role'

printf 'PASS: Crossview PostgreSQL renders a least-privilege Coroot integration with rollout safety\n'
