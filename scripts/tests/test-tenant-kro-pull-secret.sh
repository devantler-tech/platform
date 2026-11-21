#!/usr/bin/env bash
# Exercise the retired Tenant pull-secret boundary in the pinned offline
# admission engine. First-party packages are public (platform#6272), so the
# Tenant RGD renders no pull-secret ExternalSecret and KRO has no carve-out:
# the former generated shape must now be denied for every actor, while the
# reserved-ownership-label rules keep holding for tenant writers.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
policy="$root/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-secret-stores.yaml"
graph="$root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"

# The constructor renders no ExternalSecret and no pull-secret reference at all.
if yq -e '.spec.resources[] | select(.template.kind == "ExternalSecret")' "$graph" >/dev/null 2>&1; then
  echo 'the Tenant RGD renders an ExternalSecret again' >&2
  exit 1
fi
if grep -Eq 'ghcr-auth|infrastructure/ghcr/' "$graph"; then
  echo 'the Tenant RGD references the retired GHCR pull secret' >&2
  exit 1
fi

# The shape earlier RGD revisions rendered, with KRO's ownership labels.
jq -n '{
  apiVersion:"external-secrets.io/v1",kind:"ExternalSecret",
  metadata:{name:"ghcr-auth",namespace:"business-site",labels:{
    "app.kubernetes.io/managed-by":"kro",
    "kro.run/instance-id":"11111111-1111-4111-8111-111111111111",
    "kro.run/instance-name":"business-site","kro.run/instance-namespace":"flux-system",
    "kro.run/instance-group":"kro.run","kro.run/instance-version":"v1alpha1",
    "kro.run/instance-kind":"Tenant","kro.run/node-id":"ghcrAuth",
    "applyset.kubernetes.io/part-of":"fictional-tenant-applyset"}},
  spec:{refreshInterval:"1h",secretStoreRef:{name:"openbao",kind:"ClusterSecretStore"},
    target:{name:"ghcr-auth",creationPolicy:"Owner",deletionPolicy:"Retain",
      template:{engineVersion:"v2",mergePolicy:"Replace",type:"kubernetes.io/dockerconfigjson",
        data:{".dockerconfigjson":"{{ .dockerconfigjson }}"}}},
    data:[{secretKey:"dockerconfigjson",remoteRef:{key:"infrastructure/ghcr/auth",property:"dockerconfigjson"}}]}
}' >"$work/retired.json"
# A tenant's own ExternalSecret through its namespaced SecretStore.
jq '.metadata.labels={} | .metadata.name="app" | .spec.secretStoreRef={name:"business-site",kind:"SecretStore"} |
  .spec.data[0].remoteRef.key="apps/business-site/app"' "$work/retired.json" >"$work/tenant.json"

jq -nc --slurpfile retired "$work/retired.json" --slurpfile tenant "$work/tenant.json" '
  def request($label;$object;$old;$actor;$operation;$expected):
    {label:$label,policy:"restrict-tenant-secret-stores",object:$object,oldObject:$old,
     actor:$actor,operation:$operation,expected:$expected,
     namespaces:(["business-site","other-tenant"]|map({apiVersion:"v1",kind:"Namespace",metadata:{name:.,labels:{"app.kubernetes.io/managed-by":"ksail"}}}))};
  def kro($label;$object;$old;$operation;$expected): request($label;$object;$old;"system:serviceaccount:kro-system:kro";$operation;$expected);
  def tenant($label;$object;$old;$operation;$expected): request($label;$object;$old;"system:serviceaccount:business-site:business-site";$operation;$expected);
  kro("retired generated pull secret";$retired[0];null;"CREATE";"fail"),
  kro("retired owned update";$retired[0];$retired[0];"UPDATE";"fail"),
  kro("retired shape in another tenant";($retired[0]|.metadata.namespace="other-tenant");null;"CREATE";"fail"),
  tenant("tenant cannot spoof controller";$retired[0];null;"CREATE";"fail"),
  tenant("tenant cannot copy namespace-only profile";($retired[0]|.metadata.labels={});null;"CREATE";"fail"),
  tenant("tenant namespaced store";$tenant[0];null;"CREATE";"pass"),
  kro("controller namespaced store";$tenant[0];null;"CREATE";"pass"),
  tenant("tenant cannot add reserved labels";($retired[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"});null;"CREATE";"fail"),
  tenant("tenant cannot remove reserved labels";($retired[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"}|del(.metadata.labels["kro.run/instance-id"]));($retired[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"});"UPDATE";"fail")
' >"$work/requests.jsonl"
expected="$(jq -s length "$work/requests.jsonl")"
(
  cd "$root/scripts/tests/tenant-webapp-admission"
  go build -mod=readonly -o "$work/admission" .
)
for owner in ksail kro; do
  jq -c --arg owner "$owner" '.namespaces[].metadata.labels["app.kubernetes.io/managed-by"]=$owner' \
    "$work/requests.jsonl" >"$work/owner.jsonl"
  "$work/admission" "$policy" <"$work/owner.jsonl" >"$work/result"
  grep -Fq "$expected explicit Kyverno engine controls passed" "$work/result"
done
# Removing namespace observations must fail instead of silently excluding cases.
jq -c 'del(.namespaces)' "$work/requests.jsonl" >"$work/no-namespaces.jsonl"
if "$work/admission" "$policy" <"$work/no-namespaces.jsonl" >"$work/no-namespaces.log" 2>&1; then
  echo 'missing namespace observation silently bypassed the policy' >&2
  exit 1
fi
grep -Fq 'policy did not evaluate' "$work/no-namespaces.log"
printf 'Retired KRO pull-secret boundary: %s real admission controls passed.\n' "$expected"
