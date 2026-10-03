#!/usr/bin/env bash
# Explicitly dispatched disposable Docker proof. Never uses a production config.
set -euo pipefail
umask 077
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform && "${GITHUB_REF:-}" == refs/heads/main ]] || fail 'main-only Actions runner required'
[[ "${CONFIRM_DISPOSABLE_CILIUM_PROOF:-}" == RUN_DISPOSABLE_CILIUM_PROOF ]] || fail 'explicit disposable-cluster confirmation required'
[[ "${GITHUB_RUN_ID:-}" =~ ^[0-9]+$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[0-9]+$ && "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'invalid run identity'
[[ "${RUNNER_TEMP:-}" == /* && -d "${RUNNER_TEMP}" ]] || fail 'absolute runner scratch directory required'
readonly work="${RUNNER_TEMP}/cilium-deny-proof-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
readonly name="deny-proof-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
readonly context="kind-${name}" kubeconfig="${work}/kubeconfig"
readonly candidate="${RUNNER_TEMP}/cilium-deny-candidate"
readonly cilium_image='quay.io/cilium/cilium:v1.20.2@sha256:2939231d0d3e3ebddcd80fffa168b7ddcc78fdf0dc864d1c8c126ff523c54f01'
readonly image='docker.io/library/busybox:1.38.0-musl@sha256:ea2b9914a16a4ac1981994af97b318f7c7d4db76b580c56177f08bf76f4a0be8'
export KUBECONFIG="$kubeconfig"

receipt_update() {
  jq "$@" "${work}/receipt/result.json" >"${work}/receipt/result.next"
  mv "${work}/receipt/result.next" "${work}/receipt/result.json"
}
owned_containers() { docker ps -aq --filter "label=io.x-k8s.kind.cluster=${name}"; }
cleanup() {
  if [[ ! -f "${work}/owner.json" ]]; then [[ ! -d "$work" ]]; return; fi
  jq -e --arg name "$name" --arg head "$GITHUB_SHA" --arg config "$kubeconfig" \
    '.name == $name and .workflow_head == $head and .kubeconfig == $config and .provider == "Docker"' "${work}/owner.json" >/dev/null || return 1
  # Recheck the config, not only its receipt, before allowing a deletion. Both
  # calls are explicit: ambient context/provider can never choose the target.
  [[ "$(yq '.metadata.name' "${work}/ksail.yaml")" == "$name" &&
     "$(yq '.spec.cluster.provider' "${work}/ksail.yaml")" == Docker &&
     "$(yq '.spec.cluster.distribution' "${work}/ksail.yaml")" == Vanilla &&
     "$(yq '.spec.cluster.connection.kubeconfig' "${work}/ksail.yaml")" == "$kubeconfig" ]] || return 1
  local containers
  containers="$(owned_containers)" || return 1
  if [[ -n "$containers" ]]; then
    timeout 8m ksail --config "${work}/ksail.yaml" cluster delete \
      --name "$name" --provider Docker --kubeconfig "$kubeconfig" --force || return 1
  fi
  containers="$(owned_containers)" || return 1
  [[ -z "$containers" ]] || return 1
  receipt_update '.cleanup = "PASS"'
}
finish() {
  local result=$?
  trap - EXIT INT TERM
  if ! cleanup; then
    receipt_update '.cleanup = "FAIL" | .verdict = "FAIL"'
    result=1
  fi
  if [[ "$result" != 0 ]]; then receipt_update '.verdict = "FAIL"'; fi
  exit "$result"
}
case "${1:-}" in
  cleanup) cleanup; exit ;;
  run) ;;
  *) fail 'usage: prove-cilium-default-deny.sh run|cleanup' ;;
esac
[[ ! -e "$work" ]] || fail 'run scratch path already exists; refusing to reuse it'
# A hosted, otherwise empty Docker daemon prevents names or shared mirrors from
# belonging to another run. No Docker prune, network prune or volume deletion.
existing="$(docker ps -aq)" || fail 'Docker inventory read failed'
[[ -z "$existing" ]] || fail 'Docker daemon already has containers'
ksail_version="$(ksail --version)" || fail 'KSail version read failed'
grep -Eq '^ksail version v?7[.]193[.]8([[:space:]]|$)' <<<"$ksail_version" || fail 'audited KSail 7.193.8 required'
jq -e '.schema == 1 and .repository == "devantler-tech/platform" and (.head | test("^[0-9a-f]{40}$")) and (.pr > 0)' "${candidate}/candidate.json" >/dev/null || fail 'candidate receipt absent or invalid'
[[ "$(sha256sum "${candidate}/generator.yaml" | cut -d' ' -f1)" == "$(jq -r '.generator_sha256' "${candidate}/candidate.json")" &&
   "$(sha256sum "${candidate}/flux-copy.yaml" | cut -d' ' -f1)" == "$(jq -r '.flux_copy_sha256' "${candidate}/candidate.json")" ]] || fail 'candidate files differ from verified hashes'
[[ "$(yq '.kind + "/" + .metadata.name' "${candidate}/generator.yaml")" == ClusterPolicy/add-default-deny &&
   "$(yq '.kind + "/" + .metadata.name' "${candidate}/flux-copy.yaml")" == CiliumNetworkPolicy/default-deny ]] || fail 'unexpected fixed candidate resources'
mkdir -p "${work}/receipt"
jq --arg workflow "$GITHUB_SHA" --arg run "$GITHUB_RUN_ID" --arg attempt "$GITHUB_RUN_ATTEMPT" \
  '. + {workflow_head:$workflow,run_id:$run,run_attempt:$attempt,ksail_version:"7.193.8",cilium_version:"1.20.2",verdict:"UNKNOWN",cleanup:"UNKNOWN",floor_tests:{},tests:{},floor_valid_cnp_count:0,valid_cnp_count:0}' \
  "${candidate}/candidate.json" >"${work}/receipt/result.json"
cat >"${work}/ksail.yaml" <<CONFIG
apiVersion: ksail.io/v1alpha1
kind: Cluster
metadata:
  name: ${name}
spec:
  cluster:
    distribution: Vanilla
    distributionConfig: ${work}/kind.yaml
    provider: Docker
    cni: Cilium
    policyEngine: None
    gitOpsEngine: None
    certManager: Disabled
    csi: Disabled
    metricsServer: Disabled
    loadBalancer: Disabled
    controlPlanes: 1
    workers: 0
    connection:
      kubeconfig: ${kubeconfig}
      context: ${context}
      timeout: 15m
CONFIG
cat >"${work}/kind.yaml" <<CONFIG
apiVersion: kind.x-k8s.io/v1alpha4
kind: Cluster
name: ${name}
networking:
  disableDefaultCNI: true
nodes:
  - role: control-plane
CONFIG
jq -n --arg name "$name" --arg workflow "$GITHUB_SHA" --arg config "$kubeconfig" \
  '{name:$name,workflow_head:$workflow,kubeconfig:$config,provider:"Docker"}' >"${work}/owner.json"
trap finish EXIT
trap 'exit 1' INT TERM
# Empty mirror list is an explicit KSail switch: no shared registry containers.
timeout 20m ksail --config "${work}/ksail.yaml" cluster create --mirror-registry ''
k() { timeout 4m kubectl --kubeconfig "$kubeconfig" --context "$context" --request-timeout=30s "$@"; }
k -n kube-system rollout status daemonset/cilium --timeout=5m
actual_image="$(k -n kube-system get daemonset cilium -o json | jq -r '.spec.template.spec.containers[] | select(.name == "cilium-agent") | .image')"
[[ "$actual_image" == "$cilium_image" ]] || fail 'running Cilium image differs from audited version/digest'

# Generate using the actual candidate policy, not a hand-copied CNP fixture.
printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: deny-proof\n' >"${work}/namespace.yaml"
kyverno apply "${candidate}/generator.yaml" --resource "${work}/namespace.yaml" -o "${work}/generated" >"${work}/generation.log" 2>&1
shopt -s nullglob
generated=("${work}"/generated/*.yaml)
shopt -u nullglob
[[ "${#generated[@]}" -gt 0 ]] || fail 'Kyverno generated no resources'
yq -o=json -I=0 'select(.metadata.namespace == "deny-proof" and .kind == "CiliumNetworkPolicy" and (.metadata.name == "default-deny" or .metadata.name == "allow-dns"))' "${generated[@]}" | \
  jq -s '{apiVersion:"v1",kind:"List",items:.}' >"${work}/runtime-policies.yaml"
jq -e '[.items[].metadata.name] | sort == ["allow-dns","default-deny"]' "${work}/runtime-policies.yaml" >/dev/null || fail 'exact generated CNP/DNS floor missing'
generated_rules="$(jq -cS '.items[] | select(.kind == "CiliumNetworkPolicy" and .metadata.name == "default-deny") | {spec,specs}' "${work}/runtime-policies.yaml")"
flux_rules="$(yq -o=json -I=0 '.' "${candidate}/flux-copy.yaml" | jq -cS '{spec,specs}')"
[[ "$generated_rules" == "$flux_rules" ]] || fail 'Flux default-deny rule body differs from generation'
jq '.items[] | select(.metadata.name == "default-deny")' "${work}/runtime-policies.yaml" >"${work}/default-deny.json"
jq '.items[] | select(.metadata.name == "allow-dns")' "${work}/runtime-policies.yaml" >"${work}/allow-dns.json"
# jq owns this variable.
# shellcheck disable=SC2016
receipt_update --arg rules "$generated_rules" '.generated_rules = ($rules | fromjson)'

k apply -f "${work}/namespace.yaml"
# One tiny, digest-pinned toolbox serves and probes HTTP. No service account,
# host access or credentials; pod IPs avoid DNS becoming a false denial proof.
for role in client-allowed client-denied server-allowed server-denied; do
  if [[ "$role" == server-* ]]; then command='mkdir -p /tmp/www; printf "deny-proof-ok\n" >/tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www'; else command='exec sleep 3600'; fi
  jq -n --arg name "$role" --arg image "$image" --arg command "$command" \
    '{apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:"deny-proof",labels:{role:$name}},spec:{automountServiceAccountToken:false,securityContext:{runAsNonRoot:true,runAsUser:1000,fsGroup:1000,seccompProfile:{type:"RuntimeDefault"}},containers:[{name:"probe",image:$image,command:["/bin/sh","-ec",$command],securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}},resources:{requests:{cpu:"10m",memory:"8Mi"},limits:{cpu:"100m",memory:"32Mi"}},volumeMounts:[{name:"tmp",mountPath:"/tmp"}]}],volumes:[{name:"tmp",emptyDir:{}}]}} | if ($name | startswith("server-")) then .spec.containers[0].readinessProbe={httpGet:{path:"/",port:8080},initialDelaySeconds:1,periodSeconds:1,failureThreshold:30} else . end' >"${work}/${role}.json"
  k apply -f "${work}/${role}.json"
done
k -n deny-proof wait --for=condition=Ready pod --all --timeout=3m
probe_snapshot() {
  k -n deny-proof get pods client-allowed client-denied server-allowed server-denied -o json >"${work}/probe-pods.json" || return 1
  # Ready alone can lag a dead listener. Bind the four exact Pod/runtime
  # identities here, then separately exercise the denied listener on loopback.
  # shellcheck disable=SC2016
  jq -e --arg image "$image" '
    ([.items[].metadata.name] | sort) == ["client-allowed","client-denied","server-allowed","server-denied"] and
    ([.items[].metadata.uid] | unique | length) == 4 and
    ([.items[].status.podIP] | unique | length) == 4 and
    all(.items[];
      .metadata.namespace == "deny-proof" and .metadata.deletionTimestamp == null and
      (.metadata.uid | type == "string" and length > 0) and
      (.status.podIP | type == "string" and test("^[0-9]+[.][0-9]+[.][0-9]+[.][0-9]+$")) and
      (.status.podIP | split(".") | map(tonumber) | all(. >= 0 and . <= 255)) and
      .status.phase == "Running" and any(.status.conditions[]?; .type == "Ready" and .status == "True") and
      (.spec.containers | length) == 1 and .spec.containers[0].name == "probe" and .spec.containers[0].image == $image and
      (.status.containerStatuses | length) == 1 and .status.containerStatuses[0].name == "probe" and
      .status.containerStatuses[0].ready == true and .status.containerStatuses[0].restartCount == 0 and
      (.status.containerStatuses[0].containerID | type == "string" and length > 0) and
      (.status.containerStatuses[0].imageID | type == "string" and length > 0) and
      (.status.containerStatuses[0].state.running.startedAt | type == "string" and length > 0)
    )' "${work}/probe-pods.json" >/dev/null || return 1
  jq -cS '[.items[] | {name:.metadata.name,uid:.metadata.uid,pod_ip:.status.podIP,container_id:.status.containerStatuses[0].containerID,image_id:.status.containerStatuses[0].imageID,restart_count:.status.containerStatuses[0].restartCount,started_at:.status.containerStatuses[0].state.running.startedAt}] | sort_by(.name)' "${work}/probe-pods.json"
}
probe_snapshot >"${work}/probes-before.json" || fail 'baseline probe identities or readiness unavailable'
probes_unchanged() {
  probe_snapshot >"${work}/probes-current.json" || return 1
  cmp -s "${work}/probes-before.json" "${work}/probes-current.json"
}
allowed_ip="$(jq -r '.[] | select(.name == "server-allowed") | .pod_ip' "${work}/probes-before.json")"
denied_ip="$(jq -r '.[] | select(.name == "server-denied") | .pod_ip' "${work}/probes-before.json")"
http() {
  # Execute only inside the disposable probe pod.
  # shellcheck disable=SC2016
  k -n deny-proof exec "$1" -- sh -c 'wget -T 3 -q -O - "$1" 2>&1; result=$?; printf "\nPROBE_EXIT=%s\n" "$result"' probe "http://$2:8080/"
}
admitted() { local result; result="$(http "$1" "$2")" || return 1; [[ "$result" == *deny-proof-ok* && "${result##*$'\n'}" == PROBE_EXIT=0 ]]; }
denied() { local result; result="$(http "$1" "$2")" || return 1; [[ "$result" == *'timed out'* && "${result##*$'\n'}" == PROBE_EXIT=1 ]]; }
dns() { k -n deny-proof exec client-allowed -- nslookup kubernetes.default.svc.cluster.local >/dev/null; }
http_routes() {
  admitted client-allowed "$allowed_ip" || return 1
  if [[ "$1" == open ]]; then
    admitted client-denied "$allowed_ip" && admitted client-allowed "$denied_ip"
  else
    denied client-denied "$allowed_ip" && denied client-allowed "$denied_ip"
  fi
}
# Valid=True does not establish datapath convergence. Every phase requires
# stable probe identities and three consecutive samples with a healthy target
# and admitted HTTP alongside the expected result for each negative pair.
traffic_proof() {
  local mode="$1" require_dns="$2" consecutive=0 attempt
  for ((attempt=0; attempt<12; attempt++)); do
    probes_unchanged || fail 'probe readiness or UID/IP/runtime identity changed during traffic proof'
    if admitted server-denied 127.0.0.1 && { [[ "$require_dns" == false ]] || dns; } && http_routes "$mode" && admitted server-denied 127.0.0.1; then
      consecutive=$((consecutive + 1))
    else
      consecutive=0
    fi
    probes_unchanged || fail 'probe readiness or UID/IP/runtime identity changed during traffic proof'
    [[ "$consecutive" != 3 ]] || break
    sleep 2
  done
  [[ "$consecutive" == 3 ]]
}
# All later denied pairs must work before enforcement. A broken server or exec
# API cannot be mistaken for the policy blocking a healthy route.
probes_unchanged || fail 'baseline probe identities changed'
dns || fail 'baseline DNS failed'
if ! admitted server-denied 127.0.0.1 || ! http_routes open || ! admitted server-denied 127.0.0.1; then fail 'baseline HTTP route or target failed'; fi
probes_unchanged || fail 'baseline probe identities changed'

# Deliberately allow the OTHER direction on each negative pair, so each denial
# demonstrates one direction independently, alongside healthy admitted traffic.
# These L4 allow rules must never activate enforcement themselves. Otherwise
# their limited peers can make a broken candidate floor appear to block traffic.
cat >"${work}/allows.yaml" <<'POLICY'
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: allow-proof-http
  namespace: deny-proof
specs:
  - endpointSelector:
      matchLabels:
        role: client-allowed
    enableDefaultDeny:
      ingress: false
      egress: false
    egress:
      - toEndpoints:
          - matchLabels:
              role: server-allowed
        toPorts:
          - ports:
              - port: "8080"
                protocol: TCP
  - endpointSelector:
      matchLabels:
        role: client-denied
    enableDefaultDeny:
      ingress: false
      egress: false
    egress:
      - toEndpoints:
          - matchLabels:
              role: server-allowed
        toPorts:
          - ports:
              - port: "8080"
                protocol: TCP
  - endpointSelector:
      matchLabels:
        role: server-allowed
    enableDefaultDeny:
      ingress: false
      egress: false
    ingress:
      - fromEndpoints:
          - matchLabels:
              role: client-allowed
        toPorts:
          - ports:
              - port: "8080"
                protocol: TCP
  - endpointSelector:
      matchLabels:
        role: server-denied
    enableDefaultDeny:
      ingress: false
      egress: false
    ingress:
      - fromEndpoints:
          - matchLabels:
              role: client-allowed
        toPorts:
          - ports:
              - port: "8080"
                protocol: TCP
POLICY
snapshot() {
  local expected="$1"
  k -n deny-proof get ciliumnetworkpolicies -o json >"${work}/policies.json" || return 1
  # Cilium's Valid condition has no observedGeneration. Bind the object UID,
  # generation and complete spec/specs in each phase. Status alone could
  # describe another body, or a DNS policy could conceal broken floor egress.
  jq -e --slurpfile expected "$expected" '([.items[].metadata.name] | sort) == ($expected[0] | map(.name) | sort) and all(.items[]; (.metadata.uid | type == "string" and length > 0) and .metadata.generation > 0 and any(.status.conditions[]?; .type == "Valid" and .status == "True"))' "${work}/policies.json" >/dev/null || return 1
  jq -cS '[.items[] | {name:.metadata.name,uid:.metadata.uid,generation:.metadata.generation,spec,specs}] | sort_by(.name)' "${work}/policies.json" >"${work}/policies-current.json" || return 1
  jq -cS 'map({name,spec,specs})' "${work}/policies-current.json" >"${work}/actual-policies.json" || return 1
  cmp -s "$expected" "${work}/actual-policies.json" || return 1
  cat "${work}/policies-current.json"
}
preserves_policies() {
  jq -cS --slurpfile previous "$1" '[.[] | select(.name as $name | any($previous[0][]; .name == $name))]' "$2" >"${work}/preserved-policies.json" || return 1
  cmp -s "$1" "${work}/preserved-policies.json"
}
yq -o=json -I=0 '.' "${work}/allows.yaml" | jq -cS '[{name:.metadata.name,spec,specs}]' >"${work}/expected-helper.json"
jq -cS --slurpfile helper "${work}/expected-helper.json" '[$helper[0][], {name:.metadata.name,spec,specs}] | sort_by(.name)' "${work}/default-deny.json" >"${work}/expected-floor.json"
yq -o=json -I=0 '.' "${work}/allows.yaml" | jq -cS --slurpfile runtime "${work}/runtime-policies.yaml" \
  '[($runtime[0].items[]),.] | map({name:.metadata.name,spec,specs}) | sort_by(.name)' >"${work}/expected-policies.json"

k apply -f "${work}/allows.yaml"
k -n deny-proof wait --for=condition=Valid=True ciliumnetworkpolicy --all --timeout=3m
snapshot "${work}/expected-helper.json" >"${work}/helper-before.json" || fail 'main HTTP helper validity or complete body unavailable'
traffic_proof open true || fail 'main HTTP helper changed previously healthy HTTP/DNS routes'
snapshot "${work}/expected-helper.json" >"${work}/helper-after.json" || fail 'HTTP helper validity or body changed during its baseline'
cmp -s "${work}/helper-before.json" "${work}/helper-after.json" || fail 'HTTP helper UID, generation or spec changed during its baseline'

# Prove the floor before adding the generated DNS companion, whose egress
# rules can independently activate default deny. DNS is intentionally absent
# in this phase; HTTP uses fixed Pod IPs and the helper is non-enforcing.
k apply -f "${work}/default-deny.json"
k -n deny-proof wait --for=condition=Valid=True ciliumnetworkpolicy --all --timeout=3m
snapshot "${work}/expected-floor.json" >"${work}/floor-before.json" || fail 'floor-only CNP validity or complete bodies unavailable'
preserves_policies "${work}/helper-after.json" "${work}/floor-before.json" || fail 'HTTP helper changed while applying the floor'
traffic_proof denied false || fail 'floor-only healthy target/admitted HTTP and independent ingress/egress denial did not converge'
snapshot "${work}/expected-floor.json" >"${work}/floor-after.json" || fail 'floor-only CNP validity or bodies changed during traffic proof'
cmp -s "${work}/floor-before.json" "${work}/floor-after.json" || fail 'floor-only policy UID, generation or spec changed during proof'
receipt_update '.floor_tests = {pre_floor_http:true,allowed_http:true,denied_ingress:true,denied_egress:true,denied_target_http:true} | .floor_valid_cnp_count = 2'

k apply -f "${work}/allow-dns.json"
k -n deny-proof wait --for=condition=Valid=True ciliumnetworkpolicy --all --timeout=3m
snapshot "${work}/expected-policies.json" >"${work}/policies-before.json" || fail 'complete generated/main CNP validity or bodies unavailable'
preserves_policies "${work}/floor-after.json" "${work}/policies-before.json" || fail 'floor or HTTP helper changed while adding DNS'
receipt_update '.valid_cnp_count = 3'
traffic_proof denied true || fail 'complete healthy target/DNS/admitted/independent ingress and egress denial did not converge'
snapshot "${work}/expected-policies.json" >"${work}/policies-after.json" || fail 'CNP validity or complete bodies were lost during traffic proof'
cmp -s "${work}/policies-before.json" "${work}/policies-after.json" || fail 'policy UID, generation or spec changed during proof'
probes_unchanged || fail 'final probe readiness or UID/IP/runtime identity changed during proof'
bindings='[]'
for policy_name in default-deny allow-dns allow-proof-http; do
  spec_hash="$(jq -cS --arg name "$policy_name" '.[] | select(.name == $name) | {spec,specs}' "${work}/policies-after.json" | sha256sum | cut -d' ' -f1)"
  # jq owns these variables.
  # shellcheck disable=SC2016
  binding="$(jq -c --arg name "$policy_name" --arg hash "$spec_hash" '.[] | select(.name == $name) | {name,uid,generation,spec_sha256:$hash,valid:true}' "${work}/policies-after.json")"
  bindings="$(jq -c --argjson binding "$binding" '. + [$binding]' <<<"$bindings")"
done
# shellcheck disable=SC2016
receipt_update --argjson bindings "$bindings" '.policy_bindings = $bindings'
probe_bindings='[]'
for role in client-allowed client-denied server-allowed server-denied; do
  # Keep private runtime addresses and container identities out of the artifact.
  ip_hash="$(jq -r --arg name "$role" '.[] | select(.name == $name) | .pod_ip' "${work}/probes-current.json" | sha256sum | cut -d' ' -f1)"
  container_hash="$(jq -r --arg name "$role" '.[] | select(.name == $name) | .container_id' "${work}/probes-current.json" | sha256sum | cut -d' ' -f1)"
  image_hash="$(jq -r --arg name "$role" '.[] | select(.name == $name) | .image_id' "${work}/probes-current.json" | sha256sum | cut -d' ' -f1)"
  # shellcheck disable=SC2016
  binding="$(jq -c --arg name "$role" --arg ip "$ip_hash" --arg container "$container_hash" --arg image "$image_hash" '.[] | select(.name == $name) | {name,uid,ip_sha256:$ip,container_sha256:$container,image_sha256:$image,restart_count,ready:true}' "${work}/probes-current.json")"
  probe_bindings="$(jq -c --argjson binding "$binding" '. + [$binding]' <<<"$probe_bindings")"
done
# shellcheck disable=SC2016
receipt_update --argjson bindings "$probe_bindings" '.probe_bindings = $bindings'
receipt_update '.tests = {dns:true,allowed_http:true,denied_ingress:true,denied_egress:true,denied_target_http:true} | .verdict = "PASS"'
printf 'PASS: candidate floor independently denies both forbidden directions; generated DNS and admitted HTTP preserved\n'
