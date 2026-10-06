#!/usr/bin/env bash
# CI-owned, bounded runtime acceptance. No Secret or generated JIT configuration is read.
set -euo pipefail
umask 077

fail() { printf 'ARC acceptance: FAIL at %s\n' "$1" >&2; exit 1; }
[[ "$#" -eq 1 && "$1" == --if-active ]] || fail arguments
readonly controller_overlay=k8s/providers/hetzner/infrastructure/controllers/kustomization.yaml
readonly pool_overlay=k8s/providers/hetzner/infrastructure/kustomization.yaml
controller_active=$(yq '[.resources[] | select(. == "../../../../bases/infrastructure/controllers/actions-runner-controller/")] | length' "$controller_overlay")
pool_active=$(yq '[.resources[] | select(. == "../../../bases/infrastructure/ksail-analysis-runners/")] | length' "$pool_overlay")
# Render every independently reconciled production layer before deciding to
# skip. Legacy bases, aliases and overlay patches cannot activate ARC unseen.
source_scratch=$(mktemp -d)
trap 'rm -rf "$source_scratch"' EXIT
for overlay in k8s/providers/hetzner/apps k8s/providers/hetzner/infrastructure \
  k8s/providers/hetzner/infrastructure/controllers k8s/clusters/prod/bootstrap k8s/clusters/prod; do
  timeout 95s kubectl kustomize "$overlay" >>"$source_scratch/rendered.yaml" \
    2>"$source_scratch/render-error" || fail source-render
  printf '\n---\n' >>"$source_scratch/rendered.yaml"
done
yq ea -o=json -I=0 '[select(.kind == "HelmRelease" and
  ((.metadata.namespace == "arc-systems" and .metadata.name == "arc-controller") or
   (.metadata.namespace == "arc-ksail-analysis" and .metadata.name == "ksail-analysis-runners")))]' \
  "$source_scratch/rendered.yaml" >"$source_scratch/releases.json"
rendered_controller=$(jq '[.[] | select(.metadata.namespace == "arc-systems")] | length' "$source_scratch/releases.json")
rendered_pool=$(jq '[.[] | select(.metadata.namespace == "arc-ksail-analysis")] | length' "$source_scratch/releases.json")
if [[ "$controller_active" == 0 && "$pool_active" == 0 && "$rendered_controller" == 0 && "$rendered_pool" == 0 ]]; then
  printf 'ARC acceptance: inactive source; no runtime access\n'
  exit 0
fi
[[ "$controller_active" == 1 && "$pool_active" == 1 && "$rendered_controller" == 1 && "$rendered_pool" == 1 ]] || fail partial-activation
source_image=$(yq '.spec.values.template.spec.containers[0].image' k8s/bases/infrastructure/ksail-analysis-runners/helm-release.yaml)
jq -e --arg image "$source_image" 'all(.[]; .spec.suspend == false) and
  all(.[] | select(.metadata.namespace == "arc-ksail-analysis");
    .spec.values.template.spec.containers[0].image == $image and
    .spec.values.template.spec.initContainers[0].image == $image)' "$source_scratch/releases.json" >/dev/null || fail rendered-source-state
