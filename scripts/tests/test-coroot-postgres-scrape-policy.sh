#!/usr/bin/env bash
# Guards Coroot's Postgres integration on every CloudNativePG database the
# platform runs in production.
#
# Every platform database is monitored in Coroot. One added without the
# integration is invisible there, and the kagent lane's two databases shipped
# without it. So every CNPG Cluster in the rendered production layers must
# carry all of it:
# - the coroot.com/postgres-scrape annotations in spec.inheritedMetadata, which
#   CNPG copies onto the instance pods where Coroot's cluster-agent finds them,
#   naming a credentials Secret;
# - a coroot_monitor login in pg_monitor whose password comes from that Secret;
# - an ExternalSecret that creates that Secret in the database's namespace;
# - pg_stat_statements, for Coroot's per-query view;
# - outside the observability namespace, a CiliumNetworkPolicy that admits only
#   the operator-generated cluster-agent, only to the database's pods, on 5432.
#   Inside it, the base allow-coroot policy already admits the agent.
# Tenant databases live in their tenants' repositories and are not rendered
# here.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly layers=(infrastructure/controllers infrastructure apps)

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production layers'
command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the rendered manifests'

# check_databases <rendered manifests>: prints one line per gap, naming the
# database, and nothing when every database is covered.
#
# yq is not jq: `==` compares scalars only, and both `as $var |` and `|` end at
# the next `and`. So every clause names its full path, and every pipe sits in
# its own parentheses.
check_databases() {
  local rendered="$1" relevant="${workdir}/relevant.yaml" database namespace name secret
  # Keep only the kinds the checks read, so each query below parses a small file.
  yq e 'select(
      (.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster") or
      .kind == "ExternalSecret" or .kind == "CiliumNetworkPolicy")' "${rendered}" >"${relevant}"
  while IFS= read -r database; do
    [[ -n "${database}" ]] || continue
    namespace="${database%%/*}"
    name="${database#*/}"
    NAMESPACE="${namespace}" NAME="${name}" yq e -N 'select(.kind == "Cluster" and
        .metadata.namespace == strenv(NAMESPACE) and .metadata.name == strenv(NAME))' \
      "${relevant}" >"${workdir}/cluster.yaml"

    yq e -e '.spec.inheritedMetadata.annotations["coroot.com/postgres-scrape"] == "true" and
        .spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-port"] == "5432" and
        .spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-credentials-secret-username-key"] == "username" and
        .spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-credentials-secret-password-key"] == "password" and
        .spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-param-sslmode"] == "require" and
        (.spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-credentials-secret-name"] // "") != ""' \
      "${workdir}/cluster.yaml" >/dev/null 2>&1 ||
      printf '%s: the coroot.com/postgres-scrape annotations are missing or incomplete in spec.inheritedMetadata\n' "${database}"
    secret="$(yq e '.spec.inheritedMetadata.annotations["coroot.com/postgres-scrape-credentials-secret-name"] // ""' "${workdir}/cluster.yaml")"

    SECRET="${secret}" yq e -e '([.spec.managed.roles[]? | select(
        .name == "coroot_monitor" and .login == true and
        ((.inRoles // []) | length) == 1 and .inRoles[0] == "pg_monitor" and
        .passwordSecret.name == strenv(SECRET))] | length) == 1' \
      "${workdir}/cluster.yaml" >/dev/null 2>&1 ||
      printf '%s: no coroot_monitor login in pg_monitor whose password comes from the scrape Secret\n' "${database}"

    yq e -e '((.spec.postgresql.parameters // {}) | has("pg_stat_statements.track")) == true' \
      "${workdir}/cluster.yaml" >/dev/null 2>&1 ||
      printf '%s: pg_stat_statements is not enabled\n' "${database}"

    NAMESPACE="${namespace}" SECRET="${secret}" yq ea -e '([select(
        .kind == "ExternalSecret" and .metadata.namespace == strenv(NAMESPACE) and
        (.spec.target.name // .metadata.name) == strenv(SECRET) and
        .spec.target.template.type == "kubernetes.io/basic-auth" and
        .spec.target.template.data.username == "coroot_monitor")] | length) == 1' \
      "${relevant}" >/dev/null 2>&1 ||
      printf '%s: no ExternalSecret creates its scrape Secret for the coroot_monitor login\n' "${database}"

    [[ "${namespace}" == observability ]] && continue
    NAMESPACE="${namespace}" NAME="${name}" yq ea -e '([select(
        .kind == "CiliumNetworkPolicy" and .metadata.namespace == strenv(NAMESPACE) and
        (.spec.endpointSelector | length) == 1 and
        (.spec.endpointSelector.matchLabels | length) == 1 and
        .spec.endpointSelector.matchLabels["cnpg.io/cluster"] == strenv(NAME) and
        (.spec.ingress | length) == 1 and
        (.spec.ingress[0] | length) == 2 and
        (.spec.ingress[0].fromEndpoints | length) == 1 and
        (.spec.ingress[0].fromEndpoints[0] | length) == 1 and
        (.spec.ingress[0].fromEndpoints[0].matchLabels | length) == 4 and
        .spec.ingress[0].fromEndpoints[0].matchLabels["k8s:io.kubernetes.pod.namespace"] == "observability" and
        .spec.ingress[0].fromEndpoints[0].matchLabels["app.kubernetes.io/managed-by"] == "coroot-operator" and
        .spec.ingress[0].fromEndpoints[0].matchLabels["app.kubernetes.io/part-of"] == "coroot" and
        .spec.ingress[0].fromEndpoints[0].matchLabels["app.kubernetes.io/component"] == "coroot-cluster-agent" and
        (.spec.ingress[0].toPorts | length) == 1 and
        (.spec.ingress[0].toPorts[0] | length) == 1 and
        (.spec.ingress[0].toPorts[0].ports | length) == 1 and
        (.spec.ingress[0].toPorts[0].ports[0] | length) == 2 and
        .spec.ingress[0].toPorts[0].ports[0].port == "5432" and
        .spec.ingress[0].toPorts[0].ports[0].protocol == "TCP" and
        ((.spec.egress // []) | length) == 0)] | length) == 1' \
      "${relevant}" >/dev/null 2>&1 ||
      printf '%s: no CiliumNetworkPolicy admits only the Coroot cluster-agent to its pods on 5432\n' "${database}"
  done < <(yq e -N 'select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster") |
      .metadata.namespace + "/" + .metadata.name' "${relevant}")
}

# --- Self-test the checks ----------------------------------------------------
# Checks that report nothing on a database they never examined would pass
# every real database too, so prove them on a fixture first: a covered
# database, one in observability (no policy needed), and two that each miss
# something.
cat >"${workdir}/fixture.yaml" <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: covered-db, namespace: app}
spec:
  inheritedMetadata:
    annotations:
      coroot.com/postgres-scrape: "true"
      coroot.com/postgres-scrape-port: "5432"
      coroot.com/postgres-scrape-credentials-secret-name: covered-db-coroot-monitor
      coroot.com/postgres-scrape-credentials-secret-username-key: username
      coroot.com/postgres-scrape-credentials-secret-password-key: password
      coroot.com/postgres-scrape-param-sslmode: require
  postgresql:
    parameters: {pg_stat_statements.track: all}
  managed:
    roles:
      - {name: coroot_monitor, ensure: present, login: true, inRoles: [pg_monitor], passwordSecret: {name: covered-db-coroot-monitor}}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: covered-db-coroot-monitor, namespace: app}
