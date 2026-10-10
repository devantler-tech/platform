#!/usr/bin/env bash
# Protected-main-only challenge; never reads an App key, Secret or runner JIT data.
set -Eeuo pipefail
umask 077
fail() { printf 'ARC transport: FAIL at %s\n' "$1" >&2; exit 1; }
[[ "$#" == 1 && "$1" == --same-node ]] || fail arguments
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform &&
   "${GITHUB_REF:-}" == refs/heads/main && "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ &&
   "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]{0,19}$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]{0,2}$ ]] || fail identity
current_main() { [[ "$(timeout 20s gh api repos/devantler-tech/platform/branches/main --jq .commit.sha 2>/dev/null)" == "$GITHUB_SHA" ]] || fail moved-main; }
current_main
readonly context=admin@prod namespace=arc-runners host=openbao-arc.openbao.svc.cluster.local
readonly label=platform.devantler.tech/arc-transport-probe
readonly image=ghcr.io/devantler-tech/ksail-analysis-runner@sha256:1ab01644fd6f67e0b1ab0b456e10b78f58be92ac67e5e06d103cd8c12bbb119b
scratch=$(mktemp -d)
readonly scratch
probe='' uid='' created=false forward_pid=''
stage=initialization
kc() { timeout 35s kubectl --context "$context" --request-timeout=30s "$@" 2>"$scratch/kubectl-error"; }
quiet() { "$@" >"$scratch/command-output" 2>"$scratch/command-error"; }
identity() { jq -ce '{uid:.metadata.uid,ip:.status.podIP,node:.spec.nodeName,containers:[.status.containerStatuses[]|{name,containerID,restartCount,ready}]}'; }
cleanup_probe() {
  local now
  "$created" || return 0
  [[ -n "$uid" ]] || return 1
  now=$(kc -n "$namespace" get pod "$probe" --ignore-not-found -o json) || return 1
  if [[ -n "$now" ]]; then
    jq -e --arg uid "$uid" --arg key "$label" --arg owner "$probe" \
      '.metadata.uid == $uid and .metadata.labels[$key] == $owner' <<<"$now" >/dev/null || return 1
    jq -n --arg uid "$uid" '{apiVersion:"v1",kind:"DeleteOptions",preconditions:{uid:$uid},gracePeriodSeconds:5}' >"$scratch/delete.json"
    quiet kc delete --raw="/api/v1/namespaces/$namespace/pods/$probe" -f "$scratch/delete.json" || return 1
    quiet kc -n "$namespace" wait "pod/$probe" --for=delete --timeout=25s || return 1
  fi
  now=$(kc -n "$namespace" get pod "$probe" --ignore-not-found -o json) || return 1
  [[ -z "$now" ]] || return 1
  kc -n "$namespace" get pods -l "$label=$probe" -o json | jq -e '.items | length == 0' >/dev/null || return 1
  created=false
}
cleanup() {
  local result=$? failed=0
  trap - EXIT ERR INT TERM
  if [[ -n "$forward_pid" ]]; then kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; fi
  cleanup_probe || failed=1
  rm -rf "$scratch"
  if [[ "$failed" != 0 ]]; then printf 'ARC transport: FAIL cleanup\n' >&2; exit 4; fi
  printf 'ARC transport: owned probe cleanup verified\n'
  exit "$result"
}
trap cleanup EXIT
trap 'printf "ARC transport: FAIL at %s\n" "$stage" >&2' ERR
trap 'exit 143' INT TERM
go build -trimpath -o "$scratch/evidence" ./scripts/verify-arc-transport >"$scratch/build-output" 2>"$scratch/build-error"

policy_spec() {
  # Kyverno v1.19.1 ClusterPolicy CRD defaults, also observed from this API.
  # Add only absent defaults to the source; retain explicit values and unknown
  # live fields so the complete spec comparison still rejects drift.
  yq -o=json '.spec' "$1" | jq '
    {admission:true,emitWarning:false,validationFailureAction:"Audit"} + . |
    .rules |= map({skipBackgroundRequests:true} + . |
      if has("validate") then .validate = ({allowExistingViolations:true} + .validate) else . end)'
}

