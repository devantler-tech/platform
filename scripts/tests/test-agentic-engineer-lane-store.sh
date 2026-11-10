#!/usr/bin/env bash
# Prove the Agentic Engineer lane's store is wired end to end, and reachable only as intended.
#
# A kagent run starts in a fresh sandbox, so the lane keeps its memory and transcripts in an
# in-cluster WebDAV store (devantler-tech/monorepo#4869, #4870). The agent reaches it only through
# Substrate's egress gateway, which swaps its placeholder for the store credential; the maintainer
# and the Agent Improver read it only through the Kubernetes API. This renders the lane and the
# Substrate base and checks every link: the EgressPolicy names the store's real address and the
# credential the store checks, the Agent uses it, the gateway may reach it, and nothing else may.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer lane store: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer lane store: %s\n' "$1" >&2
  exit 1
}
for tool in yq kubectl; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

kubectl kustomize "${root}/k8s/providers/hetzner/apps/agentic-engineer" >"${work}/lane.yaml" ||
  fail 'the lane does not render'
kubectl kustomize "${root}/k8s/bases/infrastructure/substrate" >"${work}/substrate.yaml" ||
  fail 'the Substrate base does not render'
# doc <file> <kind> <name>: one rendered document as JSON, or nothing.
doc() { yq -o=json -I=0 "select(.kind == \"$2\" and .metadata.name == \"$3\")" "${work}/$1"; }
# q <json> <expression>: one value from a document.
q() { yq -p=json -r "$2" <<<"$1"; }

deployment="$(doc lane.yaml Deployment lane-store)"
service="$(doc lane.yaml Service lane-store)"
reader_service="$(doc lane.yaml Service lane-store-reader)"
policy="$(doc lane.yaml EgressPolicy lane-store)"
secret="$(doc lane.yaml ExternalSecret lane-store)"
network="$(doc lane.yaml CiliumNetworkPolicy allow-lane-store)"
workers="$(doc lane.yaml CiliumNetworkPolicy allow-agentic-engineer)"
role="$(doc lane.yaml Role lane-store-reader)"
binding="$(doc lane.yaml RoleBinding lane-store-reader)"
agent="$(doc lane.yaml Agent agentic-engineer)"
template="$(doc lane.yaml AgentTemplate agentic-engineer)"
gateway="$(doc substrate.yaml CiliumNetworkPolicy allow-egress-gateway)"
for name in deployment service reader_service policy secret network workers role binding agent template gateway; do
  [[ -n "${!name}" ]] || fail "the ${name//_/ } is not rendered"
done

container() { q "${deployment}" ".spec.template.spec.containers[] | select(.name == \"$1\") | $2"; }
writer_port="$(container webdav '.ports[] | select(.name == "webdav") | .containerPort')"
reader_port="$(container reader '.ports[] | select(.name == "reader") | .containerPort')"
[[ "$(q "${service}" '.spec.ports[0].targetPort')" == webdav ]] || fail 'the store Service does not front the WebDAV listener'
[[ "$(q "${reader_service}" '.spec.ports[0].targetPort')" == reader ]] ||
  fail 'the reader Service does not front the read-only listener'
# The documented proxy path names no port, and the API server resolves that only to one unnamed
# port; a named port would answer 503 behind a grant that could never be used.
reader_ports="$(q "${reader_service}" '[.spec.ports[] | .name // ""] | join(",")')"
[[ "${reader_ports}" == "" ]] || fail "the reader Service port is named (${reader_ports}), so the proxy path cannot reach it"
service_port="$(q "${service}" '.spec.ports[0].port')"

# The agent's way in: the EgressPolicy names the Service's real address, the Agent uses the
# policy, and the system prompt tells the agent that address.
origin="http://lane-store.agentic-engineer.svc.cluster.local:${service_port}"
destinations="$(q "${policy}" '[.spec.destinations[].origin] | join(",")')"
[[ "${destinations}" == "${origin}" ]] || fail "the EgressPolicy does not name the store at ${origin} alone: ${destinations}"
grep -qx lane-store <<<"$(q "${agent}" '.spec.egressPolicyRefs[].name')" || fail 'the Agent does not use the store EgressPolicy'
grep -Fq "${origin}" <<<"$(q "${template}" '.spec.systemPrompt')" || fail "the system prompt does not give the store at ${origin}"

# One credential: the gateway sends the policy's key, the store checks the htpasswd key, and both
# come from the same generated password and user. ESO's htpasswd takes the hash as a third argument.
[[ "$(q "${policy}" '.spec.destinations[0].authentication.secretKeyRef.name')" == "$(q "${secret}" '.spec.target.name')" ]] ||
  fail 'the EgressPolicy does not use the store Secret'
key="$(q "${policy}" '.spec.destinations[0].authentication.secretKeyRef.key')"
[[ "$(q "${policy}" '.spec.destinations[0].authentication.prefix')" == 'Basic ' ]] || fail 'the gateway does not send Basic credentials'
[[ "$(q "${secret}" ".spec.target.template.data.${key}")" == '{{ printf "agentic-engineer:%s" .password | b64enc }}' ]] ||
  fail "the gateway's ${key} key is not the store user's Basic credential"
