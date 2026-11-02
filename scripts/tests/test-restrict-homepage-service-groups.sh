#!/usr/bin/env bash
# The Homepage layout and the Kyverno allow-list are two lists that must agree.
# The policy exists because an undeclared group fails silently; a duplicated
# allow-list that drifts would fail just as silently, so this pins them together.
#
# Service groups are derived STRUCTURALLY — every layout key that is not a
# bookmark group — rather than by reading the section comment, so moving or
# rewording that comment cannot quietly change what this test believes.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
config_map="${repo_root}/k8s/bases/apps/homepage/config-map.yaml"
policy="${repo_root}/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-homepage-service-groups.yaml"

for f in "${config_map}" "${policy}"; do
  [ -r "$f" ] || {
    echo "::error::missing or unreadable: $f"
    exit 1
  }
done

# --- 1. the policy must DENY, on creates and updates only ---------------------
action="$(yq -r '.spec.validationActions | join(",")' "${policy}")"
if [ "${action}" != "Deny" ]; then
  echo "::error::the policy must set validationActions: [Deny] (found: ${action})"
  exit 1
fi

# Deleting a route is never this policy's business (#4732). A retirement removes
# a group from the allow-list and the last route using it in one deploy, and the
# infrastructure layer lands first, so the route is deleted while its group is
# already undeclared. The CLI cannot show this: a ValidatingPolicy's operations
# only decide which requests the webhook receives.
rules="$(yq -o=json -I=0 '[.spec.matchConstraints.resourceRules[] | {"group": .apiGroups[0], "resource": .resources[0], "operations": (.operations | sort)}]' "${policy}")"
expected='[{"group":"gateway.networking.k8s.io","resource":"httproutes","operations":["CREATE","UPDATE"]},{"group":"networking.k8s.io","resource":"ingresses","operations":["CREATE","UPDATE"]}]'
if [ "${rules}" != "${expected}" ]; then
  echo "::error::the policy must match HTTPRoute and Ingress on CREATE and UPDATE only (found: ${rules})"
  exit 1
fi

# --- 2. allow-list == the layout's SERVICE groups ----------------------------
settings="$(yq -r '.data."settings.yaml"' "${config_map}")"
bookmarks="$(yq -r '.data."bookmarks.yaml"' "${config_map}")"
if [ -z "${settings}" ] || [ "${settings}" = "null" ]; then
  echo "::error::settings.yaml missing from the ConfigMap"
  exit 1
fi
if [ -z "${bookmarks}" ] || [ "${bookmarks}" = "null" ]; then
  echo "::error::bookmarks.yaml missing from the ConfigMap"
  exit 1
fi

layout_groups="$(printf '%s\n' "${settings}" | yq -r '.layout | keys | .[]' | sort -u)"
bookmark_groups="$(printf '%s\n' "${bookmarks}" | yq -r '.[] | keys | .[0]' | sort -u)"
service_groups="$(comm -23 <(printf '%s\n' "${layout_groups}") <(printf '%s\n' "${bookmark_groups}"))"

policy_groups="$(yq -r '.spec.variables[] | select(.name == "declaredGroups") | .expression' "${policy}" |
  grep -o "'[^']*'" | tr -d "'" | sort -u)"

# Proof-of-life: an empty side would make the comparison pass vacuously.
for pair in "layout:${layout_groups}" "bookmark:${bookmark_groups}" "service:${service_groups}" "policy:${policy_groups}"; do
  name="${pair%%:*}"
  body="${pair#*:}"
  n="$(printf '%s\n' "${body}" | grep -c . || true)"
  if [ "${n}" -lt 1 ]; then
    echo "::error::extracted zero ${name} groups — the parse broke, so this check would pass vacuously"
    exit 1
  fi
done

if ! diff_out="$(diff <(printf '%s\n' "${service_groups}") <(printf '%s\n' "${policy_groups}"))"; then
  echo "::error::Homepage layout service groups and the Kyverno allow-list disagree."
  echo "  '<' = declared in the layout but not allowed by the policy"
  echo "  '>' = allowed by the policy but not declared in the layout"
  printf '%s\n' "${diff_out}"
  exit 1
fi

# A bookmark group must never be accepted for a service: Homepage renders the two
# as separate sections, so a shared name paints one heading twice.
while IFS= read -r b; do
  [ -n "$b" ] || continue
  if printf '%s\n' "${policy_groups}" | grep -qxF -- "$b"; then
    echo "::error::policy allows bookmark group '${b}' as a service group"
    exit 1
  fi
done <<EOF
${bookmark_groups}
EOF

# The deny message names the valid groups so a rejected apply does not have to go
# read the layout. That is a THIRD copy of the list, so pin it too — otherwise the
# admission error goes stale exactly when a group is added, which is when someone
# is most likely to be reading it.
message="$(yq -r '.spec.validations[0].message' "${policy}")"
if [ -z "${message}" ] || [ "${message}" = "null" ]; then
  echo "::error::the policy has no validation message"
  exit 1
fi
while IFS= read -r g; do
  [ -n "${g}" ] || continue
  if ! printf '%s' "${message}" | tr '\n' ' ' | grep -qF -- "${g}"; then
    echo "::error::deny message does not name the allowed group '${g}'"
    exit 1
  fi
done <<EOF
${service_groups}
EOF

# The company site and its client projects share Business & Clients (the
# company site first); personal sites stay in their own group.
for group in "Business & Clients" Personal; do
  if ! printf '%s\n' "${service_groups}" | grep -qxF -- "${group}"; then
    echo "::error::Homepage is missing the '${group}' service group"
    exit 1
  fi
done

# shellcheck disable=SC2016 # $group is a jq binding, not a shell variable.
business_sites="$(yq -r '.data."services.yaml"' "${config_map}" | yq -o=json | jq -c '
  [.[] | to_entries[] | .key as $group | .value[] | to_entries[]
   | select(.value.href == "https://devantler.tech") | {"group": $group}]')"
if [ "${business_sites}" != '[{"group":"Business & Clients"}]' ]; then
  echo "::error::devantler.tech must appear exactly once under Business & Clients (found: ${business_sites})"
  exit 1
fi

count="$(printf '%s\n' "${service_groups}" | grep -c .)"
echo "Homepage layout, Kyverno allow-list and deny message agree on ${count} service groups; policy denies."
