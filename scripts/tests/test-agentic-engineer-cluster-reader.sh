#!/usr/bin/env bash
# Prove the kagent lane's view of the cluster is read-only and reachable only the intended way.
#
# A run's sandbox has no Kubernetes credential and no route to the API. Its live-health check reads
# the cluster through the lane's cluster-reader proxy instead: `kubectl proxy`, running as a service
# account that may only get and list what that check reads, reachable only from Substrate's egress
# gateway, and named by the one kubeconfig the harness image carries. This renders the lane and the
# Substrate base and checks every link, then runs the proxy's own image with its own arguments and
# proves it refuses every method but GET and HEAD, any other Host, and exec, before the API sees it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
container=''
completed=0
# Remove the proxy container and the work directory on exit, and fail any exit that did not reach
# the PASS.
cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm -f "${container}" >/dev/null 2>&1 || true
  fi
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer cluster reader: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer cluster reader: %s\n' "$1" >&2
  exit 1
}
for tool in curl docker jq kubectl yq; do
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

account="$(doc lane.yaml ServiceAccount cluster-reader)"
cluster_role="$(doc lane.yaml ClusterRole agentic-engineer-cluster-reader)"
role="$(doc lane.yaml Role agentic-engineer-cluster-reader)"
deployment="$(doc lane.yaml Deployment cluster-reader)"
service="$(doc lane.yaml Service cluster-reader)"
network="$(doc lane.yaml CiliumNetworkPolicy allow-cluster-reader)"
policy="$(doc lane.yaml EgressPolicy cluster-reader)"
agent="$(doc lane.yaml Agent agentic-engineer)"
gateway="$(doc substrate.yaml CiliumNetworkPolicy allow-egress-gateway)"
for name in account cluster_role role deployment service network policy agent gateway; do
  [[ -n "${!name}" ]] || fail "the ${name//_/ } is not rendered"
done

# Exactly what the live-health check reads, with get and list only: no Secrets, ConfigMaps, logs,
# wildcards or writes.
want_cluster='[
  {"apiGroups":[""],"resources":["pods"],"verbs":["get","list"]},
  {"apiGroups":["gateway.networking.k8s.io"],"resources":["httproutes"],"verbs":["get","list"]},
  {"apiGroups":["helm.toolkit.fluxcd.io"],"resources":["helmreleases"],"verbs":["get","list"]},
  {"apiGroups":["kustomize.toolkit.fluxcd.io"],"resources":["kustomizations"],"verbs":["get","list"]},
  {"apiGroups":["source.toolkit.fluxcd.io"],"resources":["gitrepositories","helmrepositories","ocirepositories"],"verbs":["get","list"]}
]'
# rules <json>: a role's rules, each list sorted and the rules sorted, for comparison.
rules() { jq -cS '[.rules[] | {apiGroups: (.apiGroups | sort), resources: (.resources | sort), verbs: (.verbs | sort)}
  | select(keys | length > 0)] | sort_by(.apiGroups, .resources)' <<<"$1"; }
[[ "$(rules "${cluster_role}")" == "$(jq -cS 'sort_by(.apiGroups, .resources)' <<<"${want_cluster}")" ]] ||
  fail "the cluster role grants more or other than the live-health reads: $(rules "${cluster_role}")"
[[ "$(q "${role}" '.metadata.namespace')" == flux-system ]] || fail 'the Flux controllers role is not in flux-system'
[[ "$(rules "${role}")" == '[{"apiGroups":["apps"],"resources":["deployments"],"verbs":["get","list"]}]' ]] ||
  fail "the flux-system role grants more than reading Deployments: $(rules "${role}")"
# The service account holds those two grants and nothing else in the lane.
bound="$(yq -o=json -I=0 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and
  ([.subjects[] | select(.kind == "ServiceAccount" and .name == "cluster-reader" and .namespace == "agentic-engineer")] | length) > 0)
  | .kind + "/" + .roleRef.kind + "/" + .roleRef.name' "${work}/lane.yaml" | tr -d '"' | sort | tr '\n' ' ')"
[[ "${bound}" == 'ClusterRoleBinding/ClusterRole/agentic-engineer-cluster-reader RoleBinding/Role/agentic-engineer-cluster-reader ' ]] ||
  fail "the cluster-reader account is bound to something else: ${bound}"
for binding in ClusterRoleBinding RoleBinding; do
  subjects="$(q "$(doc lane.yaml "${binding}" agentic-engineer-cluster-reader)" '[.subjects[] | .kind + "/" + .namespace + "/" + .name] | join(" ")')"
  [[ "${subjects}" == 'ServiceAccount/agentic-engineer/cluster-reader' ]] || fail "the ${binding} binds more than the reader: ${subjects}"
done

[[ "$(q "${account}" '.metadata.namespace')" == agentic-engineer ]] || fail 'the cluster-reader account is not in the lane'

