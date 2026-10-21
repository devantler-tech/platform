#!/usr/bin/env bash
# A disposable GitHub-hosted kubelet/controller trial, never an ambient cluster.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "FAIL: WebApp hosted fixture: $1" >&2; exit 1; }
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted &&
   "$(uname -s)/$(uname -m)" == Linux/x86_64 ]] || fail 'requires an isolated GitHub-hosted Linux VM'
: "${RUNNER_TEMP:?}"
work="$(mktemp -d "$RUNNER_TEMP/webapp-runtime.XXXXXX")"
fixture="webapp-${work##*.}"
fixture="$(tr '[:upper:]' '[:lower:]' <<<"$fixture")"
export KUBECONFIG="$work/kubeconfig" GOWORK=off
forward_pid=''
created=false
cleanup() {
  if [[ -n "$forward_pid" ]]; then kill "$forward_pid" 2>/dev/null || true; wait "$forward_pid" 2>/dev/null || true; fi
  if [[ "$created" == true ]]; then "$work/kind" delete cluster --name "$fixture" >/dev/null 2>&1 || true; fi
  rm -rf -- "$work"
}
trap cleanup EXIT
download() {
  local url="$1" destination="$2" digest="$3"
  curl --proto '=https' --proto-redir '=https' --location --fail --silent --show-error \
    --connect-timeout 10 --max-time 180 "$url" --output "$destination"
  printf '%s  %s\n' "$digest" "$destination" | sha256sum --check --status || fail 'tool checksum mismatch'
}
download https://github.com/kubernetes-sigs/kind/releases/download/v0.33.0/kind-linux-amd64 \
  "$work/kind" aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
download https://dl.k8s.io/release/v1.36.2/bin/linux/amd64/kubectl \
  "$work/kubectl" 1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82
download https://get.helm.sh/helm-v4.2.0-linux-amd64.tar.gz \
  "$work/helm.tar.gz" 97dbeb971be4ac4b27e3839976d9564c0fb35c6f3b1da89dd1e292d236af4096
tar -xzf "$work/helm.tar.gz" -C "$work" linux-amd64/helm
chmod 0700 "$work/kind" "$work/kubectl" "$work/linux-amd64/helm"
export PATH="$work:$work/linux-amd64:$PATH"
gateway="$(go -C "$root/scripts/tests/webapp-compiler" mod download -json sigs.k8s.io/gateway-api | jq -er .Dir)"
[[ "$(go -C "$root/scripts/tests/webapp-compiler" list -m -f '{{.Version}}' sigs.k8s.io/gateway-api)" == v1.4.1 ]] || fail 'unreviewed route schema'

# Verify the original source-owned CI, image signature and complete provenance
# with the maintained catalogue helper. Only this temporary manifest is mutated.
download https://raw.githubusercontent.com/devantler-tech/.github/eec706368bdc0114c010afea778bf5efb17ced23/.github/scripts/prepare-prebuilt-webapp.sh \
  "$work/verify-image.sh" dcf6fee210077e12f9e147b76eb78e5f4a9c161ace2df7ffeb49ac30a6c08961
mkdir "$work/deploy"
jq -n '{apiVersion:"kro.run/v1alpha1",kind:"WebApp",metadata:{name:"runtime-fixture",namespace:"runtime-fixture"},spec:{name:"runtime-fixture",host:"runtime-fixture.invalid",image:"verified-before-use",replicas:1,port:8080,probePath:"/healthz",memoryRequest:"64Mi",memoryLimit:"256Mi",cpuLimit:"500m"}}' >"$work/deploy/webapp.yaml"
(
  cd "$work"
  REPOSITORY=devantler-tech/business-site SOURCE_SHA=1d34013bb6d40dbf1736c21d677734fe27bfc982 \
    APP_NAME=runtime-fixture DEPLOY_PATH=deploy \
    PREBUILT_IMAGE_DIGEST=sha256:5fd88737e82502d9bc63dbb1248cb3cf97f6afb93ed4ab1c0cfe93f7e341c06b \
    PREBUILT_PUBLIC_DIGEST=b362f95711b9b9fc285335bb19c1a206ee0856eb42a79e39874e653b4735e593 \
    PREBUILT_CI_RUN_ID=39603006665 PREBUILT_CI_RUN_ATTEMPT=1 GITHUB_OUTPUT="$work/verified" \
    bash "$work/verify-image.sh" >"$work/verification.log" 2>&1
) || fail 'source-owned public image verification failed'
yq -o=json '.' "$work/deploy/webapp.yaml" >"$work/omitted.json"
jq '.spec += {args:["--public","/app/public","--listen","0.0.0.0:8080","--company-disclosure-projection","/run/webapp-inputs/disclosure"],runAsUser:65532,runAsGroup:65532,readOnlyRootFilesystem:true,secretProjections:[{secretName:"synthetic-disclosure",items:[{key:"disclosure",path:"disclosure"}]}]}' \
  "$work/omitted.json" >"$work/enabled.json"
