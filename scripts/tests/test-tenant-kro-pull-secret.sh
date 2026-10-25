#!/usr/bin/env bash
# Exercise the real constructor body in the pinned offline admission engine.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
policy="$root/k8s/bases/infrastructure/cluster-policies/best-practices/restrict-tenant-secret-stores.yaml"
graph="$root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
yq -o=json '.spec.resources[] | select(.id == "ghcrAuth") | .template' "$graph" >"$work/template.json"
jq -e '.apiVersion == "external-secrets.io/v1" and .kind == "ExternalSecret" and .metadata.namespace == "${tenantNamespace.metadata.name}"' "$work/template.json" >/dev/null
jq '.metadata.namespace="business-site" | .metadata.labels += {
  "kro.run/instance-id":"11111111-1111-4111-8111-111111111111",
  "kro.run/instance-name":"business-site","kro.run/instance-namespace":"flux-system",
  "kro.run/instance-group":"kro.run","kro.run/instance-version":"v1alpha1",
  "kro.run/instance-kind":"Tenant","kro.run/node-id":"ghcrAuth",
  "applyset.kubernetes.io/part-of":"fictional-tenant-applyset"}' "$work/template.json" >"$work/raw.json"
# These are the structural defaults in the pinned ExternalSecrets 2.13.0 v1 CRD.
jq '.spec.target.deletionPolicy="Retain" | .spec.target.template.mergePolicy="Replace"' "$work/raw.json" >"$work/defaulted.json"
jq -nc --slurpfile raw "$work/raw.json" --slurpfile defaults "$work/defaulted.json" '
  def request($label;$object;$old;$actor;$operation;$expected):
    {label:$label,policy:"restrict-tenant-secret-stores",object:$object,oldObject:$old,
     actor:$actor,operation:$operation,expected:$expected,
     namespaces:(["business-site","other-tenant"]|map({apiVersion:"v1",kind:"Namespace",metadata:{name:.,labels:{"app.kubernetes.io/managed-by":"ksail"}}}))};
  def kro($label;$object;$old;$operation;$expected): request($label;$object;$old;"system:serviceaccount:kro-system:kro";$operation;$expected);
  def deny($label;$object): kro($label;$object;null;"CREATE";"fail");
  kro("actual generated pull secret";$raw[0];null;"CREATE";"pass"),
  kro("actual defaulted pull secret";$defaults[0];null;"CREATE";"pass"),
  kro("explicit documented defaults";($defaults[0]|.spec.refreshPolicy="Periodic"|.spec.data[0].remoteRef += {conversionStrategy:"Default",decodingStrategy:"None",metadataPolicy:"None"});null;"CREATE";"pass"),
  kro("owned update";$raw[0];$defaults[0];"UPDATE";"pass"),
  request("tenant cannot spoof controller";$raw[0];null;"system:serviceaccount:business-site:business-site";"CREATE";"fail"),
  request("tenant cannot copy namespace-only profile";($raw[0]|.metadata.labels={});null;"system:serviceaccount:business-site:business-site";"CREATE";"fail"),
  deny("foreign placement";($raw[0]|.metadata.namespace="other-tenant")),
  deny("other name";($raw[0]|.metadata.name="other")),
  deny("foreign store";($raw[0]|.spec.secretStoreRef.name="other")),
  deny("other path";($raw[0]|.spec.data[0].remoteRef.key="other")),
  deny("other property";($raw[0]|.spec.data[0].remoteRef.property="other")),
  deny("version override";($raw[0]|.spec.data[0].remoteRef.version="1")),
  deny("decoding override";($raw[0]|.spec.data[0].remoteRef.decodingStrategy="Auto")),
  deny("conversion override";($raw[0]|.spec.data[0].remoteRef.conversionStrategy="Unicode")),
  deny("metadata override";($raw[0]|.spec.data[0].remoteRef.metadataPolicy="Fetch")),
  deny("extra read";($raw[0]|.spec.data += [.spec.data[0]])),
  deny("dataFrom injection";($raw[0]|.spec.dataFrom=[{extract:{key:"other"}}])),
  deny("sourceRef injection";($raw[0]|.spec.data[0].sourceRef={storeRef:{name:"other",kind:"ClusterSecretStore"}})),
  deny("templateFrom injection";($raw[0]|.spec.target.template.templateFrom=[{configMap:{name:"other"}}])),
  deny("template data injection";($raw[0]|.spec.target.template.data.other="other")),
  deny("template expression injection";($raw[0]|.spec.target.template.data[".dockerconfigjson"]="other")),
  deny("template metadata injection";($raw[0]|.spec.target.template.metadata={annotations:{other:"other"}})),
  deny("target manifest injection";($raw[0]|.spec.target.manifest={other:"other"})),
  deny("target redirect";($raw[0]|.spec.target.name="other")),
  deny("target ownership";($raw[0]|.spec.target.creationPolicy="Merge")),
  deny("target deletion";($raw[0]|.spec.target.deletionPolicy="Delete")),
  deny("target merge";($raw[0]|.spec.target.template.mergePolicy="Merge")),
  deny("target engine";($raw[0]|.spec.target.template.engineVersion="v1")),
  deny("target type";($raw[0]|.spec.target.template.type="Opaque")),
  deny("refresh behavior";($raw[0]|.spec.refreshPolicy="OnChange")),
  deny("managed label";($raw[0]|.metadata.labels["app.kubernetes.io/managed-by"]="other")),
  deny("other API";($raw[0]|.apiVersion="external-secrets.io/v1beta1")),
  (["kro.run/instance-id","kro.run/instance-name","kro.run/instance-namespace","kro.run/instance-group","kro.run/instance-version","kro.run/instance-kind","kro.run/node-id"][] as $key |
    deny("substituted identity "+$key;($raw[0]|.metadata.labels[$key]="other"))),
  deny("empty ApplySet";($raw[0]|.metadata.labels["applyset.kubernetes.io/part-of"]="")),
  ([$raw[0].metadata.labels|keys[]|select(startswith("kro.run/") or startswith("applyset.kubernetes.io/"))][] as $key |
    deny("missing identity "+$key;($raw[0]|del(.metadata.labels[$key])))),
  kro("unowned collision";$raw[0];($defaults[0]|.metadata.labels={});"UPDATE";"fail"),
  ( ["kro.run/instance-id","kro.run/instance-namespace","kro.run/node-id","applyset.kubernetes.io/part-of"][] as $key |
    kro("foreign existing identity "+$key;$raw[0];($defaults[0]|.metadata.labels[$key]="other");"UPDATE";"fail") ),
  request("tenant cannot add reserved labels";($raw[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"});null;"system:serviceaccount:business-site:business-site";"CREATE";"fail"),
  request("tenant cannot remove reserved labels";($raw[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"}|del(.metadata.labels["kro.run/instance-id"]));($defaults[0]|.spec.secretStoreRef={name:"business-site",kind:"SecretStore"});"system:serviceaccount:business-site:business-site";"UPDATE";"fail")
' >"$work/requests.jsonl"
expected="$(jq -s length "$work/requests.jsonl")"
(
  cd "$root/scripts/tests/tenant-webapp-admission"
  go build -mod=readonly -o "$work/admission" .
)
"$work/admission" "$policy" <"$work/requests.jsonl" >"$work/result"
grep -Fq "$expected explicit Kyverno engine controls passed" "$work/result"
# Removing namespace observations must fail instead of silently excluding cases.
jq -c 'del(.namespaces)' "$work/requests.jsonl" >"$work/no-namespaces.jsonl"
if "$work/admission" "$policy" <"$work/no-namespaces.jsonl" >"$work/no-namespaces.log" 2>&1; then
  echo 'missing namespace observation silently bypassed the policy' >&2; exit 1
fi
grep -Fq 'policy did not evaluate' "$work/no-namespaces.log"
printf 'KRO pull-secret boundary: %s real admission controls passed.\n' "$expected"
