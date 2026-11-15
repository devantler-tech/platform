#!/usr/bin/env bash
# Native Kyverno admission/projection tests; no cluster or credentials are used.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="${root}/tests/longhorn-elastic-eviction-guard"
component="${root}/k8s/bases/components/longhorn-elastic-eviction-guard"
policy="${component}/cluster-policy.yaml"
mutation_policy="${component}/uid-mutation-policy.yaml"
work="$(mktemp -d "${root}/tests/.longhorn-elastic-eviction.XXXXXX")"
completed=0
cleanup() {
  local rc=$?
  rm -rf "${work}"
  if [[ "${completed}" != 1 && "${rc}" == 0 ]]; then rc=1; fi
  exit "${rc}"
}
trap cleanup EXIT
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
for tool in kyverno kubectl yq jq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done
# A missing mocked context must never fall through to the user's real cluster.
export KUBECONFIG=/dev/null
yq -o=json '.' "${fixture}/values.yaml" >"${work}/base-values.json"
yq -o=json '.' "${fixture}/kyverno-test.yaml" >"${work}/base-test.json"
count=0
run_case() {
  local name="$1" result="$2" change="$3" resource_change="${4:-.}" mode="${5:-normal}" dir resource namespace selected_policies
  dir="${work}/${name}"
  mkdir "${dir}"
  yq -o=json '.' "${fixture}/eviction.yaml" | jq "${resource_change}" >"${dir}/eviction.yaml"
  resource="$(jq -r '.metadata.name' "${dir}/eviction.yaml")"
  namespace="$(jq -r '.metadata.namespace' "${dir}/eviction.yaml")"
  jq "${change}" "${work}/base-values.json" >"${dir}/values.json"
  cp "${policy}" "${dir}/validator.yaml"
  cp "${mutation_policy}" "${dir}/mutator.yaml"
  selected_policies='["validator.yaml","mutator.yaml"]'
  if [[ "${mode}" == validation-only ]]; then
    selected_policies='["validator.yaml"]'
  fi
  jq --argjson policies "${selected_policies}" --arg result "${result}" --arg resource "${resource}" \
    '.policies=$policies | .variables="values.json" | .results[0].result=$result | .results[0].resources=[$resource]' \
    "${work}/base-test.json" >"${dir}/kyverno-test.yaml"
  jq --arg resource "${resource}" --arg namespace "${namespace}" \
    '.globalValues["request.name"]=$resource | .globalValues["request.namespace"]=$namespace' \
    "${dir}/values.json" >"${dir}/scoped-values.json"
  mv "${dir}/scoped-values.json" "${dir}/values.json"
  if [[ "${mode}" == mutation ]]; then
    jq '.deleteOptions.preconditions.uid="pod-uid"' "${dir}/eviction.yaml" >"${dir}/patched-eviction.yaml"
    jq --arg resource "${resource}" '.results += [{policy:"bind-elastic-longhorn-eviction-uid",rule:"bind-elastic-instance-manager-uid",resources:[$resource],kind:"Eviction",result:"pass",patchedResources:"patched-eviction.yaml"}]' \
      "${dir}/kyverno-test.yaml" >"${dir}/mutation-test.yaml"
    mv "${dir}/mutation-test.yaml" "${dir}/kyverno-test.yaml"
  fi
  if ! bash "${root}/scripts/validate-kyverno-fixture-evaluation.sh" "${dir}" >"${dir}/result.log" 2>&1; then
    cat "${dir}/result.log" >&2
    fail "native eviction admission: ${name}"
  fi
  count=$((count + 1))
}
prefix='.policies[0].rules[0].values'
run_case empty-owned-v2 pass '.'
run_case permanent-worker skip "${prefix}.targetNode.metadata.labels[\"ksail.io/autoscaled\"]=\"false\""
run_case fixed-migration-worker skip "${prefix}.targetNode.metadata.name=\"autoscale-longhorn-v2-0123456789abcdef\" | ${prefix}.storageNode.metadata.labels={\"storage.platform.devantler.tech/migration-enabled\":\"true\"}"
run_case unqualified-migration-name fail "${prefix}.targetNode.metadata.name=\"autoscale-longhorn-v2-0123456789abcdef\" | ${prefix}.replicas.resident=[{metadata:{uid:\"resident\"}}]"
run_case unrelated-pod skip '.' '.metadata.name="csi-attacher-example"'
run_case unrelated-namespace skip '.' '.metadata.namespace="other-namespace"'
run_case uid-injected pass '.' 'del(.deleteOptions)' mutation
run_case uid-required-by-validator fail '.' 'del(.deleteOptions)' validation-only
run_case supplied-uid-preserved fail '.' '.deleteOptions.preconditions.uid="another-pod"'
run_case supplied-empty-uid-refused fail '.' '.deleteOptions.preconditions.uid=""'
run_case replaced-between-admissions fail "${prefix}.targetPod.metadata.uid=\"replacement-pod\"" 'del(.deleteOptions)'
run_case unbound-pod fail "${prefix}.targetPod.metadata.uid=\"\""
run_case replaced-manager fail "${prefix}.instanceManager.metadata.uid=\"replacement\""
run_case wrong-owner fail "${prefix}.targetPod.metadata.ownerReferences[0].uid=\"other\""
run_case non-controller-owner fail "${prefix}.targetPod.metadata.ownerReferences[0].controller=false"
run_case extra-owner fail "${prefix}.targetPod.metadata.ownerReferences += [{kind:\"Other\",uid:\"other\"}]"
run_case terminating-pod fail "${prefix}.targetPod.metadata.deletionTimestamp=\"2026-11-15T12:00:00Z\""
run_case terminating-manager fail "${prefix}.instanceManager.metadata.deletionTimestamp=\"2026-11-15T12:00:00Z\""
run_case pod-node-mismatch fail "${prefix}.targetPod.spec.nodeName=\"another-node\""
run_case manager-node-mismatch fail "${prefix}.instanceManager.spec.nodeID=\"another-node\""
run_case storage-node-mismatch fail "${prefix}.storageNode.metadata.name=\"another-node\""
run_case unfenced fail "${prefix}.targetNode.spec.taints=[]"
run_case replaced-node-fence fail "${prefix}.targetNode.metadata.uid=\"replacement\""
run_case unready fail "${prefix}.targetNode.status.conditions[0].status=\"False\""
run_case placement-open fail "${prefix}.storageNode.spec.allowScheduling=true"
run_case eviction-unrequested fail "${prefix}.storageNode.spec.evictionRequested=false"
run_case unknown-engine fail "${prefix}.instanceManager.spec.dataEngine=\"unknown\""
run_case manager-starting fail "${prefix}.instanceManager.status.currentState=\"starting\""
run_case wrong-eviction-precondition fail '.globalValues["request.object.deleteOptions.preconditions.uid"]="another-pod"'
for field in instanceEngines instanceEngineFrontends instanceReplicas instanceShards instanceShardGroups backingImages; do
  run_case "process-${field}" fail "${prefix}.instanceManager.status.${field}={present:{}}"