# The proxy: the reader's identity, kubectl proxy from a pinned image, the Service's own name as the
# only accepted Host, and every method but GET and HEAD refused.
[[ "$(q "${deployment}" '.spec.template.spec.serviceAccountName')" == cluster-reader ]] ||
  fail 'the proxy does not run as the cluster-reader account'
# The token is not mounted automatically: the proxy alone gets a projected, expiring one.
[[ "$(q "${deployment}" '.spec.template.spec.automountServiceAccountToken')" == false ]] ||
  fail 'the account token is mounted automatically'
[[ "$(q "${deployment}" '.spec.template.spec.volumes[] | select(.name == "api-access") | .projected.sources[0].serviceAccountToken.expirationSeconds')" == 3600 ]] ||
  fail 'the proxy has no projected, expiring token'
[[ "$(q "${deployment}" '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "api-access") | .mountPath')" == /var/run/secrets/kubernetes.io/serviceaccount ]] ||
  fail 'the projected token is not where kubectl reads it'
image="$(q "${deployment}" '.spec.template.spec.containers[0].image')"
[[ "${image}" =~ ^docker\.io/alpine/k8s:[^@]+@sha256:[0-9a-f]{64}$ ]] || fail "the proxy image is not digest-pinned: ${image}"
[[ "$(q "${deployment}" '.spec.template.spec.containers[0].command | join(" ")')" == kubectl ]] || fail 'the proxy is not kubectl'
args=()
while IFS= read -r arg; do args+=("${arg}"); done < <(q "${deployment}" '.spec.template.spec.containers[0].args[]')
[[ "${args[0]}" == proxy ]] || fail "the proxy does not run kubectl proxy: ${args[*]}"
port="$(q "${deployment}" '.spec.template.spec.containers[0].ports[] | select(.name == "http") | .containerPort')"
[[ " ${args[*]} " == *" --port=${port} "* && " ${args[*]} " == *' --address=0.0.0.0 '* ]] ||
  fail "the proxy does not listen on its container port ${port}: ${args[*]}"
host='cluster-reader.agentic-engineer.svc.cluster.local'
[[ " ${args[*]} " == *" --accept-hosts=^${host//./\\.}$ "* ]] || fail "the proxy does not accept exactly its Service's name: ${args[*]}"
[[ " ${args[*]} " == *' --reject-methods='* ]] || fail 'the proxy does not refuse write methods itself'
[[ " ${args[*]} " != *' --reject-paths='* && " ${args[*]} " != *' --disable-filter'* ]] ||
  fail 'the proxy drops its default refusal of exec and attach'
# expect <actual> <want> <reason>: fails with the reason unless actual is exactly want.
expect() { [[ "$1" == "$2" ]] || fail "$3 (got: $1)"; }
expect "$(q "${service}" '.spec.selector."app.kubernetes.io/name"')" cluster-reader 'the Service does not select the proxy'
expect "$(q "${service}" '.spec.ports[0].port')" "${port}" 'the Service does not serve the proxy port'
expect "$(q "${service}" '.spec.ports[0].targetPort')" http 'the Service does not lead to the proxy container port'

# Only the egress gateway reaches the proxy, and the proxy reaches only the API server.
expect "$(q "${network}" '.spec.endpointSelector.matchLabels."app.kubernetes.io/name"')" cluster-reader \
  'the proxy policy does not select the proxy'
expect "$(q "${network}" '.spec.enableDefaultDeny.ingress') $(q "${network}" '.spec.enableDefaultDeny.egress')" 'true true' \
  'the proxy policy is not deny-by-default both ways'
expect "$(q "${network}" '.spec.ingress | length') $(q "${network}" '.spec.ingress[0].fromEndpoints | length')" '1 1' \
  'the proxy takes traffic from more than one peer'
expect "$(q "${network}" '.spec.ingress[0].fromEndpoints[0].matchLabels | to_entries | map(.key + "=" + .value) | sort | join(",")')" \
  'app=atenet-egress,k8s:io.kubernetes.pod.namespace=ate-system' 'the proxy takes traffic from more than the egress gateway'
expect "$(q "${network}" '[.spec.ingress[0].toPorts[].ports[] | .port + "/" + .protocol] | join(",")')" "${port}/TCP" \
  'the proxy takes traffic on more than its port'
expect "$(q "${network}" '.spec.egress | length') $(q "${network}" '.spec.egress[0].toEntities | join(",")')" '1 kube-apiserver' \
  'the proxy reaches more than the API server'
expect "$(q "${network}" '[.spec.egress[0].toPorts[].ports[] | .port + "/" + .protocol] | sort | join(",")')" '443/TCP,6443/TCP' \
  'the proxy reaches the API server on other ports'