spec:
  target:
    name: covered-db-coroot-monitor
    template: {type: kubernetes.io/basic-auth, data: {username: coroot_monitor, password: "{{ .password }}"}}
---
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: allow-coroot-postgres-scrape, namespace: app}
spec:
  endpointSelector: {matchLabels: {cnpg.io/cluster: covered-db}}
  ingress:
    - fromEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: observability
            app.kubernetes.io/managed-by: coroot-operator
            app.kubernetes.io/part-of: coroot
            app.kubernetes.io/component: coroot-cluster-agent
      toPorts:
        - ports: [{port: "5432", protocol: TCP}]
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: agent-db, namespace: observability}
spec:
  inheritedMetadata:
    annotations:
      coroot.com/postgres-scrape: "true"
      coroot.com/postgres-scrape-port: "5432"
      coroot.com/postgres-scrape-credentials-secret-name: agent-db-coroot-monitor
      coroot.com/postgres-scrape-credentials-secret-username-key: username
      coroot.com/postgres-scrape-credentials-secret-password-key: password
      coroot.com/postgres-scrape-param-sslmode: require
  postgresql:
    parameters: {pg_stat_statements.track: all}
  managed:
    roles:
      - {name: coroot_monitor, ensure: present, login: true, inRoles: [pg_monitor], passwordSecret: {name: agent-db-coroot-monitor}}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: agent-db-coroot-monitor, namespace: observability}
