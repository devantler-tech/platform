#!/usr/bin/env bash
# Offline real-admission controls for the optional tenant WebApp capability.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
fail() { printf 'tenant WebApp boundary: %s\n' "$*" >&2; exit 1; }
graph="$root/k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml"
policy_dir="$root/k8s/bases/infrastructure/cluster-policies/best-practices"
parent="$policy_dir/restrict-tenant-webapps.yaml"
children="$policy_dir/restrict-webapp-child-ownership.yaml"
role="$root/k8s/bases/infrastructure/cluster-roles/tenant-webapp-edit.yaml"
GITHUB_OUTPUT="$scratch/version" bash "$root/.github/scripts/kyverno-version.sh"
engine_version="$(cd "$root/scripts/tests/tenant-webapp-admission" && go list -m -f '{{.Version}}' github.com/kyverno/kyverno)"
[[ "$(< "$scratch/version")" == "release=$engine_version" ]] || fail 'the admission engine must match the reviewed Kyverno CLI pin'

[[ "$(yq -r '.spec.schema.spec.webApp' "$graph")" == 'boolean | default=false' ]] || fail 'the optional capability must default false'
yq -o=json '.' "$role" | jq -e '
  .kind == "ClusterRole" and .metadata.name == "tenant-webapp-edit" and
  ((.metadata.labels // {}) | keys | all(. != "platform.devantler.tech/aggregate-to-tenant-edit" and . != "rbac.kro.run/aggregate-to-controller" and (startswith("rbac.authorization.k8s.io/aggregate-to-")|not))) and
  .rules == [{apiGroups:["kro.run"],resources:["webapps"],verbs:["get","list","watch","create","update","patch","delete"]}]
' >/dev/null || fail 'the dedicated role must grant WebApps only, without aggregation'
yq -o=json '.spec.resources[] | select(.id == "webAppBinding")' "$graph" | jq -e '
  .includeWhen == ["${schema.spec.webApp}"] and
  .template.kind == "RoleBinding" and
  .template.metadata.namespace == "${tenantNamespace.metadata.name}" and
  .template.roleRef == {apiGroup:"rbac.authorization.k8s.io",kind:"ClusterRole",name:"tenant-webapp-edit"} and
  .template.subjects == [{kind:"ServiceAccount",name:"${serviceAccount.metadata.name}",namespace:"${tenantNamespace.metadata.name}"}]
' >/dev/null || fail 'the enabled capability must add only the namespaced binding'

count=0
check() {
  local label="$1" policy="$2" object="$3" old="$4" actor="$5" operation="$6" expected="$7" subresource="${8:-}"
  jq -nc --arg label "$label" --arg policy "$(yq -r '.metadata.name' "$policy")" --argjson object "$object" --argjson old "$old" --arg actor "$actor" --arg operation "$operation" --arg expected "$expected" --arg subresource "$subresource" \
    '{label:$label,policy:$policy,object:$object,oldObject:$old,actor:$actor,operation:$operation,expected:$expected,subresource:$subresource}' >> "$scratch/requests.jsonl"
  count=$((count+1))
}

tenant='system:serviceaccount:tenant-a:tenant-a'
kro='system:serviceaccount:kro-system:kro'
flux='system:serviceaccount:flux-system:kustomize-controller'
app='{"apiVersion":"kro.run/v1alpha1","kind":"WebApp","metadata":{"name":"tenant-a","namespace":"tenant-a"},"spec":{"name":"tenant-a","image":"example.invalid/fixture:v1"}}'
check own-create "$parent" "$app" null "$tenant" CREATE pass
check normal-update "$parent" "$(jq '.spec.image="example.invalid/fixture:v2"' <<< "$app")" "$app" "$tenant" UPDATE pass
check missing-target "$parent" "$(jq 'del(.spec.name)' <<< "$app")" null "$tenant" CREATE fail
check foreign-target "$parent" "$(jq '.spec.name="foreign"' <<< "$app")" null "$tenant" CREATE fail
check colliding-parent "$parent" "$(jq '.metadata.name="second-parent"' <<< "$app")" null "$tenant" CREATE fail
check retarget "$parent" "$(jq '.spec.name="foreign"' <<< "$app")" "$app" "$tenant" UPDATE fail
check kro-metadata "$parent" "$(jq '.metadata.finalizers=["kro.run/finalizer"]|.metadata.labels={"applyset.kubernetes.io/parent":"fixture"}' <<< "$app")" "$app" "$kro" UPDATE pass
check kro-spec-change "$parent" "$(jq '.spec.image="example.invalid/fixture:v2"' <<< "$app")" "$app" "$kro" UPDATE fail
check kro-status "$parent" "$(jq '.status={state:"ACTIVE"}' <<< "$app")" "$app" "$kro" UPDATE pass status
check kro-status-spec-change "$parent" "$(jq '.spec.image="example.invalid/fixture:v2"' <<< "$app")" "$app" "$kro" UPDATE fail status
pilot="$(jq '.metadata={name:"wedding-app",namespace:"flux-system"}|.spec.name="wedding-app"' <<< "$app")"
check platform-pilot "$parent" "$pilot" null "$flux" CREATE pass
check platform-pilot-maintenance "$parent" "$pilot" "$pilot" "$kro" UPDATE pass
check forged-platform "$parent" "$pilot" null 'system:serviceaccount:tenant-a:kustomize-controller' CREATE fail
check unsupported-platform-parent "$parent" "$(jq '.metadata.name="other"' <<< "$pilot")" null "$flux" CREATE fail
owned_app="$(jq '.metadata.labels={"kro.run/state":"ACTIVE","applyset.kubernetes.io/parent":"fixture"}' <<< "$app")"
check retained-parent-metadata "$parent" "$owned_app" "$owned_app" "$tenant" UPDATE pass
check forged-parent-metadata "$parent" "$owned_app" null "$tenant" CREATE fail
check removed-parent-metadata "$parent" "$app" "$owned_app" "$tenant" UPDATE fail
check changed-parent-metadata "$parent" "$(jq '.metadata.labels["applyset.kubernetes.io/parent"]="other"' <<< "$owned_app")" "$owned_app" "$tenant" UPDATE fail
annotated_app="$(jq '.metadata.annotations={"kro.run/fixture":"controller"}' <<< "$app")"
check forged-parent-annotation "$parent" "$annotated_app" null "$tenant" CREATE fail
check retained-parent-annotation "$parent" "$annotated_app" "$annotated_app" "$tenant" UPDATE pass
check removed-parent-annotation "$parent" "$app" "$annotated_app" "$tenant" UPDATE fail

child='{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"tenant-a","namespace":"tenant-a","labels":{"kro.run/instance-id":"11111111-1111-1111-1111-111111111111","kro.run/instance-name":"tenant-a","kro.run/instance-namespace":"tenant-a","kro.run/instance-group":"kro.run","kro.run/instance-version":"v1alpha1","kro.run/instance-kind":"WebApp","kro.run/node-id":"deployment","applyset.kubernetes.io/part-of":"fixture-membership"}},"spec":{"replicas":1}}'
for kind in Deployment Service HTTPRoute; do
  case "$kind" in
    Deployment) api='apps/v1'; node=deployment ;;
    Service) api=v1; node=service ;;
    HTTPRoute) api='gateway.networking.k8s.io/v1'; node=httpRoute ;;
  esac
  object="$(jq --arg api "$api" --arg kind "$kind" --arg node "$node" '.apiVersion=$api|.kind=$kind|.metadata.labels["kro.run/node-id"]=$node' <<< "$child")"
  check "$kind-create" "$children" "$object" null "$kro" CREATE pass
  check "$kind-update" "$children" "$object" "$object" "$kro" UPDATE pass
  unowned="$(jq 'del(.metadata.labels)' <<< "$object")"
  check "$kind-unowned-collision" "$children" "$object" "$unowned" "$kro" UPDATE fail
  check "$kind-forged-labels" "$children" "$object" null "$tenant" CREATE fail
  check "$kind-removed-labels" "$children" "$unowned" "$object" "$tenant" UPDATE fail
  check "$kind-ordinary-workload" "$children" "$unowned" null "$tenant" CREATE pass
