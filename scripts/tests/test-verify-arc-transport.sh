#!/usr/bin/env bash
# No credentials or cluster: execute the real orchestration and evidence parser
# against bounded API/exec fixtures, including failure and replacement races.
set -euo pipefail
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin" "$scratch/state"
go build -o "$scratch/evidence" ./scripts/verify-arc-transport
export PROOF_TEST_ROOT="$scratch"
readonly head=1111111111111111111111111111111111111111
export GITHUB_ACTIONS=true GITHUB_REPOSITORY=devantler-tech/platform GITHUB_REF=refs/heads/main GITHUB_SHA="$head" GITHUB_RUN_ID=12345 GITHUB_RUN_ATTEMPT=1
# Native Kyverno v1.19.1 admission adds both rule defaults. Keep the fixture
# independent of the verifier's expected-spec construction.
yq -o=json '.spec' k8s/providers/hetzner/infrastructure/cluster-policies/restrict-arc-openbao-certificate.yaml \
  | jq '.rules |= map(.skipBackgroundRequests=true | .validate.allowExistingViolations=true) |
      {spec:(. + {admission:true,emitWarning:false,validationFailureAction:"Audit"}),
      status:{conditions:[{type:"Ready",status:"True"}]}}' >"$scratch/policy.json"
yq -o=json '.spec' k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener.yaml \
  | jq '.rules |= map(.skipBackgroundRequests=true | .validate.allowExistingViolations=true) |
      {metadata:{uid:"policy-uid"},spec:(. + {admission:true,emitWarning:false,validationFailureAction:"Audit"}),
      status:{conditions:[{type:"Ready",status:"True"}]}}' >"$scratch/listener-policy.json"
yq -o=json '.' k8s/providers/hetzner/infrastructure/controllers/openbao/transport/config-map-listener.yaml \
  | jq '.metadata.uid="listener-uid" | .metadata.resourceVersion="123"' >"$scratch/listener.json"
readonly script="$PWD/scripts/verify-arc-transport.sh"
for file in k8s/providers/hetzner/infrastructure/cluster-policies/restrict-arc-openbao-certificate.yaml \
  k8s/bases/infrastructure/cluster-policies/best-practices/restrict-arc-openbao-listener.yaml \
  k8s/providers/hetzner/infrastructure/controllers/openbao/transport/config-map-listener.yaml; do
  mkdir -p "$scratch/source/$(dirname "$file")"
  cp "$file" "$scratch/source/$file"
  if [[ "$file" == *cluster-policies* ]]; then
    yq -i '.spec.rules[].skipBackgroundRequests = false | .spec.rules[].validate.allowExistingViolations = false' "$scratch/source/$file"
  fi
done
for policy in policy listener-policy; do
  jq '.spec.rules |= map(.skipBackgroundRequests=false | .validate.allowExistingViolations=false)' \
    "$scratch/$policy.json" >"$scratch/explicit-$policy.json"
done
cat >"$scratch/bin/gh" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "${GITHUB_SHA}"
MOCK
cat >"$scratch/bin/go" <<'MOCK'
#!/usr/bin/env bash
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == -o ]]; then cp "$PROOF_TEST_ROOT/evidence" "$2"; exit; fi
  shift
done
exit 1
MOCK
cat >"$scratch/bin/curl" <<'MOCK'
#!/usr/bin/env bash
if [[ "${PROOF_CASE:-}" == tls_error ]]; then exit 60; fi
if [[ "${PROOF_CASE:-}" == sealed ]]; then printf '{"initialized":true,"sealed":true,"version":"2.6.3","cluster_id":"fixture"}'; exit; fi
printf '{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"fixture"}'
MOCK
cat >"$scratch/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state="$PROOF_TEST_ROOT/state"
printf '%s\n' "$*" >>"$state/calls"
while [[ "$1" == --context* || "$1" == --request-timeout* ]]; do
  if [[ "$1" == --context ]]; then shift 2; else shift; fi
