#!/usr/bin/env bash

# Behaviour contract for Coroot's declarative Applications-view health policy.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-application-health-reconciler.yaml"
readonly kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/kustomization.yaml"
readonly talos_patch="${root_dir}/talos/cluster/gc-terminated-pods-sooner.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'ok — %s\n' "$1"
}

for tool in yq jq; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

[ -f "${manifest}" ] || fail "manifest not found: ${manifest}"
grep -Fq 'coroot/cron-job-application-health-reconciler.yaml' "${kustomization}" ||
  fail 'the production overlay must render the application-health reconciler'
grep -Fq "'scripts/tests/test-coroot-application-health-reconciler.sh'" "${ci_workflow}" ||
  fail 'the application-health contract must trigger CI'
grep -Fq 'bash scripts/tests/test-coroot-application-health-reconciler.sh' "${ci_workflow}" ||
  fail 'CI must execute the application-health contract'

script_body="$(yq eval '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' "${manifest}")"
if [ -z "${script_body}" ] || [ "${script_body}" = "null" ]; then
  fail 'could not extract the reconciler script'
fi
# shellcheck disable=SC2016
[[ "${script_body}" != *'${'* ]] ||
  fail 'the reconciler contains a Flux-consumable shell expansion'
# The embedded script must contain this literal variable reference.
# shellcheck disable=SC2016
[[ "${script_body}" == *'reconcile_threshold "$KUBELET" NetworkTCPConnections 0 inherit kubelet-probe-source-fixed'* ]] ||
  fail 'the reconciler must remove the obsolete kubelet item-check threshold'
[[ "${script_body}" != *'kubelet-static-pod-probes'* ]] ||
  fail 'item-based Coroot checks ignore thresholds; kubelet probe failures must be fixed at source'

yq eval -e '.cluster.controllerManager.extraArgs."log-text-split-stream" == "false"' \
  "${talos_patch}" >/dev/null ||
  fail 'the controller-manager patch must retain GC and force a static-pod refresh'
yq eval -e '.cluster.controllerManager.extraArgs."terminated-pod-gc-threshold" == "100"' \
  "${talos_patch}" >/dev/null ||
  fail 'the controller-manager refresh must preserve the tuned PodGC threshold'
yq eval -e '.cluster.controllerManager.extraArgs."bind-address" == "::1"' \
  "${talos_patch}" >/dev/null ||
  fail 'the controller-manager health endpoint must match the IPv6-first localhost probe'
yq eval -e '.cluster.scheduler.extraArgs."bind-address" == "::1"' \
  "${talos_patch}" >/dev/null ||
  fail 'the scheduler health endpoint must match the IPv6-first localhost probe'
pass 'control-plane probe targets match loopback-only component listeners'

work_root="$(mktemp -d /tmp/tmp.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT

setup_scenario() {
  local name="$1" dex_mode="$2" dex_current="$3"
  local state_error="${4:-context deadline exceeded}"
  local init_current="${5:-null}"
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin"
  printf '%s' "${dex_mode}" >"${dir}/dex-mode"
  printf '%s' "${dex_current}" >"${dir}/dex-current"
  printf '%s' "${state_error}" >"${dir}/state-error"
  printf '%s' "${init_current}" >"${dir}/init-current"

  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${SCENARIO_DIR}"
url=""
payload=""
method="GET"
prev=""
for arg in "$@"; do
  [ "${prev}" = "-d" ] && payload="${arg}"
  [ "${prev}" = "-X" ] && method="${arg}"
  case "${arg}" in http://*) url="${arg}" ;; esac
  prev="${arg}"
done

case "${url}" in
  */api/user)
    printf '%s\n' '{"data":{"projects":[{"id":"95rsc5yp","name":"platform"}]}}'
    ;;
  */overview/logs?query=*)
    jq -cn --arg error "$(cat "${dir}/state-error")" '{data:{logs:{entries:[{
      message:"get state for fe039a7e0bb7e1ce7637b0e33da1c6776268dcb222f92e4c79374af54e5c569d",
      attributes:{error:$error,"service.name":"/talos/init"}
    }]}}}'
    ;;
  */logs?query=*)
    if [[ "$url" == *'%3Aobservability%3ACronJob%3Acoroot-alert-autosuppressor'* ]]; then
      cat <<'JSON'
{"data":{"patterns":[{"severity":"fatal","sample":"suppressed 3 by-design alert(s)"},{"severity":"fatal","sample":"no by-design alerts to suppress"}]}}
JSON
    elif [[ "$url" == *'%3Adex%3ADeployment%3Adex'* ]]; then
      if [ "$(cat "${dir}/dex-mode")" = "known" ]; then
        cat <<'JSON'
{"data":{"patterns":[{"severity":"error","sample":"time=2026-09-19T16:24:05.370Z level=ERROR msg=\"failed to parse authorization request\" err=\"Invalid client_id (\\\"\\\").\" request_id=af4e2eb0-e04b-41cc-93f0-a5f8f808e2ef"}]}}
JSON
      elif [ "$(cat "${dir}/dex-mode")" = "unknown" ]; then
        printf '%s\n' '{"data":{"patterns":[{"severity":"error","sample":"database connection refused"}]}}'
      else
        printf '%s\n' '{"data":{"patterns":null}}'
      fi
    elif [[ "$url" == *'%3A_%3AUnknown%3Ainit'* ]]; then
      cat <<'JSON'
{"data":{"patterns":[
  {"severity":"error","sample":"kern:     err: [2026-09-19T15:57:41.650693675Z]: audit: error in audit_log_subj_ctx"},
  {"severity":"error","sample":"ContainerStatus from runtime service failed"},
  {"severity":"error","sample":"DeleteContainer returned error"},
  {"severity":"error","sample":"ContainerStatus for \"234fe59bd2768b7b43eacd07506553819d88c4d05687ed2c9ab191bb0948e2e4\" failed"},
  {"severity":"fatal","sample":"Observed pod startup duration"},
  {"severity":"error","sample":"time=\"2026-09-19T16:45:07.668204590Z\" level=error msg=\"failed sending message on channel\" error=\"write unix /run/containerd/s/id->@: write: broken pipe\" runtime=io.containerd.runc.v2"},
  {"severity":"error","sample":"collecting metrics for 8015f69b6b683d1b1c0eb5187bdfa57e1193bb07cc1e12eff8f3b61354312539"},
  {"severity":"error","sample":"get state for fe039a7e0bb7e1ce7637b0e33da1c6776268dcb222f92e4c79374af54e5c569d"},
  {"severity":"error","sample":"ttrpc: received message on inactive stream"}
]}}
JSON
    elif [[ "$url" == *'%3Akube-system%3AStaticPods%3Akube-apiserver'* ]]; then
      cat <<'JSON'
{"data":{"patterns":[
  {"severity":"error","sample":"E0919 16:32:49.937385       1 status.go:71] \"Unhandled Error\" err=\"apiserver received an error that is not an metav1.Status: &errors.errorString{s:\\\"context canceled\\\"}: context canceled\" logger=\"UnhandledError\""},
  {"severity":"fatal","sample":"{\"kind\":\"Event\",\"apiVersion\":\"audit.k8s.io/v1\",\"level\":\"Metadata\",\"auditID\":\"1845fc58-1235-49f3-bb89-52dd44151383\",\"stage\":\"RequestReceived\",\"requestURI\":\"/api/v1/namespaces/kubescape/secrets/sh.helm.release.v1.alertmanager.v7\",\"verb\":\"get\",\"user\":{\"username\":\"system:serviceaccount:flux-system:helm-controller\"},\"objectRef\":{\"resource\":\"secrets\",\"namespace\":\"kubescape\",\"name\":\"sh.helm.release.v1.alertmanager.v7\",\"apiVersion\":\"v1\"}}"},
  {"severity":"error","sample":"E0919 16:32:49.939927       1 timeout.go:140] \"Post-timeout activity\" logger=\"UnhandledError\" timeElapsed=\"2.920993ms\" method=\"GET\" path=\"/apis/batch/v1/namespaces/openbao/jobs/vault-snapshot-init\" result=null"}
]}}
JSON
    elif [[ "$url" == *'%3Akube-system%3AStaticPods%3Akube-controller-manager'* ]]; then
      cat <<'JSON'
{"data":{"patterns":[
  {"severity":"error","sample":"E0919 15:54:02.455011       1 reflector.go:227] \"Failed to watch\" err=\"failed to list *v1.PartialObjectMetadata: the server could not find the requested resource\" logger=\"UnhandledError\" reflector=\"k8s.io/client-go/metadata/metadatainformer/informer.go:146\" type=\"*v1.PartialObjectMetadata\""},
  {"severity":"error","sample":"E0919 15:54:57.858842       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"flux-system/kustomize-controller-9895f7fb8\\\" failed with read version: 295731570 is not as new as written version: 295731572 for group resource replicasets.apps\" logger=\"UnhandledError\""}
]}}
JSON
    elif [[ "$url" == *'%3Avertical-pod-autoscaler%3ADeployment%3Avertical-pod-autoscaler-vpa-updater'* ]]; then
      cat <<'JSON'
{"data":{"patterns":[{"severity":"error","sample":"E0919 17:00:42.892044       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"events \\\"pod.123\\\" is forbidden: User \\\"system:serviceaccount:vertical-pod-autoscaler:vertical-pod-autoscaler-vpa-updater\\\" cannot patch resource \\\"events\\\" in API group \\\"\\\" in the namespace \\\"kubescape\\\"\" event=\"&Event{Reason:InPlaceResizedByVPA,Message:Pod was resized in place by VPA Updater.}\""}]}}
JSON
    else
      printf '%s\n' '{"data":{"patterns":[]}}'
    fi
    ;;
  */inspection/*/config)
    if [ "${method}" = "POST" ]; then
      jq -cn --arg url "${url}" --argjson body "${payload}" '{url:$url,body:$body}' >>"${dir}/posts.ndjson"
    elif [[ "${url}" == *'/MemoryLeakPercent/'* ]]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":10},null,null]}}'
    elif [[ "${url}" == *'%3A_%3AUnknown%3Akubelet/inspection/NetworkTCPConnections/config'* ]]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":3}]}}'
    elif [[ "${url}" == *'%3A_%3AUnknown%3Ainit'* ]]; then
      current="$(cat "${dir}/init-current")"
      if [ "${current}" = "null" ]; then
        printf '%s\n' '{"form":{"configs":[{"threshold":0},null,null]}}'
      else
        printf '{"form":{"configs":[{"threshold":0},null,{"threshold":%s}]}}\n' "${current}"
      fi
    elif [[ "${url}" == *'%3Adex%3ADeployment%3Adex'* ]]; then
      current="$(cat "${dir}/dex-current")"
      if [ "${current}" = "null" ]; then
        printf '%s\n' '{"form":{"configs":[{"threshold":0},null,null]}}'
      else
        printf '{"form":{"configs":[{"threshold":0},null,{"threshold":%s}]}}\n' "${current}"
      fi
    else
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,null]}}'
    fi
    ;;
  *)
    printf 'unstubbed curl URL: %s\n' "${url}" >&2
    exit 1
    ;;
esac
STUB
  chmod +x "${dir}/bin/curl"
  printf '%s' "${dir}"
}

run_scenario() {
  local dir="$1"
  SCENARIO_DIR="${dir}" PATH="${dir}/bin:${PATH}" /bin/sh -c "${script_body}"
}

known_dir="$(setup_scenario known known null)"
known_output="$(run_scenario "${known_dir}")"
printf '%s\n' "${known_output}" | jq -s -e \
  'length > 0 and all(.[]; .level == "info" and (.msg | type == "string" and length > 0))' \
  >/dev/null || fail 'successful reconciliation must emit structured info JSON'
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Akubelet/inspection/NetworkTCPConnections/config")) and .body.configs[2] == null) and
  any(.[]; (.url | contains("%3Akyverno%3ADeployment%3Akyverno-background-controller/inspection/MemoryLeakPercent/config")) and .body.configs[2].threshold == 35) and
  any(.[]; (.url | contains("%3Acrossplane-system%3ADeployment%3Acrossplane/inspection/MemoryLeakPercent/config")) and .body.configs[2].threshold == 35) and
  any(.[]; (.url | contains("%3Aobservability%3ACronJob%3Acoroot-alert-autosuppressor/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2].threshold == 1000) and
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-apiserver/inspection/LogErrors/config")) and .body.configs[2].threshold == 100) and
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-controller-manager/inspection/LogErrors/config")) and .body.configs[2].threshold == 5000) and
  any(.[]; (.url | contains("%3Avertical-pod-autoscaler%3ADeployment%3Avertical-pod-autoscaler-vpa-updater/inspection/LogErrors/config")) and .body.configs[2].threshold == 10)
' "${known_dir}/posts.ndjson" >/dev/null ||
  fail 'reviewed evidence did not produce the exact app-level policies'
pass 'reviewed application-health signals receive narrow app-level policies'

unknown_dir="$(setup_scenario unknown unknown 1)"
run_scenario "${unknown_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${unknown_dir}/posts.ndjson" >/dev/null ||
  fail 'an unknown Dex error did not remove the reviewed app-level threshold'
pass 'an unreviewed log pattern fails closed and becomes visible'

runtime_dir="$(setup_scenario runtime known null 'permission denied' 1000)"
run_scenario "${runtime_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${runtime_dir}/posts.ndjson" >/dev/null ||
  fail 'a get-state error without exact deadline evidence did not remain visible'
pass 'a runtime state error requires exact timeout evidence'

aged_dir="$(setup_scenario aged none 10)"
run_scenario "${aged_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${aged_dir}/posts.ndjson" >/dev/null ||
  fail 'an aged-out Dex pattern did not remove its app-level threshold'
pass 'an aged-out pattern returns to the global zero threshold'