spec:
  target:
    name: agent-db-coroot-monitor
    template: {type: kubernetes.io/basic-auth, data: {username: coroot_monitor, password: "{{ .password }}"}}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: bare-db, namespace: app}
spec:
  instances: 1
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: open-db, namespace: other}
spec:
  inheritedMetadata:
    annotations:
      coroot.com/postgres-scrape: "true"
      coroot.com/postgres-scrape-port: "5432"
      coroot.com/postgres-scrape-credentials-secret-name: open-db-coroot-monitor
      coroot.com/postgres-scrape-credentials-secret-username-key: username
      coroot.com/postgres-scrape-credentials-secret-password-key: password
      coroot.com/postgres-scrape-param-sslmode: require
  postgresql:
    parameters: {pg_stat_statements.track: all}
  managed:
    roles:
      - {name: coroot_monitor, ensure: present, login: true, inRoles: [pg_monitor], passwordSecret: {name: open-db-coroot-monitor}}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: open-db-coroot-monitor, namespace: other}
spec:
  target:
    name: open-db-coroot-monitor
    template: {type: kubernetes.io/basic-auth, data: {username: coroot_monitor, password: "{{ .password }}"}}
---
# Admits the whole observability namespace, not only the cluster-agent.
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata: {name: allow-coroot-postgres-scrape, namespace: other}
spec:
  endpointSelector: {matchLabels: {cnpg.io/cluster: open-db}}
  ingress:
    - fromEndpoints:
        - matchLabels: {k8s:io.kubernetes.pod.namespace: observability}
      toPorts:
        - ports: [{port: "5432", protocol: TCP}]
EOF
fixture_gaps="$(check_databases "${workdir}/fixture.yaml")"
expected_gaps='app/bare-db: the coroot.com/postgres-scrape annotations are missing or incomplete in spec.inheritedMetadata
app/bare-db: no coroot_monitor login in pg_monitor whose password comes from the scrape Secret
app/bare-db: pg_stat_statements is not enabled
app/bare-db: no ExternalSecret creates its scrape Secret for the coroot_monitor login
app/bare-db: no CiliumNetworkPolicy admits only the Coroot cluster-agent to its pods on 5432
other/open-db: no CiliumNetworkPolicy admits only the Coroot cluster-agent to its pods on 5432'
[[ "${fixture_gaps}" == "${expected_gaps}" ]] ||
  fail "the checks found these gaps in their fixture instead of the expected ones; fix the checks before trusting this test:
${fixture_gaps}"

# --- Check every production layer Flux reconciles -----------------------------
rendered="${workdir}/rendered.yaml"
: >"${rendered}"
for layer in "${layers[@]}"; do
  # Check each build on its own, so a layer that failed to build cannot read
  # as one with no databases.
  kubectl kustomize "${root_dir}/k8s/providers/hetzner/${layer}" >>"${rendered}" 2>"${workdir}/render.err" ||
    fail "k8s/providers/hetzner/${layer} failed to build: $(tail -5 "${workdir}/render.err")"
  printf '\n---\n' >>"${rendered}"
done

databases="$(yq e -N 'select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster") |
    .metadata.namespace + "/" + .metadata.name' "${rendered}" | sort)"
[[ -n "${databases}" ]] ||
  fail 'the production layers rendered no CNPG Cluster; the selector or the render is broken'

gaps="$(check_databases "${rendered}")"
[[ -z "${gaps}" ]] || fail "every production database needs Coroot's Postgres integration (see this test's header):
${gaps}"

printf 'PASS: all %s production databases (%s) carry Coroot'"'"'s Postgres integration\n' \
  "$(printf '%s\n' "${databases}" | wc -l | tr -d ' ')" "$(printf '%s' "${databases}" | tr '\n' ' ')"