done
ns=''
if [[ "$1" == -n ]]; then ns=$2; shift 2; fi
args="$*"
case "$args" in
  'port-forward '*) printf 'Forwarding from 127.0.0.1:38204 -> 8204\n'; exec sleep 300 ;;
  'get externalsecrets '*) printf '{"items":[]}' ;;
  'get helmreleases '*) printf '{"items":[]}' ;;
  'get secretstore '*) jq -n '{spec:{provider:{vault:{server:"https://openbao-arc.openbao.svc.cluster.local:8204",caProvider:{type:"ConfigMap",name:"arc-openbao-ca",key:"ca.crt"},path:"secret",version:"v2",auth:{kubernetes:{mountPath:"kubernetes",role:"arc-secret-reader",serviceAccountRef:{name:"arc-secret-reader"}}}}}}}' ;;
  'get configmap arc-openbao-ca '*) printf '{"data":{"ca.crt":"-----BEGIN CERTIFICATE-----fixture"}}' ;;
  'get configmap cilium-config '*) printf '{"data":{"cluster-name":"prod"}}' ;;
  'get configmap arc-openbao-listener '*)
    if [[ "${PROOF_CASE:-}" == listener_content ]]; then jq '.data.password="synthetic"' "$PROOF_TEST_ROOT/listener.json"
    elif [[ "${PROOF_CASE:-}" == listener_churn && -e "$state/attempt" ]]; then jq '.metadata.uid="replacement-listener"' "$PROOF_TEST_ROOT/listener.json"
    else cat "$PROOF_TEST_ROOT/listener.json"; fi ;;
  'get clusterpolicy restrict-arc-openbao-listener '*)
    if [[ "${PROOF_CASE:-}" == listener_read_failure ]]; then printf 'private-error-sentinel\n' >&2; exit 17
    elif [[ "${PROOF_CASE:-}" == source_explicit_false ]]; then cat "$PROOF_TEST_ROOT/explicit-listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_audit ]]; then jq '.spec.rules[0].validate.failureAction="Audit"' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_background_drift ]]; then jq '.spec.rules[0].skipBackgroundRequests=false' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_existing_drift ]]; then jq '.spec.rules[0].validate.allowExistingViolations=false' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_unknown_field ]]; then jq '.spec.rules[0].unexpected=true' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_admission_disabled ]]; then jq '.spec.admission=false' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_failure_open ]]; then jq '.spec.failurePolicy="Ignore"' "$PROOF_TEST_ROOT/listener-policy.json"
    elif [[ "${PROOF_CASE:-}" == listener_override ]]; then jq '.spec.rules[0].validate.failureActionOverrides=[{action:"Audit",namespaces:["openbao"]}]' "$PROOF_TEST_ROOT/listener-policy.json"
    else cat "$PROOF_TEST_ROOT/listener-policy.json"; fi ;;
  'get clusterpolicy '*)
    if [[ "${PROOF_CASE:-}" == source_explicit_false ]]; then cat "$PROOF_TEST_ROOT/explicit-policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_admission_disabled ]]; then jq '.spec.admission=false' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_background_drift ]]; then jq '.spec.rules[0].skipBackgroundRequests=false' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_existing_drift ]]; then jq '.spec.rules[0].validate.allowExistingViolations=false' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_unknown_field ]]; then jq '.spec.rules[0].unexpected=true' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_exclusion ]]; then jq '.spec.rules[0].exclude={any:[{resources:{namespaces:["arc-runners"]}}]}' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_failure_open ]]; then jq '.spec.failurePolicy="Ignore"' "$PROOF_TEST_ROOT/policy.json"
    elif [[ "${PROOF_CASE:-}" == issuance_override ]]; then jq '.spec.rules[0].validate.failureActionOverrides=[{action:"Audit",namespaces:["arc-runners"]}]' "$PROOF_TEST_ROOT/policy.json"
    else cat "$PROOF_TEST_ROOT/policy.json"; fi ;;
  'get kustomization '*) printf '{"metadata":{"generation":1},"status":{"observedGeneration":1,"lastAppliedRevision":"latest@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","conditions":[{"type":"Ready","status":"True"}]}}' ;;
  'get service '*) printf '{"spec":{"selector":{"app.kubernetes.io/name":"openbao","app.kubernetes.io/instance":"openbao","statefulset.kubernetes.io/pod-name":"openbao-2"},"ports":[{"name":"arc-tls","port":8204,"targetPort":8204,"protocol":"TCP"}]}}' ;;
  'get node '*) printf '{"metadata":{"uid":"node-uid","labels":{}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}' ;;
  'get --raw '*) printf '{"initialized":true,"sealed":false,"version":"2.6.3","cluster_id":"fixture"}' ;;
  'get pods '*arc-role=runner*) : ;;
  'get pods '*arc-transport-probe*)
    if [[ "${PROOF_CASE:-}" == stale_probe && ! -e "$state/created" ]]; then printf '{"items":[{}]}'; else printf '{"items":[]}'; fi ;;
  'get pods '*external-secrets*) printf '{"items":[{"metadata":{"uid":"eso-uid"},"spec":{"nodeName":"node-a"},"status":{"podIP":"10.1.1.3","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"eso","ready":true,"restartCount":0,"containerID":"containerd://eso"}]}}]}' ;;
  'get pods '*k8s-app=cilium*) printf '{"items":[{"metadata":{"name":"cilium-a","uid":"agent-uid"},"spec":{"nodeName":"node-a"},"status":{"podIP":"10.1.1.4","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"cilium-agent","ready":true,"restartCount":0,"containerID":"containerd://agent"}]}}]}' ;;
  'get pod openbao-2 '*) printf '{"metadata":{"uid":"bao-uid"},"spec":{"nodeName":"node-a","containers":[{"name":"openbao"},{"name":"arc-tls-reload"}]},"status":{"podIP":"10.1.1.2","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"openbao","ready":true,"restartCount":0,"containerID":"containerd://bao"},{"name":"arc-tls-reload","ready":true,"restartCount":0,"containerID":"containerd://reload"}]}}' ;;
  'get ciliumendpoint openbao-2 '*)
    owner=bao-uid; [[ "${PROOF_CASE:-}" != identity_stale ]] || owner=replaced-bao-uid
    jq -n --arg owner "$owner" '{metadata:{ownerReferences:[{kind:"Pod",uid:$owner}]},status:{identity:{id:123,labels:["k8s:app.kubernetes.io/name=openbao","k8s:app.kubernetes.io/instance=openbao","k8s:io.kubernetes.pod.namespace=openbao","k8s:platform.devantler.tech/arc-transport=tls"]}}}' |
      if [[ "${PROOF_CASE:-}" == identity_ordinal_only ]]; then jq '.status.identity.labels |= map(select(. != "k8s:platform.devantler.tech/arc-transport=tls"))'; else cat; fi ;;
  'get pod cilium-a '*) printf '{"metadata":{"uid":"agent-uid"},"spec":{"nodeName":"node-a"},"status":{"podIP":"10.1.1.4","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"cilium-agent","ready":true,"restartCount":0,"containerID":"containerd://agent"}]}}' ;;
  'get pod arc-transport-'*)
    [[ -e "$state/created" ]] || exit 0
    if [[ "${PROOF_CASE:-}" == replacement && -e "$state/attempt" ]]; then jq '.metadata.uid="replacement-uid"' "$state/pod.json"; else cat "$state/pod.json"; fi ;;
  'replace --dry-run=server '*)
    file=${args##* -f }
    if jq -e --slurpfile public "$PROOF_TEST_ROOT/listener.json" '.data == $public[0].data and ((.binaryData // {}) | length) == 0' "$file" >/dev/null; then exit 0; fi
    [[ "${PROOF_CASE:-}" != listener_admit ]] || exit 0
    if [[ "${PROOF_CASE:-}" == listener_api_error ]]; then printf 'connection refused\n' >&2
    else printf 'restrict-arc-openbao-listener public-listener-settings-only denied\n' >&2; fi
    exit 1 ;;
  'create --dry-run=server '*)
    file=${args##* -f }
    if jq -e '.spec.containers[0].securityContext.privileged == true' "$file" >/dev/null; then problem=privileged
    elif jq -e 'any(.spec.volumes[]; .hostPath != null)' "$file" >/dev/null; then problem=hostPath
    elif jq -e '.spec.hostNetwork == true' "$file" >/dev/null; then problem=hostNetwork
    elif jq -e '.spec.hostPID == true' "$file" >/dev/null; then problem=hostPID
    elif jq -e '.spec.containers[0].securityContext.capabilities.add == ["NET_RAW"]' "$file" >/dev/null; then problem=NET_RAW
    else exit 0; fi
    [[ "${PROOF_CASE:-}" != admit_privilege ]] || exit 0
    if [[ "${PROOF_CASE:-}" == api_failure ]]; then printf 'connection refused\n' >&2; else printf 'violates PodSecurity restricted: %s\n' "$problem" >&2; fi
    exit 1 ;;
  'create -f '*)
    file=$3
    jq '.metadata.uid="probe-uid" | .status={podIP:"10.1.1.1",containerStatuses:[{name:"probe",ready:true,restartCount:0,containerID:"containerd://probe"}]}' "$file" >"$state/pod.json"
    if [[ "${PROOF_CASE:-}" == wrong_node ]]; then jq '.spec.nodeName="other"' "$state/pod.json" >"$state/changed"; mv "$state/changed" "$state/pod.json"; fi
    touch "$state/created"; cat "$state/pod.json" ;;
  'wait '*) : ;;
  'exec '*hubble\ status*) : ;;
  'exec '*hubble\ observe*)
    while [[ "$#" -gt 0 ]]; do
      case "$1" in --since) when=$2;; --to-port) port=$2;; esac; shift
    done
    reason=POLICY_DENIED; [[ "${PROOF_CASE:-}" != wrong_reason ]] || reason=CT_TRUNCATED_OR_INVALID_HEADER
    jq -n --arg reason "$reason" --arg when "$when" --argjson port "$port" '{flow:{verdict:"DROPPED",drop_reason_desc:$reason,traffic_direction:"EGRESS",time:$when,node_name:"prod/node-a",source:{namespace:"arc-runners",pod_name:"arc-transport-12345-1-1"},destination:{namespace:"openbao",pod_name:"openbao-2"},IP:{source:"10.1.1.1",destination:"10.1.1.2"},l4:{TCP:{destination_port:$port,flags:{SYN:true}}}}}'
    [[ "${PROOF_CASE:-}" != lost_events ]] || printf '\n{"lost_events":{}}\n' ;;
  'exec '*curl*) touch "$state/attempt"; [[ "${PROOF_CASE:-}" != reachable ]] || exit 0; [[ "${PROOF_CASE:-}" != cert_error ]] || exit 60; exit 28 ;;
  'exec '*test*) : ;;
  'delete --raw='*)
    [[ "${PROOF_CASE:-}" != cleanup_failure ]] || exit 1
    jq -e '.preconditions.uid == "probe-uid"' "${args##* -f }" >/dev/null
    rm -f "$state/created" ;;
  *) printf 'unexpected fixture command\n' >&2; exit 99 ;;