bash "$root/scripts/validate-webapp-inputs.sh" "$work/enabled.json"
created=true
kind create cluster --name "$fixture" --kubeconfig "$KUBECONFIG" \
  --image kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed --wait 120s
kubectl apply -f "$gateway/config/crd/standard/gateway.networking.k8s.io_httproutes.yaml" >/dev/null
helm install kro oci://registry.k8s.io/kro/charts/kro@sha256:bafeda52939e319d231ad8d59d082d4f68f5bccad2d8b9ea0433bc872609e732 \
  --namespace kro-system --create-namespace --set fullnameOverride=kro --set rbac.mode=aggregation \
  --set config.allowCRDDeletion=false --set config.featureGates.CELOmitFunction=false \
  --set deployment.replicaCount=1 \
  --set-string image.tag=v0.9.2@sha256:4adde767d11163f3064a3431ab6bb87c8dc70498fdaac3109d9b9df7ffa494e3 \
  --wait --timeout 180s >"$work/helm.log"
kubectl -n kro-system get deployment kro -o json | jq -e '
  [.spec.template.spec.containers[].image] ==
  ["registry.k8s.io/kro/kro:v0.9.2@sha256:4adde767d11163f3064a3431ab6bb87c8dc70498fdaac3109d9b9df7ffa494e3"]' >/dev/null || fail 'installed controller image differs from the reviewed pin'
kubectl apply -f "$root/k8s/bases/infrastructure/resource-graph-definitions/webapp/cluster-role.yaml" >/dev/null
kubectl apply -f "$root/k8s/bases/infrastructure/resource-graph-definitions/webapp/resource-graph-definition.yaml" >/dev/null
for _attempt in {1..60}; do
  if kubectl get crd/webapps.kro.run >/dev/null 2>&1; then break; fi
  sleep 2
done
kubectl wait --for=condition=Established crd/webapps.kro.run --timeout=120s
kubectl create namespace runtime-fixture >/dev/null
kubectl label namespace runtime-fixture pod-security.kubernetes.io/enforce=restricted >/dev/null
printf 'Example & Partners Road 1\n1234 Exampletown\n' >"$work/disclosure"
kubectl -n runtime-fixture create secret generic synthetic-disclosure --from-file="disclosure=$work/disclosure" >/dev/null