done
for collection in replicas engines frontends shards shardgroups orphans; do
  run_case "resident-${collection}" fail "${prefix}.${collection}.resident=[{metadata:{uid:\"resident\"}}]"
  run_case "incomplete-${collection}" fail "${prefix}.${collection}.complete=false"
  run_case "missing-${collection}" fail "del(${prefix}.${collection}.resident)"
done

# Exercise the policy's actual list projections independently of context mocks.
# An empty filtered result alone must not turn a partial list into absence.
query_json() {
  local input="$1" expression="$2" output="$3" heading
  kyverno jp query -c -i "${input}" "${expression}" >"${output}.raw"
  IFS= read -r heading <"${output}.raw"
  [[ "${heading}" == "# ${expression}" ]] || fail 'native query output did not retain its expected header'
  tail -n +2 "${output}.raw" >"${output}"
  jq -se 'length == 1 and (.[0] | type == "object")' "${output}" >/dev/null || fail 'native projection did not return one complete JSON object'
}
for collection in replicas engines frontends shards shardgroups orphans; do
  query="$(COLLECTION="${collection}" yq -r '.spec.rules[0].context[] | select(.name == strenv(COLLECTION)) | .apiCall.jmesPath' "${policy}")"
  query="${query//\{\{ targetPod.spec.nodeName \}\}/autoscale-cx53-0123456789abcdef}"
  query="${query//\{\{ request.name \}\}/instance-manager-example}"
  case "${collection}" in
    replicas) list_kind=ReplicaList ;;
    engines) list_kind=EngineList ;;
    frontends) list_kind=EngineFrontendList ;;
    shards) list_kind=ShardList ;;
    shardgroups) list_kind=ShardGroupList ;;
    orphans) list_kind=OrphanList ;;
  esac
  jq -n --arg kind "${list_kind}" '{apiVersion:"longhorn.io/v1beta2",kind:$kind,metadata:{resourceVersion:"17"},items:[]}' >"${work}/collection.json"
  query_json "${work}/collection.json" "${query}" "${work}/projection.json"
  jq -e '.complete == true and .resident == []' "${work}/projection.json" >/dev/null || fail "${collection}: complete empty list refused"
  for mutation in '.metadata.continue="next-page"' 'del(.metadata.resourceVersion)' 'del(.items)' 'del(.apiVersion)' '.kind="AnotherList"'; do
    jq "${mutation}" "${work}/collection.json" >"${work}/partial.json"
    query_json "${work}/partial.json" "${query}" "${work}/projection.json"
    jq -e '.complete == false' "${work}/projection.json" >/dev/null || fail "${collection}: partial list admitted"
  done
  jq '.items=[{metadata:{name:"resident",namespace:"longhorn-system",uid:"resident",resourceVersion:"18"},spec:{nodeID:"autoscale-cx53-0123456789abcdef"}}]' "${work}/collection.json" >"${work}/resident.json"
  query_json "${work}/resident.json" "${query}" "${work}/projection.json"
  jq -e '.complete == true and (.resident|length) == 1' "${work}/projection.json" >/dev/null || fail "${collection}: target reference missed"
  for mutation in '.items[0].metadata.uid=""' 'del(.items[0].metadata.resourceVersion)' '.items[0].metadata.namespace="other"' '.items += [.items[0] | .metadata.name="duplicate-uid"]' '.items += [.items[0] | .metadata.uid="duplicate-name"]'; do
    jq "${mutation}" "${work}/resident.json" >"${work}/partial.json"
    query_json "${work}/partial.json" "${query}" "${work}/projection.json"
    jq -e '.complete == false' "${work}/projection.json" >/dev/null || fail "${collection}: ambiguous native identities admitted"
  done
  if [[ "${collection}" == orphans ]]; then
    jq '.items[0].spec.nodeID="different" | .items[0].spec.parameters.InstanceManager="instance-manager-example"' "${work}/resident.json" >"${work}/manager-reference.json"
    jq 'del(.items[0].spec.nodeID)' "${work}/resident.json" >"${work}/partial.json"
    query_json "${work}/partial.json" "${query}" "${work}/projection.json"
    jq -e '.complete == false' "${work}/projection.json" >/dev/null || fail 'an unplaceable orphan was accepted'
  else
    jq '.items[0].spec.nodeID="different" | .items[0].status.instanceManagerName="instance-manager-example"' "${work}/resident.json" >"${work}/manager-reference.json"
  fi
  query_json "${work}/manager-reference.json" "${query}" "${work}/projection.json"
  jq -e '.complete == true and (.resident|length) == 1' "${work}/projection.json" >/dev/null || fail "${collection}: retained manager reference missed"