[[ "$(q "${secret}" '.spec.target.template.data.htpasswd')" == '{{ htpasswd "agentic-engineer" .password "bcrypt" }}' ]] ||
  fail 'the store does not check a bcrypt htpasswd entry for the same user'
grep -qx -- '--htpasswd=/etc/lane-store/htpasswd' <<<"$(container webdav '.args[]')" || fail 'the WebDAV listener does not require the credential'
mount="$(container webdav '.volumeMounts[] | select(.mountPath == "/etc/lane-store") | .name')"
source="$(q "${deployment}" ".spec.template.spec.volumes[] | select(.name == \"${mount}\") | .secret.secretName")"
[[ "${source}" == lane-store ]] || fail 'the WebDAV listener does not read the credential from the store Secret'

# The reader only reads.
grep -qx -- '--read-only' <<<"$(container reader '.args[]')" || fail 'the reader is not read-only'
[[ "$(container reader '.volumeMounts[] | select(.name == "data") | .readOnly')" == true ]] ||
  fail "the reader's data mount is writable"

# Network: deny by default, the gateway on the WebDAV port, the API server on the reader port,
# no egress, and the namespace's worker policy leaves the store alone.
[[ "$(q "${network}" '.spec.enableDefaultDeny.ingress')" == true && "$(q "${network}" '.spec.enableDefaultDeny.egress')" == true ]] ||
  fail 'the store policy does not deny by default'
[[ "$(q "${network}" '.spec.egress // [] | length')" == 0 ]] || fail 'the store may make outbound calls'
[[ "$(q "${network}" '.spec.ingress | length')" == 2 ]] || fail 'the store admits more than the gateway and the API server'
# Every rule is exactly one peer list and its ports, so no second selector, CIDR or port can widen
# a listener; the reader has no credential of its own.
shapes="$(q "${network}" '[.spec.ingress[] | keys | sort | join("+")] | sort | join(",")')"
[[ "${shapes}" == 'fromEndpoints+toPorts,fromEntities+toPorts' ]] || fail "an ingress rule admits other peers: ${shapes}"
[[ "$(q "${network}" '.spec.ingress[] | select(.fromEndpoints) | .fromEndpoints | length')" == 1 ]] ||
  fail 'the WebDAV rule admits more than one peer selector'
# rule_ports <peer key>: every port the rule with that peer list admits, as port/protocol.
rule_ports() { q "${network}" ".spec.ingress[] | select(.$1) | [.toPorts[].ports[] | (.port | tostring) + \"/\" + .protocol] | join(\",\")"; }
[[ "$(rule_ports fromEndpoints)" == "${writer_port}/TCP" ]] || fail "the WebDAV rule admits ports $(rule_ports fromEndpoints)"
[[ "$(rule_ports fromEntities)" == "${reader_port}/TCP" ]] || fail "the reader rule admits ports $(rule_ports fromEntities)"
gateway_peer="$(q "${network}" '.spec.ingress[] | select(.fromEndpoints) | .fromEndpoints[0].matchLabels | to_entries | map(.key + "=" + .value) | sort | join(",")')"
[[ "${gateway_peer}" == 'app=atenet-egress,k8s:io.kubernetes.pod.namespace=ate-system' ]] ||
  fail "the WebDAV port admits more than the egress gateway: ${gateway_peer}"
[[ "$(q "${network}" '.spec.ingress[] | select(.fromEntities) | .fromEntities | sort | join(",")')" == host,kube-apiserver,remote-node ]] ||
  fail 'the reader port admits more than the API server'
[[ "$(q "${workers}" '.spec.endpointSelector.matchExpressions[] | select(.key == "app.kubernetes.io/name" and .operator == "NotIn") | .values[]')" == lane-store ]] ||
  fail "the namespace's worker policy also selects the store"
[[ "$(q "${gateway}" ".spec.egress[] | select(.toEndpoints[0].matchLabels[\"app.kubernetes.io/name\"] == \"lane-store\") | .toPorts[0].ports[0].port")" == "${writer_port}" ]] ||
  fail 'the egress gateway may not reach the WebDAV port'

# The read grant: get on that one Service's proxy, for the maintainer only.
grant="$(q "${role}" '[.rules[] | (.resources | join(",")) + "|" + (.resourceNames | join(",")) + "|" + (.verbs | join(","))] | join(";")')"
[[ "${grant}" == 'services/proxy|lane-store-reader|get' ]] || fail "the reader Role grants more than get on the reader proxy: ${grant}"
holders="$(q "${binding}" '.roleRef.name + "|" + ([.subjects[].name] | join(","))')"
# shellcheck disable=SC2016 # Flux substitutes the maintainer's address after this render
[[ "${holders}" == 'lane-store-reader|oidc:${admin_email}' ]] || fail "the reader grant is not bound to the maintainer alone: ${holders}"

grep -Fq 'bash scripts/tests/test-agentic-engineer-lane-store.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer lane store: PASS\n'