public_listener_boundary() {
  kc get clusterpolicy restrict-arc-openbao-listener -o json >"$scratch/listener-policy.json"
  policy_spec k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener.yaml >"$scratch/listener-spec.json"
  jq -e --slurpfile expected "$scratch/listener-spec.json" '.spec == $expected[0] and
    any(.status.conditions[]; .type == "Ready" and .status == "True")' "$scratch/listener-policy.json" >/dev/null
  kc -n openbao get configmap arc-openbao-listener -o json >"$scratch/listener.json"
  yq -o=json '.data' k8s/providers/hetzner/infrastructure/controllers/openbao/transport/config-map-listener.yaml >"$scratch/listener-data.json"
  jq -e --slurpfile expected "$scratch/listener-data.json" '.metadata.name == "arc-openbao-listener" and
    .metadata.namespace == "openbao" and (.metadata.resourceVersion | type) == "string" and
    (.metadata.resourceVersion | length) > 0 and .data == $expected[0] and
    ((.binaryData // {}) | length) == 0' "$scratch/listener.json" >/dev/null
  quiet kc replace --dry-run=server -f "$scratch/listener.json"
  for change in '.data["listener.hcl"] += "synthetic PRIVATE KEY content\n"' \
    '.data.password = "synthetic"' '.binaryData = {credential:"c3ludGhldGlj"}'; do
    jq "$change" "$scratch/listener.json" >"$scratch/credential-negative.json"
    if kc replace --dry-run=server -f "$scratch/credential-negative.json" >"$scratch/admission-output" 2>"$scratch/admission-error"; then
      fail public-listener-admitted-credential
    fi
    grep -Fq restrict-arc-openbao-listener "$scratch/kubectl-error" || fail public-listener-admission-unknown
    grep -Fq public-listener-settings-only "$scratch/kubectl-error" || fail public-listener-admission-unknown
  done
}

stage=inactive-credential-boundary
public_listener_boundary
jq -Sc '{uid:.metadata.uid,spec:.spec}' "$scratch/listener-policy.json" >"$scratch/listener-policy-before"
jq -Sc '{uid:.metadata.uid,data:.data,binaryData:.binaryData}' "$scratch/listener.json" >"$scratch/listener-before"
# Inspect only metadata counts: neither existing App credentials nor JIT data is read.
kc -n "$namespace" get externalsecrets -o json | jq -e '.items | length == 0' >/dev/null
kc -n "$namespace" get helmreleases -o json | jq -e 'all(.items[]; .spec.suspend == true)' >/dev/null
kc -n "$namespace" get pods -l platform.devantler.tech/arc-role=runner -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' >"$scratch/runners"
[[ ! -s "$scratch/runners" ]] || fail active-runner
kc -n "$namespace" get pods -l "$label" -o json | jq -e '.items | length == 0' >/dev/null
kc -n "$namespace" get secretstore openbao -o json >"$scratch/store.json"
jq -e --arg server "https://$host:8204" '.spec.provider.vault.server == $server and
 .spec.provider.vault.caProvider == {type:"ConfigMap",name:"arc-openbao-ca",key:"ca.crt"} and
 .spec.provider.vault.path == "secret" and .spec.provider.vault.version == "v2" and
 .spec.provider.vault.auth.kubernetes == {mountPath:"kubernetes",role:"arc-secret-reader",serviceAccountRef:{name:"arc-secret-reader"}}' "$scratch/store.json" >/dev/null
kc -n "$namespace" get configmap arc-openbao-ca -o json >"$scratch/ca.json"
jq -er '.data["ca.crt"] | select(startswith("-----BEGIN CERTIFICATE-----"))' "$scratch/ca.json" >"$scratch/ca.pem"
kc get clusterpolicy restrict-arc-openbao-certificate -o json >"$scratch/policy.json"
policy_spec k8s/providers/hetzner/infrastructure/cluster-policies/restrict-arc-openbao-certificate.yaml >"$scratch/issuance-spec.json"
jq -e --slurpfile expected "$scratch/issuance-spec.json" '.spec == $expected[0] and
 any(.status.conditions[]; .type == "Ready" and .status == "True")' "$scratch/policy.json" >/dev/null
for layer in infrastructure infrastructure-controllers; do
  kc -n flux-system get kustomization "$layer" -o json | jq -ce 'select(.status.observedGeneration == .metadata.generation and
    any(.status.conditions[]; .type == "Ready" and .status == "True")) | .status.lastAppliedRevision | select(test("^latest@sha256:[0-9a-f]{64}$"))' >"$scratch/$layer-revision"
done
cmp -s "$scratch/infrastructure-revision" "$scratch/infrastructure-controllers-revision" || fail split-revision

stage=healthy-bao-pod
kc -n openbao get pod openbao-2 -o json >"$scratch/bao.json"
jq -e '.metadata.uid != null and .status.podIP != null and .spec.nodeName != null and
 any(.status.conditions[]; .type == "Ready" and .status == "True") and
 (.status.containerStatuses | length) == 2 and all(.status.containerStatuses[]; .ready == true and .containerID != "") and
 any(.spec.containers[]; .name == "arc-tls-reload")' "$scratch/bao.json" >/dev/null
bao_ip=$(jq -er '.status.podIP' "$scratch/bao.json")
bao_node=$(jq -er '.spec.nodeName' "$scratch/bao.json")
# Pod labels are not necessarily Cilium identity labels. Bind the retained
# transport selector to the current Pod before claiming policy isolation.
stage=healthy-bao-identity
kc -n openbao get ciliumendpoint openbao-2 -o json >"$scratch/bao-identity.json"
jq -e --arg uid "$(jq -er '.metadata.uid' "$scratch/bao.json")" '
 any(.metadata.ownerReferences[]; .kind == "Pod" and .uid == $uid) and
 (.status.identity.id | type == "number" and . > 0) and
 (.status.identity.labels as $labels |
  all(["k8s:app.kubernetes.io/name=openbao", "k8s:app.kubernetes.io/instance=openbao",
       "k8s:io.kubernetes.pod.namespace=openbao", "k8s:platform.devantler.tech/arc-transport=tls"][];
      . as $label | $labels | index($label) != null))' "$scratch/bao-identity.json" >/dev/null
stage=healthy-bao-service
kc -n openbao get service openbao-arc -o json | jq -e '.spec.selector == {"app.kubernetes.io/name":"openbao","app.kubernetes.io/instance":"openbao","statefulset.kubernetes.io/pod-name":"openbao-2"} and
 .spec.ports == [{name:"arc-tls",port:8204,protocol:"TCP",targetPort:8204}]' >/dev/null
stage=healthy-eso-pods
kc -n external-secrets get pods -l app.kubernetes.io/name=external-secrets -o json >"$scratch/eso.json"
jq -e '(.items | length) > 0 and all(.items[]; .metadata.uid != null and .spec.nodeName != null and
 any(.status.conditions[]; .type == "Ready" and .status == "True") and
 all(.status.containerStatuses[]; .ready == true and .containerID != ""))' "$scratch/eso.json" >/dev/null
{ printf '%s\n' "$bao_node"; jq -r '.items[].spec.nodeName' "$scratch/eso.json"; } | sort -u >"$scratch/nodes"
[[ "$(wc -l <"$scratch/nodes")" -le 4 ]] || fail exposure-bound
stage=healthy-health-forward
# Positive health controls use loopback-only tunnels to this exact Pod. The
# API-server proxy is not an allowed HTTP client under OpenBao's network policy.
# Direct same-node HTTP/TLS denial challenges below remain unchanged.
kubectl --context "$context" --request-timeout=30s -n openbao port-forward --address=127.0.0.1 pod/openbao-2 :8204 :8200 >"$scratch/forward" 2>"$scratch/forward-error" &
forward_pid=$!
for _ in {1..30}; do
  kill -0 "$forward_pid" 2>/dev/null || fail port-forward
  port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> 8204$/\1/p' "$scratch/forward")
  http_port=$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) -> 8200$/\1/p' "$scratch/forward")
  [[ "$port" =~ ^[0-9]+$ && "$http_port" =~ ^[0-9]+$ ]] && break
  sleep 1
