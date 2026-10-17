#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly kyverno_dir="${root_dir}/k8s/bases/infrastructure/controllers/kyverno"
readonly umami_dir="${root_dir}/k8s/bases/apps/umami"
readonly hetzner_apps_dir="${root_dir}/k8s/providers/hetzner/apps"
readonly hetzner_controllers_dir="${root_dir}/k8s/providers/hetzner/infrastructure/controllers"
readonly grant_name="kyverno:background-controller:mutate-umami-primary"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# Kubernetes RBAC cannot limit patch/update on a Deployment to metadata, so ANY
# cluster-wide Deployment write lets a compromised background-controller token
# rewrite arbitrary workload pod templates. Assert no ClusterRole grants it,
# whether or not the rule pins resourceNames -- a blanket rule (the shape that
# shipped before #2721) carries no resourceNames at all, so a resourceNames-only
# check cannot see the broadest, most dangerous form.
#
# Comparisons use anchored test() rather than ==: in yq, `. == "*"` is a GLOB and
# matches every string, which would silently match rules that hold no wildcard.
assert_no_cluster_wide_deployment_write() {
  local rendered="$1"
  local label="$2"
  local offenders

  offenders="$(yq -r '
    select(.kind == "ClusterRole")
    | select([ .rules[]?
        | select([.apiGroups[]? | select(test("^(apps|\*)$"))] | length > 0)
        | select([.resources[]? | select(test("^(deployments|\*)$"))] | length > 0)
        | select([.verbs[]? | select(test("^(create|delete|deletecollection|patch|update|\*)$"))] | length > 0)
      ] | length > 0)
    | .metadata.name
  ' "${rendered}")" || fail "${label} must render for the cluster-wide Deployment write check"

  if [[ -n "${offenders//[[:space:]]/}" ]]; then
    fail "${label} must not grant cluster-wide Deployment write to Kyverno: ${offenders//$'\n'/ }"
  fi
}

extract_resource() {
  local kind="$1"
  local name="$2"

  awk -v wanted_kind="${kind}" -v wanted_name="${name}" '
    function reset_document() {
      document = ""
      resource_kind = ""
      resource_name = ""
      in_metadata = 0
    }
    function emit_if_match() {
      if (!found && resource_kind == wanted_kind && resource_name == wanted_name) {
        printf "%s", document
        found = 1
      }
      reset_document()
    }
    BEGIN { reset_document() }
    /^---[[:space:]]*$/ { emit_if_match(); next }
    {
      if (!found) {
        document = document $0 ORS
        if ($0 ~ /^kind:[[:space:]]*/) {
          resource_kind = $0
          sub(/^kind:[[:space:]]*/, "", resource_kind)
        } else if ($0 ~ /^metadata:[[:space:]]*$/) {
          in_metadata = 1
        } else if ($0 ~ /^[^[:space:]]/ && $0 !~ /^metadata:/) {
          in_metadata = 0
        } else if (in_metadata && $0 ~ /^  name:[[:space:]]*/) {
          resource_name = $0
          sub(/^  name:[[:space:]]*/, "", resource_name)
        }
      }
    }
    END {
      if (!found) {
        emit_if_match()
      }
      if (!found) {
        exit 1
      }
    }
  '
}

rendered_file="$(mktemp)"
trap 'rm -f "${rendered_file}"' EXIT

kubectl kustomize "${kyverno_dir}" >"${rendered_file}" ||
  fail 'the Kyverno controller base must render'

role="$(extract_resource Role "${grant_name}" <"${rendered_file}")" ||
  fail 'the Umami mutation grant must render as a namespaced Role'
role_binding="$(extract_resource RoleBinding "${grant_name}" <"${rendered_file}")" ||
  fail 'the Umami mutation grant must render a namespaced RoleBinding'
umami_namespace="$(extract_resource Namespace umami <"${rendered_file}")" ||
  fail 'the infrastructure-controllers layer must create the Umami namespace before its mutation grant'

[[ "$(yq -r '.metadata.labels."pod-security.kubernetes.io/enforce"' <<<"${umami_namespace}")" == 'restricted' ]] ||
  fail 'the early Umami namespace must retain restricted Pod Security enforcement'