expect "$(q "${gateway}" '[.spec.egress[] | select(.toEndpoints[0].matchLabels."app.kubernetes.io/name" == "cluster-reader" and
  .toEndpoints[0].matchLabels."k8s:io.kubernetes.pod.namespace" == "agentic-engineer") | .toPorts[].ports[] | .port] | join(",")')" \
  "${port}" 'the egress gateway may not reach the proxy on its port'

# The agent's way there: one plaintext destination, no credential, named by the harness kubeconfig.
origin="http://${host}:${port}"
expect "$(q "${policy}" '[.spec.destinations[].origin] | join(" ")')" "${origin}" "the egress policy does not grant exactly ${origin}"
expect "$(q "${policy}" '[.spec.destinations[] | select(has("authentication"))] | length')" 0 'the egress policy sends a credential'
grep -qx cluster-reader <<<"$(q "${agent}" '.spec.egressPolicyRefs[].name')" || fail 'the Agent does not use the cluster-reader policy'
kubeconfig="${root}/scripts/kagent-claude-harness/kubeconfig"
expect "$(yq '.contexts | length' "${kubeconfig}") $(yq '.contexts[0].context.cluster' "${kubeconfig}")" '1 prod' \
  'the harness kubeconfig does not have exactly one context, on the cluster named prod'
expect "$(yq '.current-context' "${kubeconfig}")" "$(yq '.contexts[0].name' "${kubeconfig}")" 'the harness kubeconfig does not use its context'
expect "$(yq '.clusters | length' "${kubeconfig}") $(yq '.clusters[0].cluster.server' "${kubeconfig}")" "1 ${origin}" \
  "the harness kubeconfig does not point at ${origin}"
expect "$(yq -o=json -I=0 '.users' "${kubeconfig}")" '[{"name":"cluster-reader","user":{}}]' 'the harness kubeconfig carries a credential'
dockerfile="${root}/scripts/kagent-claude-harness/Dockerfile"
if ! grep -Fxq 'COPY --chmod=0644 kubeconfig /opt/kagent-lane/kubeconfig' "${dockerfile}" ||
  ! grep -Fxq 'ENV KUBECONFIG=/opt/kagent-lane/kubeconfig' "${dockerfile}"; then
  fail 'the harness image does not carry the kubeconfig as KUBECONFIG'
fi

# The proxy's own image and arguments refuse writes, other hosts and exec before the API sees them.
# Outside the cluster it needs a kubeconfig; this one points at a closed port, so a request it lets
# through fails upstream (not 403) and one it refuses gets 403.
mkdir -p "${work}/kube"
printf 'apiVersion: v1\nkind: Config\nclusters: [{name: c, cluster: {server: "http://127.0.0.1:9"}}]\nusers: [{name: u, user: {}}]\ncontexts: [{name: x, context: {cluster: c, user: u}}]\ncurrent-context: x\n' \
  >"${work}/kube/config"
chmod -R a+rX "${work}/kube"
container="cluster-reader-$$-${RANDOM}"
docker run -d --name "${container}" --user 65532:65532 --read-only --tmpfs /tmp --cap-drop ALL \
  --security-opt no-new-privileges --mount "type=bind,src=${work}/kube,dst=/kube,readonly" -e HOME=/tmp \
  -p "127.0.0.1::${port}" --entrypoint kubectl "${image}" "${args[@]}" --kubeconfig=/kube/config >/dev/null
url=''
for attempt in $(seq 1 30); do
  ports="$(docker port "${container}" "${port}/tcp" 2>/dev/null || true)"
  address="${ports%%$'\n'*}"
  if [[ -n "${address}" ]] && curl -sS -o /dev/null --max-time 5 -H "Host: ${host}:${port}" "http://${address}/" 2>/dev/null; then
    url="http://${address}"
    break
  fi
  sleep 1
done
[[ -n "${url}" ]] || fail "the proxy did not answer after ${attempt} tries: $(docker logs "${container}" 2>&1 | tail -c 600)"
# status <method> <path> [host]: the proxy's status for one request.
status() {
  local method=(-X "$1")
  if [[ "$1" == HEAD ]]; then method=(-I); fi
  curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "${method[@]}" -H "Host: ${3:-${host}:${port}}" "${url}$2" || true
}
for method in POST PUT PATCH DELETE OPTIONS PROPFIND; do
  [[ "$(status "${method}" /api/v1/namespaces/default/pods)" == 403 ]] || fail "the proxy lets ${method} through"
done
for method in GET HEAD; do
  got="$(status "${method}" /api/v1/pods)"
  [[ "${got}" != 403 && "${got}" != 000 ]] || fail "the proxy refuses a ${method} read (${got})"
done
[[ "$(status GET /api/v1/pods "elsewhere.example:${port}")" == 403 ]] || fail 'the proxy accepts another Host'
[[ "$(status GET /api/v1/namespaces/default/pods/x/exec)" == 403 ]] || fail 'the proxy lets exec through'

grep -Fq 'bash scripts/tests/test-agentic-engineer-cluster-reader.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer cluster reader: PASS\n'