done
for key in 'kro.run/instance-id' 'kro.run/instance-name' 'kro.run/instance-namespace' 'kro.run/instance-group' 'kro.run/instance-version' 'kro.run/node-id' 'applyset.kubernetes.io/part-of'; do
  changed="$(jq --arg key "$key" '.metadata.labels[$key]="foreign"' <<< "$child")"
  check "foreign-existing-$key" "$children" "$child" "$changed" "$kro" UPDATE fail
  check "removed-$key" "$children" "$(jq --arg key "$key" 'del(.metadata.labels[$key])' <<< "$child")" null "$kro" CREATE fail
done
check foreign-existing-kind "$children" "$child" "$(jq '.metadata.labels["kro.run/instance-kind"]="Tenant"' <<< "$child")" "$kro" UPDATE fail
check removed-kind-on-update "$children" "$(jq 'del(.metadata.labels["kro.run/instance-kind"])' <<< "$child")" "$child" "$kro" UPDATE fail
check foreign-child-namespace "$children" "$(jq '.metadata.namespace="foreign"' <<< "$child")" null "$kro" CREATE fail
check foreign-child-name "$children" "$(jq '.metadata.name="foreign"' <<< "$child")" null "$kro" CREATE fail
check pilot-child "$children" "$(jq '.metadata.name="wedding-app"|.metadata.namespace="wedding-app"|.metadata.labels["kro.run/instance-name"]="wedding-app"|.metadata.labels["kro.run/instance-namespace"]="flux-system"' <<< "$child")" null "$kro" CREATE pass
# The CLI's UPDATE adapter clones object into oldObject and drops subresources.
# Feed the unmodified requests directly to the same pinned compiler/engine.
(
  cd "$root/scripts/tests/tenant-webapp-admission"
  go test -mod=readonly -count=1 -timeout=60s ./...
  go run -mod=readonly . "$parent" "$children" < "$scratch/requests.jsonl"
)
printf 'Tenant WebApp boundary: %d controls submitted.\n' "$count"