done
[[ "$port" =~ ^[0-9]+$ && "$http_port" =~ ^[0-9]+$ ]] || fail port-forward
healthy() {
  stage=healthy-tls-health
  curl --noproxy '*' --proto '=https' --tlsv1.2 --cacert "$scratch/ca.pem" --resolve "$host:$port:127.0.0.1" \
    --fail --silent --show-error --max-time 15 "https://$host:$port/v1/sys/health?standbyok=true" >"$scratch/tls-health" 2>"$scratch/tls-error"
  "$scratch/evidence" health <"$scratch/tls-health"
  stage=healthy-http-health
  curl --noproxy '*' --proto '=http' --fail --silent --show-error --max-time 15 \
    "http://127.0.0.1:$http_port/v1/sys/health?standbyok=true" >"$scratch/http-health" 2>"$scratch/http-error"
  "$scratch/evidence" health <"$scratch/http-health"
  stage=healthy-health-binding
  [[ "$(jq -r .cluster_id "$scratch/tls-health")" == "$(jq -r .cluster_id "$scratch/http-health")" ]] || fail split-health
}
healthy
stage=healthy-cluster-info
cluster=$(kc -n kube-system get configmap cilium-config -o json | jq -er '.data["cluster-name"] | select(length > 0)')
count=0
while IFS= read -r node; do
  stage=same-node-challenge
  count=$((count+1))
  probe="arc-transport-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$count"
  kc get node "$node" -o json >"$scratch/node.json"
  jq -e '.metadata.uid != null and .metadata.labels["platform.devantler.tech/ci-runner"] != "enabled" and
    any(.status.conditions[]; .type == "Ready" and .status == "True")' "$scratch/node.json" >/dev/null
  kc -n kube-system get pods -l k8s-app=cilium --field-selector "spec.nodeName=$node" -o json \
    | jq -e 'if (.items | length) == 1 then .items[0] else error("observer count") end |
      select(any(.status.conditions[]; .type == "Ready" and .status == "True"))' >"$scratch/agent.json"
  agent=$(jq -er '.metadata.name' "$scratch/agent.json")
  quiet kc -n kube-system exec "$agent" -c cilium-agent -- hubble status --server unix:///var/run/cilium/hubble.sock --timeout 5s --request-timeout 5s --output json
  jq -n --arg probe "$probe" --arg label "$label" --arg node "$node" --arg image "$image" \
    '{apiVersion:"v1",kind:"Pod",metadata:{name:$probe,namespace:"arc-runners",labels:{($label):$probe}},spec:{
    nodeName:$node,automountServiceAccountToken:false,restartPolicy:"Never",activeDeadlineSeconds:300,
    securityContext:{runAsNonRoot:true,runAsUser:1001,runAsGroup:1001,seccompProfile:{type:"RuntimeDefault"}},
    containers:[{name:"probe",image:$image,command:["/bin/sh","-ec","test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token; grep -Eq \u0027^CapEff:[[:space:]]+0+$\u0027 /proc/self/status; printf ready > /tmp/ready; sleep 260"],
    readinessProbe:{exec:{command:["/bin/sh","-ec","test -f /tmp/ready"]},periodSeconds:2},
    securityContext:{privileged:false,allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},
    resources:{requests:{cpu:"10m",memory:"32Mi"},limits:{cpu:"100m",memory:"128Mi"}},volumeMounts:[{name:"tmp",mountPath:"/tmp"}]}],
    volumes:[{name:"tmp",emptyDir:{sizeLimit:"16Mi"}}]}}' >"$scratch/pod.json"
  quiet kc create --dry-run=server -f "$scratch/pod.json"
  for negative in privileged hostpath hostnetwork hostpid netraw; do
    case "$negative" in
      privileged) jq '.spec.containers[0].securityContext.privileged=true' "$scratch/pod.json" >"$scratch/negative.json" ;;
      hostpath) jq '.spec.volumes += [{name:"host",hostPath:{path:"/"}}]' "$scratch/pod.json" >"$scratch/negative.json" ;;
      hostnetwork) jq '.spec.hostNetwork=true' "$scratch/pod.json" >"$scratch/negative.json" ;;
      hostpid) jq '.spec.hostPID=true' "$scratch/pod.json" >"$scratch/negative.json" ;;
      netraw) jq '.spec.containers[0].securityContext.capabilities.add=["NET_RAW"]' "$scratch/pod.json" >"$scratch/negative.json" ;;
    esac
    if quiet kc create --dry-run=server -f "$scratch/negative.json"; then fail negative-admission; fi
    grep -Eq 'violates PodSecurity.*restricted|admission webhook.*denied the request' "$scratch/kubectl-error" || fail admission-transport-failure
    case "$negative" in
      privileged) grep -qi privileged "$scratch/kubectl-error" ;; hostpath) grep -Eqi 'hostPath|host.path' "$scratch/kubectl-error" ;;
      hostnetwork) grep -qi hostNetwork "$scratch/kubectl-error" ;; netraw) grep -qi NET_RAW "$scratch/kubectl-error" ;;
      hostpid) grep -qi hostPID "$scratch/kubectl-error" ;;
    esac
  done
  created=true
  kc create -f "$scratch/pod.json" -o json | jq -er --arg name "$probe" --arg label "$label" \
    'select(.metadata.name == $name and .metadata.namespace == "arc-runners" and .metadata.labels[$label] == $name) |
    .metadata.uid | select(type == "string" and length > 0)' >"$scratch/uid"
  uid=$(cat "$scratch/uid")
  quiet timeout 130s kubectl --context "$context" --request-timeout=120s -n "$namespace" wait "pod/$probe" --for=condition=Ready --timeout=90s
  kc -n "$namespace" get pod "$probe" -o json >"$scratch/probe.json"
  jq -e --arg uid "$uid" --arg node "$node" --arg image "$image" \
    '.metadata.uid == $uid and .spec.nodeName == $node and .spec.automountServiceAccountToken == false and
    (.spec.hostNetwork // false) == false and (.spec.hostPID // false) == false and (.spec.hostIPC // false) == false and
    (.spec.containers | length) == 1 and .spec.containers[0].image == $image and
    .spec.containers[0].securityContext.capabilities.drop == ["ALL"] and (.spec.containers[0].securityContext.capabilities.add // [] | length) == 0 and
    .spec.containers[0].securityContext.privileged == false and .spec.containers[0].securityContext.allowPrivilegeEscalation == false and
    .spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    .spec.volumes == [{name:"tmp",emptyDir:{sizeLimit:"16Mi"}}] and (.spec.initContainers // [] | length) == 0 and
    (.spec.ephemeralContainers // [] | length) == 0 and (.spec.shareProcessNamespace // false) == false and
    .spec.securityContext.runAsNonRoot == true and .spec.securityContext.runAsUser == 1001 and
    .spec.securityContext.runAsGroup == 1001 and .spec.securityContext.seccompProfile.type == "RuntimeDefault" and
    (.spec.containers[0].env // [] | length) == 0 and (.spec.containers[0].envFrom // [] | length) == 0 and
    .spec.containers[0].volumeMounts == [{name:"tmp",mountPath:"/tmp"}] and
    (.status.containerStatuses | length) == 1 and .status.containerStatuses[0].ready == true and
    .status.containerStatuses[0].restartCount == 0 and .status.containerStatuses[0].containerID != ""' "$scratch/probe.json" >/dev/null
  source_ip=$(jq -er '.status.podIP' "$scratch/probe.json")
  for destination_port in 8200 8204; do
    healthy
    start=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
    scheme=http; [[ "$destination_port" == 8204 ]] && scheme=https
    if quiet kc -n "$namespace" exec "$probe" -c probe -- curl --noproxy '*' --connect-timeout 3 --max-time 5 \
      --silent --show-error --output /dev/null "$scheme://$bao_ip:$destination_port/v1/sys/health?standbyok=true"; then fail reachable-openbao; else result=$?; fi
    [[ "$result" == 28 ]] || fail unproven-network-timeout
    quiet kc -n "$namespace" exec "$probe" -c probe -- test -f /tmp/ready
    end=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
    kc -n kube-system exec "$agent" -c cilium-agent -- hubble observe --server unix:///var/run/cilium/hubble.sock --timeout 5s \
      --since "$start" --until "$end" --from-ip "$source_ip" --from-pod "$namespace/$probe" --to-ip "$bao_ip" \
      --to-port "$destination_port" --protocol tcp --verdict DROPPED --drop-reason-desc POLICY_DENIED --output jsonpb >"$scratch/flows"
    [[ ! -s "$scratch/kubectl-error" ]] || fail observer-diagnostics
    "$scratch/evidence" denial --pod "$probe" --source "$source_ip" --destination "$bao_ip" --port "$destination_port" \
      --node "$cluster/$node" --since "$start" --until "$end" <"$scratch/flows"
    healthy
  done
  for binding in 'arc-runners probe' 'kube-system agent' 'openbao bao'; do
    read -r ns object <<<"$binding"
    name=$probe; [[ "$object" == agent ]] && name=$agent; [[ "$object" == bao ]] && name=openbao-2
    identity <"$scratch/$object.json" >"$scratch/before"
    kc -n "$ns" get pod "$name" -o json | identity >"$scratch/after"
    cmp -s "$scratch/before" "$scratch/after" || fail workload-churn
  done
  kc get node "$node" -o json | jq -ce '{uid:.metadata.uid,ready:[.status.conditions[]|select(.type=="Ready")|.status]}' >"$scratch/after"
  jq -ce '{uid:.metadata.uid,ready:[.status.conditions[]|select(.type=="Ready")|.status]}' "$scratch/node.json" >"$scratch/before"
  cmp -s "$scratch/before" "$scratch/after" || fail node-churn
  cleanup_probe || { printf 'ARC transport: FAIL cleanup\n' >&2; exit 4; }
done <"$scratch/nodes"
stage=final-bindings
public_listener_boundary
jq -Sc '{uid:.metadata.uid,spec:.spec}' "$scratch/listener-policy.json" >"$scratch/after"
cmp -s "$scratch/listener-policy-before" "$scratch/after" || fail listener-policy-churn
jq -Sc '{uid:.metadata.uid,data:.data,binaryData:.binaryData}' "$scratch/listener.json" >"$scratch/after"
cmp -s "$scratch/listener-before" "$scratch/after" || fail listener-configuration-churn
for object in 'secretstore openbao' 'configmap arc-openbao-ca'; do
  read -r kind name <<<"$object"
  kc -n "$namespace" get "$kind" "$name" -o json | jq -Sc '{uid:.metadata.uid,spec:.spec,data:.data}' >"$scratch/after"
  original=store; [[ "$kind" == configmap ]] && original=ca
  jq -Sc '{uid:.metadata.uid,spec:.spec,data:.data}' "$scratch/$original.json" >"$scratch/before"
  cmp -s "$scratch/before" "$scratch/after" || fail transport-churn
done
kc get clusterpolicy restrict-arc-openbao-certificate -o json | jq -Sc '{uid:.metadata.uid,spec:.spec}' >"$scratch/after"
jq -Sc '{uid:.metadata.uid,spec:.spec}' "$scratch/policy.json" >"$scratch/before"
cmp -s "$scratch/before" "$scratch/after" || fail issuance-policy-churn
kc -n external-secrets get pods -l app.kubernetes.io/name=external-secrets -o json | jq -Sc '[.items[]|{uid:.metadata.uid,ip:.status.podIP,node:.spec.nodeName,containers:.status.containerStatuses}] | sort_by(.uid)' >"$scratch/after"
jq -Sc '[.items[]|{uid:.metadata.uid,ip:.status.podIP,node:.spec.nodeName,containers:.status.containerStatuses}] | sort_by(.uid)' "$scratch/eso.json" >"$scratch/before"
cmp -s "$scratch/before" "$scratch/after" || fail eso-churn
for layer in infrastructure infrastructure-controllers; do
  kc -n flux-system get kustomization "$layer" -o json | jq -ce 'select(.status.observedGeneration == .metadata.generation and
    any(.status.conditions[]; .type == "Ready" and .status == "True")) | .status.lastAppliedRevision' >"$scratch/after"
  cmp -s "$scratch/$layer-revision" "$scratch/after" || fail revision-churn
done
current_main
printf 'PASS: verified native TLS health and %s same-node HTTP/TLS policy-denial pairs; no App key read\n' "$count"