# Observe actual controller convergence in both directions, not just resolved CEL.
# The production-template image intentionally cannot serve without disclosure;
# omitted-state assertions concern the generated template, not healthy startup.
converged() {
  local state="$1" _attempt previous='' generation stable=0
  for _attempt in {1..60}; do
    if kubectl -n runtime-fixture get deployment runtime-fixture -o json >"$work/deployment.json" 2>/dev/null &&
      jq -e --arg state "$state" '
        select(.status.observedGeneration == .metadata.generation) |
        .spec.template.spec as $s | $s.containers[0] as $c |
        if $state == "omitted" then
          ($c.args // []) == [] and ($c.volumeMounts // []) == [] and ($s.volumes // []) == [] and
          $s.securityContext.runAsUser == 1000 and $s.securityContext.runAsGroup == 1000 and
          $c.securityContext.readOnlyRootFilesystem == false
        else
          $s.securityContext.runAsUser == 65532 and $s.securityContext.runAsGroup == 65532 and
          $s.securityContext.fsGroup == 65532 and $c.securityContext.readOnlyRootFilesystem == true and
          $c.args[-1] == "/run/webapp-inputs/disclosure" and
          $s.volumes == [{name:"webapp-inputs",secret:{secretName:"synthetic-disclosure",defaultMode:288,optional:false,items:[{key:"disclosure",path:"disclosure"}]}}] and
          $c.volumeMounts == [{name:"webapp-inputs",mountPath:"/run/webapp-inputs",readOnly:true}]
        end' "$work/deployment.json" >/dev/null; then
      generation="$(jq -er .metadata.generation "$work/deployment.json")"
      if [[ "$generation" == "$previous" ]]; then stable=$((stable + 1)); else stable=1; fi
      previous="$generation"
      if [[ "$stable" -ge 2 ]]; then return 0; fi
    else
      stable=0
    fi
    sleep 2
  done
  fail 'controller did not converge to the expected template'
}
kubectl apply --validate=strict -f "$work/omitted.json" >/dev/null
converged omitted
kubectl apply --validate=strict -f "$work/enabled.json" >/dev/null
converged enabled
kubectl -n runtime-fixture rollout status deployment/runtime-fixture --timeout=180s
for _attempt in {1..60}; do
  if kubectl -n runtime-fixture get service/runtime-fixture httproute/runtime-fixture >/dev/null 2>&1; then break; fi
  sleep 2
done
kubectl -n runtime-fixture get service/runtime-fixture httproute/runtime-fixture >/dev/null
kubectl -n runtime-fixture get pods -l app.kubernetes.io/name=runtime-fixture -o json >"$work/pods.json"
jq -e '[.items[] | select(.status.phase=="Running") | .status.containerStatuses[] |
  select(.ready==true and .user.linux.uid==65532 and .user.linux.gid==65532)] | length==1' "$work/pods.json" >/dev/null || fail 'kubelet did not report the expected running process identity'
pod="$(jq -er '[.items[] | select(any(.status.containerStatuses[]?; .ready==true))] | if length==1 then .[0].metadata.name else error("pod count") end' "$work/pods.json")"
uid="$(kubectl -n runtime-fixture get pod "$pod" -o jsonpath='{.metadata.uid}')"
[[ "$uid" =~ ^[a-f0-9-]{36}$ ]] || fail 'invalid owned Pod UID'
# Inspect only the fictional Secret's kubelet projection on our owned node.
docker exec "$fixture-control-plane" sh -eu -c '
  cd "/var/lib/kubelet/pods/$1/volumes/kubernetes.io~secret/webapp-inputs"
  test "$(readlink disclosure)" = "..data/disclosure"
  test -L ..data && test -f disclosure
  test "$(stat -L -c "%u:%g:%a" disclosure)" = "0:65532:440"
' sh "$uid"
kubectl -n runtime-fixture port-forward "pod/$pod" 18080:8080 >"$work/forward.log" 2>&1 &
forward_pid=$!
for _attempt in {1..30}; do
  if curl -fsS --max-time 2 http://127.0.0.1:18080/healthz >"$work/health" 2>/dev/null; then break; fi
  sleep 1
done
printf 'ok\n' >"$work/expected-health"
cmp -s "$work/health" "$work/expected-health" || fail 'real application health response differs'
for path in /company/ /da/company/; do
  curl -fsS --max-time 10 "http://127.0.0.1:18080$path" >"$work/page"
  if ! grep -q 'Example &amp; Partners Road 1' "$work/page" || ! grep -q '1234 Exampletown' "$work/page"; then fail 'synthetic disclosure not rendered'; fi
  ! grep -q '__BUSINESS_ADDRESS_LINE_' "$work/page" || fail 'unresolved disclosure slot'
done
code="$(curl -sS --max-time 10 -D "$work/headers" -o "$work/private" -w '%{http_code}' http://127.0.0.1:18080/portal/)"
if [[ "$code" != 404 ]] || ! grep -iq '^Cache-Control: no-store' "$work/headers" || grep -iq '^Location:' "$work/headers"; then fail 'private access is not closed'; fi
kill "$forward_pid"; wait "$forward_pid" 2>/dev/null || true; forward_pid=''