rm -rf "$source_scratch"
trap - EXIT
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || fail deployment-identity
[[ "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]{0,19}$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]{0,2}$ ]] || fail run-identity
[[ "${PLATFORM_MANIFEST_DIGEST:-}" =~ ^sha256:[0-9a-f]{64}$ ]] || fail revision
readonly context=admin@prod namespace=arc-ksail-analysis
readonly ownership_label=platform.devantler.tech/arc-runtime-probe
readonly probe="arc-proof-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
scratch=$(mktemp -d)
readonly scratch
created=false
probe_uid=''
stage=initialization
trap 'printf "ARC acceptance: FAIL at %s\n" "$stage" >&2' ERR

kc() { timeout 35s kubectl --context "$context" --request-timeout=30s "$@" 2>/dev/null; }
wait_ready() {
  timeout 630s kubectl --context "$context" --request-timeout=610s -n "$namespace" \
    wait "pod/$probe" --for=condition=Ready --timeout=600s 2>/dev/null
}
quiet() { "$@" >/dev/null 2>"$scratch/command-error"; }

cleanup() {
  local result=$? failed=0 remaining=''
  trap - EXIT ERR INT TERM
  if "$created"; then
    # A timed-out create may have succeeded; without its response UID, fail closed.
    # A DELETE precondition binds the mutation atomically, including replacement races.
    local current
    if [[ -z "$probe_uid" ]]; then
      failed=1
    else
      current=$(kc -n "$namespace" get pod "$probe" --ignore-not-found -o json) || failed=1
      if [[ -n "$current" ]]; then
        if ! jq -e --arg name "$probe" --arg key "$ownership_label" --arg owner "$probe" --arg uid "$probe_uid" \
          '.metadata.name == $name and .metadata.labels[$key] == $owner and .metadata.uid == $uid' \
          <<<"$current" >/dev/null; then
          failed=1
        else
          jq -n --arg uid "$probe_uid" '{apiVersion:"v1",kind:"DeleteOptions",
            preconditions:{uid:$uid},gracePeriodSeconds:5,propagationPolicy:"Background"}' >"$scratch/delete.json"
          quiet kc delete --raw="/api/v1/namespaces/$namespace/pods/$probe" -f "$scratch/delete.json" || failed=1
          quiet kc -n "$namespace" wait "pod/$probe" --for=delete --timeout=25s || failed=1
        fi
      fi
    fi
    current=$(kc -n "$namespace" get pod "$probe" --ignore-not-found -o json) || failed=1
    [[ -z "$current" ]] || failed=1
    remaining=$(kc -n "$namespace" get pods -l "$ownership_label=$probe" -o json) || failed=1
    jq -e '.items | length == 0' <<<"$remaining" >/dev/null || failed=1
    # The dedicated min-zero pool may take the configured unneeded interval to drain.
    # Never delete nodes or resize a shared pool to manufacture cleanup.
    local deadline=$((SECONDS + 1200))
    while ((SECONDS < deadline)); do
      remaining=$(kc get nodes -o json) || { failed=1; break; }
      if jq -e '.items | all(.[]; .metadata.labels["platform.devantler.tech/ksail-analysis"] != "enabled" and
        (.metadata.name | startswith("autoscale-ksail-analysis-") | not))' <<<"$remaining" >/dev/null; then break; fi
      sleep 10
    done
    jq -e '.items | all(.[]; .metadata.labels["platform.devantler.tech/ksail-analysis"] != "enabled" and
      (.metadata.name | startswith("autoscale-ksail-analysis-") | not))' <<<"$remaining" >/dev/null || failed=1
  fi
  rm -rf "$scratch"
  if [[ "$failed" != 0 ]]; then printf 'ARC acceptance: FAIL cleanup\n' >&2; exit 4; fi
  printf 'ARC acceptance: cleanup verified\n'
  exit "$result"
}
trap cleanup EXIT
trap 'exit 143' INT TERM

stage=deployed-revision
quiet bash scripts/wait-for-platform-flux-revision.sh "$PLATFORM_MANIFEST_DIGEST"
# Helm readiness precedes asynchronous ARC registration. Await the full
# revision/generation join before taking the immutable acceptance snapshots.
quiet timeout 660s bash scripts/wait-for-ksail-arc-registration.sh "$PLATFORM_MANIFEST_DIGEST"
for layer in infrastructure apps; do
  kc -n flux-system get kustomization "$layer" -o json >"$scratch/layer.json"
  jq -e --arg revision "latest@$PLATFORM_MANIFEST_DIGEST" \
    '.status.lastAppliedRevision == $revision and .status.observedGeneration == .metadata.generation and
     any(.status.conditions[]; .type == "Ready" and .status == "True")' "$scratch/layer.json" >/dev/null
done