done

# Both component states use the actual Kustomize binary. Production must remain
# off; adding the component to an isolated overlay must produce the real rule.
kubectl kustomize "${root}/k8s/providers/hetzner/infrastructure" >"${work}/production.yaml"
kubectl kustomize "${root}/k8s/providers/hetzner/infrastructure/controllers" >"${work}/controllers.yaml"
yq ea -o=json '[.]' "${work}/production.yaml" "${work}/controllers.yaml" >"${work}/production.json"
jq -e '[.[] | select(.metadata.name == "protect-elastic-longhorn-eviction" or .metadata.name == "bind-elastic-longhorn-eviction-uid" or .metadata.name == "kyverno-elastic-longhorn-reader" or .metadata.name == "kyverno:admission-controller:read-elastic-worker")] | length == 0' \
  "${work}/production.json" >/dev/null || fail 'the inactive guard unexpectedly affects production'
mkdir "${work}/enabled"
jq -n '{apiVersion:"kustomize.config.k8s.io/v1beta1",kind:"Kustomization",components:["../../../k8s/bases/components/longhorn-elastic-eviction-guard","../../../k8s/bases/components/longhorn-elastic-eviction-reader"]}' >"${work}/enabled/kustomization.yaml"
kubectl kustomize "${work}/enabled" >"${work}/enabled.yaml"
yq ea -o=json '[.]' "${work}/enabled.yaml" >"${work}/enabled.json"
jq -e 'length == 5 and
  ([.[]|select(.kind == "ClusterPolicy" and .metadata.name == "protect-elastic-longhorn-eviction")|
    .spec.background == false and .spec.webhookConfiguration.failurePolicy == "Fail" and
    .spec.rules[0].match.any[0].resources.kinds == ["Pod/eviction"] and
    .spec.rules[0].validate.failureAction == "Enforce"] == [true]) and
  ([.[]|select(.kind == "ClusterPolicy" and .metadata.name == "bind-elastic-longhorn-eviction-uid")|
    .spec.background == false and .spec.webhookConfiguration.failurePolicy == "Fail" and
    .spec.rules[0].match.any[0].resources.kinds == ["Pod/eviction"]] == [true]) and
  ([.[]|select(.kind == "Role")|.metadata.namespace == "longhorn-system" and ([.rules[].verbs[]]|unique|sort) == ["get","list"]] == [true]) and
  ([.[]|select(.kind == "ClusterRole")|.metadata.labels["rbac.kyverno.io/aggregate-to-admission-controller"] == "true" and .rules == [{apiGroups:[""],resources:["nodes"],verbs:["get"]}]] == [true]) and
  ([.[]|select(.kind == "RoleBinding")|.metadata.namespace == "longhorn-system" and .subjects == [{kind:"ServiceAccount",name:"kyverno-admission-controller",namespace:"kyverno"}]] == [true])' \
  "${work}/enabled.json" >/dev/null || fail 'the reader render widened or lost its scoped read permissions'
completed=1
printf 'PASS: %s native eviction cases, complete collection projections, and both component states\n' "${count}"