esac
MOCK
chmod +x "$scratch/bin/gh" "$scratch/bin/go" "$scratch/bin/curl" "$scratch/bin/kubectl"
export PATH="$scratch/bin:$PATH"
cases=0
run_case() {
  local name=$1 expected=$2 result=0
  rm -f "$scratch/state/"*
  if [[ "$name" == source_explicit_* ]]; then
    (cd "$scratch/source"; PROOF_CASE=$name bash "$script" --same-node) >"$scratch/output" 2>"$scratch/error" || result=$?
  else
    PROOF_CASE=$name bash "$script" --same-node >"$scratch/output" 2>"$scratch/error" || result=$?
  fi
  [[ "$result" == "$expected" ]] || { printf 'FAIL %s: exit %s expected %s\n' "$name" "$result" "$expected"; cat "$scratch/error"; exit 1; }
  if [[ "$name" == listener_read_failure ]]; then
    grep -Fxq 'ARC transport: FAIL at inactive-credential-boundary' "$scratch/error" || { printf 'FAIL missing failure stage\n'; exit 1; }
    ! grep -Fq private-error-sentinel "$scratch/error" || { printf 'FAIL private diagnostic leaked\n'; exit 1; }
    grep -Fxq 'ARC transport: owned probe cleanup verified' "$scratch/output" || { printf 'FAIL missing cleanup\n'; exit 1; }
  fi
  # No secret, TokenRequest, JIT, host exec, policy change or node deletion is permitted.
  if [[ -f "$scratch/state/calls" ]] && grep -Eqi 'get secret |tokenrequest|create token|ephemeralrunner|delete node|patch |apply ' "$scratch/state/calls"; then exit 1; fi
  if [[ -f "$scratch/state/calls" ]] && grep 'replace ' "$scratch/state/calls" | grep -vq 'replace --dry-run=server '; then exit 1; fi
  if [[ "$name" == replacement ]] && grep -q 'delete --raw=' "$scratch/state/calls"; then printf 'FAIL replacement delete\n'; exit 1; fi
  cases=$((cases+1))
}
GITHUB_REF=refs/heads/feature run_case unreviewed_ref 1
run_case healthy_denied 0
run_case source_explicit_false 0
run_case source_explicit_drift 1
run_case listener_read_failure 17
run_case tls_error 60
for name in sealed stale_probe admit_privilege api_failure wrong_node reachable cert_error wrong_reason lost_events \
  listener_content listener_churn listener_audit listener_admission_disabled listener_failure_open listener_override \
  listener_admit listener_api_error issuance_admission_disabled issuance_exclusion issuance_failure_open issuance_override \
  listener_background_drift listener_existing_drift listener_unknown_field \
  issuance_background_drift issuance_existing_drift issuance_unknown_field \
  identity_stale identity_ordinal_only; do run_case "$name" 1; done
run_case replacement 4
run_case cleanup_failure 4
printf 'PASS: %s transport orchestration cases\n' "$cases"