# Real derived Pods distinguish reader refusal from OOM or another startup error.
for control in mode group missing; do
  jq --arg control "$control" '.spec.template | .apiVersion="v1" | .kind="Pod" |
    .metadata={name:("unsafe-"+$control),namespace:"runtime-fixture"} | .spec.restartPolicy="Never" |
    del(.spec.containers[0].livenessProbe,.spec.containers[0].readinessProbe) |
    if $control=="mode" then .spec.volumes[0].secret.defaultMode=292
    elif $control=="group" then .spec.securityContext.runAsGroup=65531
    else .spec.containers[0].args[-1]="/run/webapp-inputs/missing" end' "$work/deployment.json" >"$work/unsafe.json"
  kubectl apply --validate=strict -f "$work/unsafe.json" >/dev/null
  kubectl -n runtime-fixture wait --for=jsonpath='{.status.phase}'=Failed "pod/unsafe-$control" --timeout=120s
  kubectl -n runtime-fixture get pod "unsafe-$control" -o json | jq -e '
    .status.containerStatuses[0] | .ready == false and
    .state.terminated.exitCode == 1 and .state.terminated.reason == "Error"' >/dev/null
  kubectl -n runtime-fixture logs "unsafe-$control" >"$work/refusal.log"
  grep -Fxq 'company disclosure projection is unavailable' "$work/refusal.log" || fail 'negative Pod did not report the actual projection refusal'
  if grep -Eq 'Exampletown|Partners Road|listening' "$work/refusal.log"; then fail 'negative Pod disclosed input or opened a listener'; fi
  kubectl -n runtime-fixture delete pod "unsafe-$control" --wait=true >/dev/null
done
jq '.metadata.name="unsafe-input" | .spec.secretProjections[0].items[0].path="../outside"' "$work/enabled.json" >"$work/unsafe-input.json"
if kubectl create --dry-run=server --validate=strict -f "$work/unsafe-input.json" >"$work/input-refusal.log" 2>&1; then fail 'real generated API accepted a traversal destination'; fi
grep -Fq 'Invalid' "$work/input-refusal.log" || fail 'unsafe destination did not receive API validation refusal'

# A selected key absent from the actual Secret must never start the executable.
jq '.spec.template | .apiVersion="v1" | .kind="Pod" | .metadata={name:"missing-key",namespace:"runtime-fixture"} |
  .spec.restartPolicy="Never" | .spec.volumes[0].secret.items[0].key="absent"' "$work/deployment.json" >"$work/missing-key.json"
kubectl apply --validate=strict -f "$work/missing-key.json" >/dev/null
missing_uid="$(kubectl -n runtime-fixture get pod missing-key -o jsonpath='{.metadata.uid}')"
mount_refused=false
for _attempt in {1..60}; do
  if kubectl -n runtime-fixture get events --field-selector "involvedObject.uid=$missing_uid" -o json |
    jq -e 'any(.items[]; .reason=="FailedMount" and (.message|contains("non-existent secret key")))' >/dev/null; then mount_refused=true; break; fi
  sleep 2
done
[[ "$mount_refused" == true ]] || fail 'missing selected key did not produce an observed kubelet mount refusal'
kubectl -n runtime-fixture get pod missing-key -o json | jq -e '[.status.containerStatuses[]? | select(.state.running!=null or .state.terminated!=null)] | length==0' >/dev/null
kubectl -n runtime-fixture delete pod missing-key --wait=true >/dev/null
kubectl apply --validate=strict -f "$work/omitted.json" >/dev/null
converged omitted
# A zero-unavailable rollout may keep the healthy old Pod when new startup
# lacks required disclosure. Template omission is not workload revocation.
jq '.spec.replicas=0' "$work/omitted.json" >"$work/stopped.json"
kubectl apply --validate=strict -f "$work/stopped.json" >/dev/null
for _attempt in {1..60}; do
  if [[ "$(kubectl -n runtime-fixture get pods -l app.kubernetes.io/name=runtime-fixture -o json | jq '.items|length')" == 0 ]]; then
    echo 'PASS: real KRO template transitions, restricted process, atomic 0440 projection, EN/DA public image, exact refusal and closed private routes; fixture scaled to zero (Kubernetes 1.36.4; no production ingress).'
    cleanup
    trap - EXIT
    exit 0
  fi
  sleep 2
done
fail 'owned fixture Pods did not stop after explicit scale-to-zero'