[[ "$(yq -r '.metadata.namespace' <<<"${role}")" == 'umami' ]] ||
  fail 'the mutation Role must be scoped to the umami namespace'
[[ "$(yq -r '.rules[0].apiGroups | join(",")' <<<"${role}")" == 'apps' ]] ||
  fail 'the mutation Role must grant only the apps API group'
[[ "$(yq -r '.rules[0].resources | join(",")' <<<"${role}")" == 'deployments' ]] ||
  fail 'the mutation Role must grant only Deployments'
[[ "$(yq -r '.rules[0].resourceNames | join(",")' <<<"${role}")" == 'umami-umami-primary' ]] ||
  fail 'the mutation Role must grant only the Umami Flagger primary'
[[ "$(yq -r '.rules[0].verbs | sort | join(",")' <<<"${role}")" == 'get,patch,update' ]] ||
  fail 'the mutation Role must grant only get, patch, and update'

[[ "$(yq -r '.metadata.namespace' <<<"${role_binding}")" == 'umami' ]] ||
  fail 'the mutation RoleBinding must be scoped to the umami namespace'
[[ "$(yq -r '.roleRef.kind + ":" + .roleRef.name' <<<"${role_binding}")" == "Role:${grant_name}" ]] ||
  fail 'the mutation RoleBinding must bind the namespaced mutation Role'
[[ "$(yq -r '.subjects | length' <<<"${role_binding}")" == '1' ]] ||
  fail 'the mutation RoleBinding must have exactly one subject'
[[ "$(yq -r '.subjects[0].kind + ":" + .subjects[0].namespace + ":" + .subjects[0].name' <<<"${role_binding}")" == 'ServiceAccount:kyverno:kyverno-background-controller' ]] ||
  fail 'the mutation RoleBinding must bind only Kyverno background controller'

if yq -r \
  'select(.kind == "ClusterRole") | .rules[]? | .resourceNames[]? | select(. == "umami-umami-primary")' \
  "${rendered_file}" | grep -q .; then
  fail 'the Umami primary mutation grant must not remain cluster-wide'
fi

assert_no_cluster_wide_deployment_write "${rendered_file}" 'the Kyverno controller base'

umami_rendered_file="$(mktemp)"
trap 'rm -f "${rendered_file}" "${umami_rendered_file}"' EXIT
kubectl kustomize "${umami_dir}" >"${umami_rendered_file}" ||
  fail 'the Umami app base must render'
if extract_resource Namespace umami <"${umami_rendered_file}" >/dev/null; then
  fail 'the Umami app layer must not co-own the namespace created before the Kyverno policy'
fi

hetzner_apps_rendered_file="$(mktemp)"
hetzner_controllers_rendered_file="$(mktemp)"
trap 'rm -f "${rendered_file}" "${umami_rendered_file}" "${hetzner_apps_rendered_file}" "${hetzner_controllers_rendered_file}"' EXIT
kubectl kustomize "${hetzner_apps_dir}" >"${hetzner_apps_rendered_file}" ||
  fail 'the Hetzner apps overlay must render without patching the moved Umami namespace'
kubectl kustomize "${hetzner_controllers_dir}" >"${hetzner_controllers_rendered_file}" ||
  fail 'the Hetzner infrastructure-controllers overlay must render'

assert_no_cluster_wide_deployment_write "${hetzner_controllers_rendered_file}" 'the Hetzner infrastructure-controllers overlay'

if extract_resource Namespace umami <"${hetzner_apps_rendered_file}" >/dev/null; then
  fail 'the Hetzner apps layer must not co-own the Umami namespace'
fi
hetzner_umami_namespace="$(extract_resource Namespace umami <"${hetzner_controllers_rendered_file}")" ||
  fail 'the Hetzner infrastructure-controllers layer must render the Umami namespace'
[[ "$(yq -r '.metadata.labels."pod-security.devantler.tech/user-namespaces"' <<<"${hetzner_umami_namespace}")" == 'enabled' ]] ||
  fail 'the Hetzner infrastructure-controllers layer must preserve Umami user-namespace opt-in'

echo 'Kyverno can mutate only the Umami primary in the umami namespace.'