stage=registration-and-bounds
kc -n "$namespace" get autoscalingrunnerset.actions.github.com ksail-code-quality -o json >"$scratch/ars.json"
jq -e '
  .spec.template.spec.volumes[2].configMap.name as $metrics |
  .status.phase == "Running" and .status.observedGeneration == .metadata.generation and
  ((.metadata.annotations["runner-scale-set-id"] | tonumber) > 0) and
  .spec.githubConfigUrl == "https://github.com/devantler-tech/ksail" and
  .spec.githubConfigSecret == "arc-ksail-app" and .spec.runnerScaleSetName == "ksail-code-quality" and
  .spec.minRunners == 0 and .spec.maxRunners == 1 and
  .spec.template.spec.serviceAccountName == "ksail-code-quality-gha-rs-no-permission" and
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.securityContext.runAsUser == 1001 and
  .spec.template.spec.securityContext.runAsGroup == 1001 and
  .spec.template.spec.securityContext.fsGroup == 1001 and
  .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
  (.spec.template.spec.containers | length) == 1 and
  (.spec.template.spec.initContainers | length) == 1 and
  .spec.template.spec.containers[0].resources.requests.memory == "12Gi" and
  .spec.template.spec.containers[0].resources.limits.memory == "14Gi" and
  .spec.template.spec.containers[0].resources.requests.cpu == "3" and
  .spec.template.spec.containers[0].resources.limits.cpu == "3500m" and
  .spec.template.spec.containers[0].resources.requests["ephemeral-storage"] == "32Gi" and
  .spec.template.spec.containers[0].resources.limits["ephemeral-storage"] == "48Gi" and
  .spec.template.spec.nodeSelector["platform.devantler.tech/ksail-analysis"] == "enabled" and
  .spec.template.spec.tolerations == [{"key":"platform.devantler.tech/ksail-analysis","operator":"Equal","value":"enabled","effect":"NoSchedule"}] and
  ($metrics | test("^ksail-arc-job-metrics-[a-z0-9]{10}$")) and
  .spec.template.spec.volumes == [{"name":"runner-home","emptyDir":{"sizeLimit":"40Gi"}},{"name":"runner-tmp","emptyDir":{"sizeLimit":"2Gi"}},
    {"name":"runner-metrics","configMap":{"name":$metrics,"defaultMode":365,"items":[{"key":"job-metrics.sh","path":"job-metrics.sh"}]}}] and
  .spec.template.spec.containers[0].env == [{"name":"ACTIONS_RUNNER_HOOK_JOB_COMPLETED","value":"/etc/ksail-arc-metrics/job-metrics.sh"}] and
  .spec.template.spec.containers[0].volumeMounts == [{"name":"runner-home","mountPath":"/home/runner"},{"name":"runner-tmp","mountPath":"/tmp"},
    {"name":"runner-metrics","mountPath":"/etc/ksail-arc-metrics","readOnly":true}] and
  (.spec.template.spec.initContainers[0].env // [] | length) == 0 and
  .spec.template.spec.initContainers[0].volumeMounts == [{"name":"runner-home","mountPath":"/runner-data"}] and
  all((.spec.template.spec.containers + .spec.template.spec.initContainers)[];
    .securityContext.readOnlyRootFilesystem == true and .securityContext.privileged == false and
    .securityContext.allowPrivilegeEscalation == false and .securityContext.capabilities.drop == ["ALL"] and
    .envFrom == null and
    (.image | test("^ghcr.io/devantler-tech/ksail-analysis-runner@sha256:[0-9a-f]{64}$"))) and
  .spec.template.spec.initContainers[0].image == .spec.template.spec.containers[0].image
' "$scratch/ars.json" >/dev/null
stage=immutable-job-metrics
metrics_name=$(jq -er '.spec.template.spec.volumes[2].configMap.name' "$scratch/ars.json")
kc -n "$namespace" get configmap "$metrics_name" -o json >"$scratch/metrics-config.json"
jq -e --rawfile expected scripts/ksail-arc-job-metrics.sh '
  .immutable == true and (.metadata.uid | type == "string" and length > 0) and
  .metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"] == "disabled" and
  (.data | length) == 1 and (.binaryData // {} | length) == 0 and .data["job-metrics.sh"] == $expected
' "$scratch/metrics-config.json" >/dev/null
image=$(jq -r '.spec.template.spec.containers[0].image' "$scratch/ars.json")
expected_image=$(yq '.spec.values.template.spec.containers[0].image' k8s/bases/infrastructure/ksail-analysis-runners/helm-release.yaml)
[[ "$image" == "$expected_image" ]] || fail image-drift
stage=immutable-image
pinned_digest=${image##*@}
quiet timeout 90s cosign verify \
  --certificate-identity 'https://github.com/devantler-tech/ksail/.github/workflows/publish-ksail-analysis-runner.yaml@refs/heads/main' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' "$image"
timeout 90s docker buildx imagetools inspect "$image" --raw >"$scratch/image.json" 2>"$scratch/registry-error"
[[ ! -s "$scratch/registry-error" ]] || fail registry-diagnostics
go run ./scripts/verify-ksail-arc-runtime verify-image --digest "$pinned_digest" \
  <"$scratch/image.json" >"$scratch/runtime-digest"
runtime_digest=$(cat "$scratch/runtime-digest")
readonly runtime_image="ghcr.io/devantler-tech/ksail-analysis-runner@$runtime_digest"
if [[ "$runtime_digest" != "$pinned_digest" ]]; then
  timeout 90s docker buildx imagetools inspect "$runtime_image" --raw >"$scratch/runtime-image.json" 2>"$scratch/registry-error"
  [[ ! -s "$scratch/registry-error" ]] || fail registry-diagnostics
  go run ./scripts/verify-ksail-arc-runtime verify-image --digest "$runtime_digest" \
    <"$scratch/runtime-image.json" >"$scratch/runtime-readback"
  cmp -s "$scratch/runtime-digest" "$scratch/runtime-readback" || fail runtime-descriptor
fi
stage=idle-pool-baseline
kc -n kube-system get deployment cluster-autoscaler-hetzner-cluster-autoscaler \
  -o jsonpath='{range .spec.template.spec.containers[*].args[*]}{.}{"\n"}{end}' >"$scratch/autoscaler-args"
declared_ceiling=$(grep '^--max-nodes-total=' "$scratch/autoscaler-args")
declared_pool=$(grep '^--nodes=.*:autoscale-ksail-analysis$' "$scratch/autoscaler-args")
declared_provider=$(grep '^--cloud-provider=' "$scratch/autoscaler-args")
[[ "$declared_ceiling" == --max-nodes-total=9 && \
   "$declared_pool" == --nodes=0:1:cx53:fsn1:autoscale-ksail-analysis && \
   "$declared_provider" == --cloud-provider=hetzner ]] || fail autoscaler-boundary
kc -n "$namespace" get pods -l platform.devantler.tech/arc-role=runner -o json \
  | jq -e '.items | length == 0' >/dev/null
kc -n "$namespace" get pods -l "$ownership_label" -o json \
  | jq -e '.items | length == 0' >/dev/null
kc get nodes -o json >"$scratch/nodes.json"
jq -e --arg key platform.devantler.tech/ksail-analysis \
  '(.items | length) < 9 and all(.items[]; .metadata.labels[$key] != "enabled" and
    (.metadata.name | startswith("autoscale-ksail-analysis-") | not) and
    (.metadata.uid | type == "string" and length > 0))' "$scratch/nodes.json" >/dev/null

stage=restricted-admission
kc -n "$namespace" get resourcequotas -o json >"$scratch/quotas.json"
quiet go run ./scripts/verify-ksail-arc-runtime verify-quota <"$scratch/quotas.json"
# Retain the actual chart-generated template; replace only process/lifecycle and run identity.
jq --arg name "$probe" --arg ns "$namespace" --arg key "$ownership_label" '
  {apiVersion:"v1",kind:"Pod",metadata:{name:$name,namespace:$ns,
   labels:(.spec.template.metadata.labels + {($key):$name})},spec:.spec.template.spec} |
  .spec.restartPolicy="Never" | .spec.activeDeadlineSeconds=1200 |
  .spec.containers[0].command=["/bin/sh","-ec",
   "/usr/local/bin/ksail-analysis-smoke --live-runner; test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token; curl --proto =https --tlsv1.2 --fail --silent --show-error --max-time 20 --output /dev/null https://api.github.com/zen; printf ready > /tmp/arc-proof-ready; sleep 900"] |
  .spec.containers[0].readinessProbe={exec:{command:["/bin/sh","-ec","test -f /tmp/arc-proof-ready"]},periodSeconds:2}
' "$scratch/ars.json" >"$scratch/pod.json"
quiet kc create --dry-run=server -f "$scratch/pod.json"
for negative in privileged hostpath; do
  if [[ "$negative" == privileged ]]; then
    jq '.spec.containers[0].securityContext.privileged=true' "$scratch/pod.json" >"$scratch/negative.json"
  else
    jq '.spec.volumes += [{"name":"forbidden-host","hostPath":{"path":"/","type":"Directory"}}]' "$scratch/pod.json" >"$scratch/negative.json"
  fi
  if timeout 35s kubectl --context "$context" --request-timeout=30s create --dry-run=server \
    -f "$scratch/negative.json" >"$scratch/denied-out" 2>"$scratch/denied-error"; then fail negative-admission; fi
  # An API/transport failure does not prove admission interception.
  grep -Eq 'violates PodSecurity.*restricted|admission webhook.*denied the request' "$scratch/denied-error" || fail unproven-admission-denial
  if [[ "$negative" == privileged ]]; then
    grep -qi 'privileged' "$scratch/denied-error" || fail unrelated-admission-denial
  else
    grep -Eqi 'hostPath|host.path' "$scratch/denied-error" || fail unrelated-admission-denial
  fi
done
created=true
kc create -f "$scratch/pod.json" -o json >"$scratch/created-pod.json"
probe_uid=$(jq -er --arg name "$probe" --arg ns "$namespace" --arg key "$ownership_label" \
  'select(.metadata.name == $name and .metadata.namespace == $ns and .metadata.labels[$key] == $name) |
   .metadata.uid | select(type == "string" and length > 0)' "$scratch/created-pod.json")
quiet wait_ready
kc -n "$namespace" get pod "$probe" -o json >"$scratch/live-pod.json"
jq -e --arg uid "$probe_uid" '.metadata.uid == $uid' "$scratch/live-pod.json" >/dev/null
probe_ip=$(jq -r '.status.podIP' "$scratch/live-pod.json")
probe_node=$(jq -r '.spec.nodeName' "$scratch/live-pod.json")
quiet go run ./scripts/verify-ksail-arc-runtime verify-pod --desired "$scratch/pod.json" \
  --actual "$scratch/live-pod.json" --runtime-image "$runtime_image"
kc get node "$probe_node" -o json >"$scratch/node.json"
jq -e --slurpfile before "$scratch/nodes.json" '
 .metadata.uid as $uid | ($uid | type == "string" and length > 0) and
 all($before[0].items[]; .metadata.uid != $uid) and
 (.metadata.name | test("^autoscale-ksail-analysis-[0-9a-f]{1,16}$")) and
 .metadata.labels["platform.devantler.tech/ksail-analysis"] == "enabled" and
 .metadata.labels["node.kubernetes.io/instance-type"] == "cx53" and
 .status.nodeInfo.operatingSystem == "linux" and .status.nodeInfo.architecture == "amd64" and
 any(.spec.taints[]; .key == "platform.devantler.tech/ksail-analysis" and .value == "enabled" and .effect == "NoSchedule") and
 any(.status.conditions[]; .type == "Ready" and .status == "True")' "$scratch/node.json" >/dev/null
node_uid=$(jq -er '.metadata.uid' "$scratch/node.json")
kc get nodes -o json | jq -e --arg uid "$node_uid" --arg name "$probe_node" \
  '(.items | length) <= 9 and any(.items[]; .metadata.uid == $uid and .metadata.name == $name) and
   ([.items[] | select(.metadata.labels["platform.devantler.tech/ksail-analysis"] == "enabled" or
     (.metadata.name | startswith("autoscale-ksail-analysis-")))] | length) == 1' >/dev/null
stage=allocatable-headroom
# Print only UIDs/phases and resource quantities, never runner environment/JIT data.
kc get pods -A --field-selector "spec.nodeName=$probe_node" -o jsonpath='{range .items[*]}P{"\t"}{.metadata.uid}{"\t"}{.status.phase}{"\n"}{range .spec.containers[*]}R{"\t"}{.resources.requests.cpu}{"\t"}{.resources.requests.memory}{"\t"}{.resources.requests.ephemeral-storage}{"\n"}{end}{range .spec.initContainers[*]}R{"\t"}{.resources.requests.cpu}{"\t"}{.resources.requests.memory}{"\t"}{.resources.requests.ephemeral-storage}{"\n"}{end}R{"\t"}{.spec.overhead.cpu}{"\t"}{.spec.overhead.memory}{"\t"}{.spec.overhead.ephemeral-storage}{"\n"}{end}' >"$scratch/reservations"
quiet go run ./scripts/verify-ksail-arc-runtime verify-budget --node "$scratch/node.json" \
  --reservations "$scratch/reservations" --probe-uid "$probe_uid"

stage='observer-and-healthy-targets'
kc -n kube-system get pods -l k8s-app=cilium --field-selector "spec.nodeName=$probe_node" -o json \
 | jq -e '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] |
  if length == 1 then .[0] else error("observer count") end' >"$scratch/agent.json"
agent=$(jq -r '.metadata.name' "$scratch/agent.json")
kc -n kube-system get configmap cilium-config -o json >"$scratch/cilium-config.json"
cluster=$(jq -er '.data["cluster-name"] | select(length > 0)' "$scratch/cilium-config.json")
observer="$cluster/$probe_node"
quiet kc -n kube-system exec "$agent" -c cilium-agent -- hubble status \
  --server unix:///var/run/cilium/hubble.sock --timeout 5s --request-timeout 5s --output json
kc -n kube-system get pods -l k8s-app=kube-dns -o json \
 | jq -e '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))][0] |
  select(.metadata.uid != null and .status.podIP != null)' >"$scratch/dns.json"
dns_pod=$(jq -r '.metadata.name' "$scratch/dns.json")
dns_ip=$(jq -r '.status.podIP' "$scratch/dns.json")
kc -n default get endpoints kubernetes -o json >"$scratch/api.json"
api_ip=$(jq -er '.subsets[0].addresses[0].ip' "$scratch/api.json")
api_port=$(jq -er '.subsets[0].ports[] | select(.name == "https") | .port' "$scratch/api.json")

healthy_dns() {
  local reply
  reply=$(kc get --raw "/api/v1/namespaces/kube-system/pods/$dns_pod:8181/proxy/ready")
  [[ "$reply" == OK ]] || fail unhealthy-internal-control
}
healthy_api() {
  local reply
  reply=$(kc --server="https://$api_ip:$api_port" get --raw /readyz)
  [[ "$reply" == ok ]] || fail unhealthy-api-control
}
identity() {
  jq -c '{uid:.metadata.uid,ip:.status.podIP,node:.spec.nodeName,
   containers:[.status.containerStatuses[]|{name,containerID,restartCount,ready}]}'
}
negative_egress() {
  local destination=$1 port=$2 url=$3 target_ns=$4 target_pod=$5 start end result
  start=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  # Only curl's connect/total timeout counts as a denied attempt; exec/transport failure does not.
  if kc -n "$namespace" exec "$probe" -c runner -- curl --noproxy '*' --connect-timeout 3 \
    --max-time 5 --silent --show-error --output /dev/null "$url" >"$scratch/attempt-out" 2>"$scratch/attempt-error"; then
    fail network-isolation
  else
    result=$?
  fi
  [[ "$result" == 28 ]] || fail unproven-curl-timeout
  # Re-prove that exec still reaches this process after the bounded attempt.
  quiet kc -n "$namespace" exec "$probe" -c runner -- test -f /tmp/arc-proof-ready
  end=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  timeout 20s kubectl --context "$context" --request-timeout=15s -n kube-system exec "$agent" -c cilium-agent -- \
    hubble observe --server unix:///var/run/cilium/hubble.sock --timeout 5s --since "$start" --until "$end" \
    --from-ip "$probe_ip" --from-pod "$namespace/$probe" --to-ip "$destination" --to-port "$port" \
    --protocol tcp --traffic-direction egress --verdict DROPPED --drop-reason-desc POLICY_DENIED --output jsonpb \
    >"$scratch/flows" 2>"$scratch/observer-error"
  [[ ! -s "$scratch/observer-error" ]] || fail observer-diagnostics
  quiet go run ./scripts/verify-ksail-arc-runtime --pod "$probe" --source "$probe_ip" \
    --destination "$destination" --port "$port" --node "$observer" --since "$start" --until "$end" \
    --destination-namespace "$target_ns" --destination-pod "$target_pod" <"$scratch/flows"
}
stage=intercepted-egress-denials
healthy_dns
healthy_api
negative_egress "$dns_ip" 8181 "http://$dns_ip:8181/ready" kube-system "$dns_pod"
negative_egress "$api_ip" "$api_port" "https://$api_ip:$api_port/readyz" '' ''
healthy_dns
healthy_api
kc -n "$namespace" get pod "$probe" -o json | identity >"$scratch/probe-after"
identity <"$scratch/live-pod.json" >"$scratch/probe-before"
cmp -s "$scratch/probe-before" "$scratch/probe-after" || fail probe-churn
kc -n kube-system get pod "$agent" -o json | identity >"$scratch/agent-after"
identity <"$scratch/agent.json" >"$scratch/agent-before"
cmp -s "$scratch/agent-before" "$scratch/agent-after" || fail observer-churn
kc -n kube-system get pod "$dns_pod" -o json | identity >"$scratch/dns-after"
identity <"$scratch/dns.json" >"$scratch/dns-before"
cmp -s "$scratch/dns-before" "$scratch/dns-after" || fail target-churn
kc -n default get endpoints kubernetes -o json | jq -c '.subsets' >"$scratch/api-after"
jq -c '.subsets' "$scratch/api.json" >"$scratch/api-before"
cmp -s "$scratch/api-before" "$scratch/api-after" || fail api-target-churn

stage=job-cgroup-measurement
quiet kc -n "$namespace" exec "$probe" -- /etc/ksail-arc-metrics/job-metrics.sh
stage=complete
printf 'PASS: ARC registration, restricted image, admission and two intercepted egress denials; manifest=%s\n' "$PLATFORM_MANIFEST_DIGEST"
