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
[[ "${script_body}" == *'view:"messages"'* ]] ||
  fail 'the reconciler must validate raw log messages'
[[ "${script_body}" != *'view:"patterns"'* ]] ||
  fail 'representative Coroot patterns must not stand in for every raw error'

# The Cilium application owns the cluster-wide DNS proxy, so Coroot attributes
# every workload's resolver search candidate to that DaemonSet. This exact,
# finite threshold complements the alert-side exemption without changing the
# Cilium agent's own resolver configuration (which does not rewrite proxied DNS
# packets).
# shellcheck disable=SC2016
[[ "${script_body}" == *'reconcile_threshold "$CILIUM" DnsNxdomainErrors 0 7500 cilium-dns-search-expansion'* ]] ||
  fail 'the Cilium DNS proxy aggregation must have a finite app-level threshold'
pass 'the Cilium DNS proxy aggregation has a narrow application policy'

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
  local controller_mode="${6:-known}"
  local response_mode="${7:-complete}"
  local operator_mode="${8:-known}"
  local storage_mode="${9:-known}"
  local cnpg_mode="${10:-known}"
  local csi_controller_mode="${11:-known}"
  local csi_node_mode="${12:-known}"
  local runtime_attributes_mode="${13:-valid}"
  local sandbox_error="${14:-rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found}"
  local sandbox_attributes_mode="${15:-valid}"
  local sandbox_service="${16:-/talos/init}"
  local kustomize_mode="${17:-known}"
  local autoscaler_mode="${18:-known}"
  local autosuppressor_mode="${19:-present}"
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin"
  printf '%s' "${dex_mode}" >"${dir}/dex-mode"
  printf '%s' "${dex_current}" >"${dir}/dex-current"
  printf '%s' "${state_error}" >"${dir}/state-error"
  printf '%s' "${init_current}" >"${dir}/init-current"
  printf '%s' "${controller_mode}" >"${dir}/controller-mode"
  printf '%s' "${response_mode}" >"${dir}/response-mode"
  printf '%s' "${operator_mode}" >"${dir}/operator-mode"
  printf '%s' "${storage_mode}" >"${dir}/storage-mode"
  printf '%s' "${cnpg_mode}" >"${dir}/cnpg-mode"
  printf '%s' "${csi_controller_mode}" >"${dir}/csi-controller-mode"
  printf '%s' "${csi_node_mode}" >"${dir}/csi-node-mode"
  printf '%s' "${runtime_attributes_mode}" >"${dir}/runtime-attributes-mode"
  printf '%s' "${sandbox_error}" >"${dir}/sandbox-error"
  printf '%s' "${sandbox_attributes_mode}" >"${dir}/sandbox-attributes-mode"
  printf '%s' "${sandbox_service}" >"${dir}/sandbox-service"
  printf '%s' "${kustomize_mode}" >"${dir}/kustomize-mode"
  printf '%s' "${autoscaler_mode}" >"${dir}/autoscaler-mode"
  printf '%s' "${autosuppressor_mode}" >"${dir}/autosuppressor-mode"

  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${SCENARIO_DIR}"
url=""
payload=""
method="GET"
write_out=""
prev=""
for arg in "$@"; do
  [ "${prev}" = "-d" ] && payload="${arg}"
  [ "${prev}" = "-X" ] && method="${arg}"
  { [ "${prev}" = "-w" ] || [ "${prev}" = "--write-out" ]; } && write_out="${arg}"
  case "${arg}" in http://*) url="${arg}" ;; esac
  prev="${arg}"
done

case "${url}" in
  */api/user)
    printf '%s\n' '{"data":{"projects":[{"id":"95rsc5yp","name":"platform"}]}}'
    ;;
  */logs?query=*)
    severity="error"
    [[ "${url}" == *'%22value%22%3A%22fatal%22'* ]] && severity="fatal"
    if [[ "$url" == *'%3Aobservability%3ACronJob%3Acoroot-alert-autosuppressor'* ]]; then
      if [ "$(cat "${dir}/autosuppressor-mode")" = "absent" ]; then
        [ -z "${write_out}" ] || printf '404'
        exit 22
      elif [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"fatal","message":"suppressed 3 by-design alert(s)"},{"severity":"fatal","message":"no by-design alerts to suppress"}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      fi
    elif [[ "$url" == *'%3Adex%3ADeployment%3Adex'* ]]; then
      if [ "${severity}" = "fatal" ] || [ "$(cat "${dir}/dex-mode")" = "none" ]; then
        # Coroot serializes an empty messages result as null, not [].
        printf '%s\n' '{"data":{"status":"ok","entries":null}}'
      elif [ "$(cat "${dir}/dex-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=2026-09-19T16:24:05.370Z level=ERROR msg=\"failed to parse authorization request\" err=\"Invalid client_id (\\\"\\\").\" request_id=af4e2eb0-e04b-41cc-93f0-a5f8f808e2ef"}]}}'
      elif [ "$(cat "${dir}/dex-mode")" = "unknown" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"database connection refused"}]}}'
      fi
    elif [[ "$url" == *'%3A_%3AUnknown%3Ainit'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"fatal","message":"Observed pod startup duration"}]}}'
      elif [ "$(cat "${dir}/runtime-attributes-mode")" = "malformed" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"UpdatePodSandboxResources from runtime service failed","attributes":"malformed"},
          {"severity":"error","message":"get state for fe039a7e0bb7e1ce7637b0e33da1c6776268dcb222f92e4c79374af54e5c569d","attributes":[]}
        ]}}'
      else
        jq -cn \
          --arg state_error "$(cat "${dir}/state-error")" \
          --arg sandbox_error "$(cat "${dir}/sandbox-error")" \
          --arg sandbox_attributes_mode "$(cat "${dir}/sandbox-attributes-mode")" \
          --arg sandbox_service "$(cat "${dir}/sandbox-service")" \
          '{data:{status:"ok",entries:[
          {severity:"error",message:"kern:     err: [2026-09-19T15:57:41.650693675Z]: audit: error in audit_log_subj_ctx"},
          {severity:"error",message:"ContainerStatus from runtime service failed"},
          {severity:"error",message:"DeleteContainer returned error"},
          {severity:"error",message:"UpdatePodSandboxResources from runtime service failed",attributes:{caller:"pkg/remote_runtime.go:755",err:"rpc error: code = Unimplemented desc = not implemented",podSandboxID:"bf0dda88c47fdbd22aa75719596b0426ac464c337a45a30bdf61404b598f4d2f","service.name":"/talos/init"}},
          {severity:"error",message:"ContainerStatus for \"234fe59bd2768b7b43eacd07506553819d88c4d05687ed2c9ab191bb0948e2e4\" failed"},
          {severity:"error",message:"time=\"2026-09-19T16:45:07.668204590Z\" level=error msg=\"failed sending message on channel\" error=\"write unix /run/containerd/s/id->@: write: broken pipe\" runtime=io.containerd.runc.v2"},
          {severity:"error",message:"collecting metrics for 8015f69b6b683d1b1c0eb5187bdfa57e1193bb07cc1e12eff8f3b61354312539"},
          {severity:"error",message:"get state for fe039a7e0bb7e1ce7637b0e33da1c6776268dcb222f92e4c79374af54e5c569d",attributes:{error:$state_error,"service.name":"/talos/init"}},
          {severity:"error",message:"PodSandboxStatus for \"108e361acaee281aa29db245fbb2672f111c881c05d81eb70ddc4ba3457a9ec4\" failed",attributes:(
            if $sandbox_attributes_mode=="scalar" then "malformed"
            elif $sandbox_attributes_mode=="array" then []
            else {error:$sandbox_error,"service.name":$sandbox_service}
            end
          )},
          {severity:"error",message:"ttrpc: received message on inactive stream"},
          {severity:"error",message:"Error while processing event (\"/sys/fs/cgroup/kubepods/burstable/pod9422b478-cafc-4a4d-83c2-94aea6221faa/3fc98769277298bd403f6e42666164c725bcb49636e81bd3b88ac102e481ce0d\": 0x40000100 == IN_CREATE|IN_ISDIR): inotify_add_watch /sys/fs/cgroup/kubepods/burstable/pod9422b478-cafc-4a4d-83c2-94aea6221faa/3fc98769277298bd403f6e42666164c725bcb49636e81bd3b88ac102e481ce0d: no such file or directory"}
        ]}}'
      fi
    elif [[ "$url" == *'%3Akube-system%3AStaticPods%3Akube-apiserver'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"fatal","message":"{\"kind\":\"Event\",\"apiVersion\":\"audit.k8s.io/v1\",\"level\":\"Metadata\",\"auditID\":\"1845fc58-1235-49f3-bb89-52dd44151383\",\"stage\":\"RequestReceived\",\"requestURI\":\"/api/v1/namespaces/kubescape/secrets/sh.helm.release.v1.alertmanager.v7\",\"verb\":\"get\",\"user\":{\"username\":\"system:serviceaccount:flux-system:helm-controller\"},\"objectRef\":{\"resource\":\"secrets\",\"namespace\":\"kubescape\",\"name\":\"sh.helm.release.v1.alertmanager.v7\",\"apiVersion\":\"v1\"}}"}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"E0919 16:32:49.937385       1 status.go:71] \"Unhandled Error\" err=\"apiserver received an error that is not an metav1.Status: &errors.errorString{s:\\\"context canceled\\\"}: context canceled\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0920 01:01:17.166158       1 writers.go:123] \"Unhandled Error\" err=\"apiserver was unable to write a JSON response: http: Handler timeout\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0920 01:01:17.167192       1 status.go:71] \"Unhandled Error\" err=\"apiserver received an error that is not an metav1.Status: &errors.errorString{s:\\\"http: Handler timeout\\\"}: http: Handler timeout\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0920 01:01:17.167224       1 writers.go:136] \"Unhandled Error\" err=\"apiserver was unable to write a fallback JSON response: http: Handler timeout\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0919 16:32:49.939927       1 timeout.go:140] \"Post-timeout activity\" logger=\"UnhandledError\" timeElapsed=\"2.920993ms\" method=\"GET\" path=\"/apis/batch/v1/namespaces/openbao/jobs/vault-snapshot-init\" result=null"},
          {"severity":"error","message":"E0919 22:57:04.882632       1 controller.go:123] \"Unhandled Error\" err=\"loading OpenAPI spec for \\\"v1beta1.metrics.k8s.io\\\" failed with: Error, could not get list of group versions for APIService\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0919 22:56:58.072707       1 wrap.go:53] \"Timeout or abort while handling\" logger=\"UnhandledError\" method=\"GET\" URI=\"/api/v1/namespaces/kube-system/configmaps/tetragon-operator-config\" auditID=\"e422a037-b8da-4b60-8982-52839b17c40a\""},
          {"severity":"error","message":"E0920 01:10:57.256030       1 wrap.go:53] \"Timeout or abort while handling\" logger=\"UnhandledError\" method=\"GET\" URI=\"/apis/spdx.softwarecomposition.kubescape.io/v1beta1/sbomsyfts?watch=true\" auditID=\"ad9ad3fe-66f1-41fd-a686-f6de2d3205fd\""},
          {"severity":"error","message":"E0920 00:24:30.657865       1 wrap.go:53] \"Timeout or abort while handling\" logger=\"UnhandledError\" method=\"GET\" URI=\"/apis/spdx.softwarecomposition.kubescape.io/v1beta1/containerprofiles?watch=true\" auditID=\"aaa60eeb-ca9d-4258-b1a1-d52481c619fb\""}
        ]}}'
      fi
    elif [[ "$url" == *'%3Akube-system%3AStaticPods%3Akube-controller-manager'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/response-mode")" = "invalid" ]; then
        printf '%s\n' '{"data":{"status":"ok"}}'
      elif [ "$(cat "${dir}/response-mode")" = "truncated" ]; then
        jq -cn '{data:{status:"ok",entries:[range(0;1000) | {
          severity:"error",
          message:"E0919 15:54:57.858842       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"flux-system/kustomize-controller-9895f7fb8\\\" failed with read version: 295731570 is not as new as written version: 295731572 for group resource replicasets.apps\" logger=\"UnhandledError\""
        }]}}'
      elif [ "$(cat "${dir}/controller-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"E0919 15:54:02.455011       1 reflector.go:227] \"Failed to watch\" err=\"failed to list *v1.PartialObjectMetadata: the server could not find the requested resource\" logger=\"UnhandledError\" reflector=\"k8s.io/client-go/metadata/metadatainformer/informer.go:146\" type=\"*v1.PartialObjectMetadata\""},
          {"severity":"error","message":"E0919 15:54:57.858842       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"flux-system/kustomize-controller-9895f7fb8\\\" failed with read version: 295731570 is not as new as written version: 295731572 for group resource replicasets.apps\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0919 23:15:11.248729       1 cronjob_controllerv2.go:179] \"Unhandled Error\" err=\"error syncing CronJobController observability/cluster-heartbeat, requeuing: Operation cannot be fulfilled on cronjobs.batch \\\"cluster-heartbeat\\\": the object has been modified; please apply your changes to the latest version and try again\" logger=\"UnhandledError\""}
        ]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"E0919 23:15:11.248729       1 cronjob_controllerv2.go:179] \"Unhandled Error\" err=\"error syncing CronJobController observability/cluster-heartbeat, requeuing: Operation cannot be fulfilled on cronjobs.batch \\\"different-job\\\": the object has been modified; please apply your changes to the latest version and try again\" logger=\"UnhandledError\""}]}}'
      fi
    elif [[ "$url" == *'%3Aflux-system%3ADeployment%3Akustomize-controller'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/kustomize-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"Reconciliation failed after 269.860866ms, next try in 2m0s","attributes":{"Kustomization.name":"ascoachingogvaner","Kustomization.namespace":"ascoachingogvaner","controller":"kustomization","controllerGroup":"kustomize.toolkit.fluxcd.io","controllerKind":"Kustomization","error":"health check failed after 45.576415ms: context canceled","name":"ascoachingogvaner","namespace":"ascoachingogvaner","reconcileID":"a4f572ba-397d-4032-8576-5fda40821c14","revision":"1.13.7@sha256:1016aa926827717db1339dcc7abbe7b9e8fce1ccfe548f0396422b613c453501","service.name":"/k8s/flux-system/kustomize-controller"}},
          {"severity":"error","message":"Reconciler error","attributes":{"Kustomization.name":"ascoachingogvaner","Kustomization.namespace":"ascoachingogvaner","controller":"kustomization","controllerGroup":"kustomize.toolkit.fluxcd.io","controllerKind":"Kustomization","error":"context canceled","errorCauses":"[{\"error\":\"context canceled\",\"errorCauses\":[{\"error\":\"context canceled\",\"errorCauses\":[{\"error\":\"context canceled\"},{\"error\":\"context canceled\"}]}]}]","name":"ascoachingogvaner","namespace":"ascoachingogvaner","reconcileID":"a4f572ba-397d-4032-8576-5fda40821c14","service.name":"/k8s/flux-system/kustomize-controller"}}
        ]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"Reconciliation failed after 269.860866ms, next try in 2m0s","attributes":{"Kustomization.name":"ascoachingogvaner","Kustomization.namespace":"ascoachingogvaner","controller":"kustomization","controllerGroup":"kustomize.toolkit.fluxcd.io","controllerKind":"Kustomization","error":"health check failed after 45.576415ms: context canceled","name":"ascoachingogvaner","namespace":"ascoachingogvaner","reconcileID":"a4f572ba-397d-4032-8576-5fda40821c14","service.name":"/k8s/flux-system/kustomize-controller"}},
          {"severity":"error","message":"Reconciler error","attributes":{"Kustomization.name":"ascoachingogvaner","Kustomization.namespace":"ascoachingogvaner","controller":"kustomization","controllerGroup":"kustomize.toolkit.fluxcd.io","controllerKind":"Kustomization","error":"context canceled","errorCauses":"[{\"error\":\"context canceled\"}]","name":"ascoachingogvaner","namespace":"ascoachingogvaner","reconcileID":"different-reconcile-id","service.name":"/k8s/flux-system/kustomize-controller"}}
        ]}}'
      fi
    elif [[ "$url" == *'%3Akube-system%3ADeployment%3Acluster-autoscaler-hetzner-cluster-autoscaler'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/autoscaler-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"E0920 07:58:01.416402       1 controller.go:300] \"Unhandled Error\" err=\"capacity buffer controller error: Operation cannot be fulfilled on capacitybuffers.autoscaling.x-k8s.io \\\"overprovisioning\\\": the object has been modified; please apply your changes to the latest version and try again\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0920 07:58:01.416472       1 controller.go:250] \"Unhandled Error\" err=\"error syncing namespace \\\"overprovisioning\\\"\" logger=\"UnhandledError\""}
        ]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"E0920 07:58:01.416402       1 controller.go:300] \"Unhandled Error\" err=\"capacity buffer controller error: Operation cannot be fulfilled on capacitybuffers.autoscaling.x-k8s.io \\\"overprovisioning\\\": the object has been modified; please apply your changes to the latest version and try again\" logger=\"UnhandledError\""},
          {"severity":"error","message":"E0920 07:58:02.416472       1 controller.go:250] \"Unhandled Error\" err=\"error syncing namespace \\\"overprovisioning\\\"\" logger=\"UnhandledError\""}
        ]}}'
      fi
    elif [[ "$url" == *'%3Avertical-pod-autoscaler%3ADeployment%3Avertical-pod-autoscaler-vpa-updater'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"E0919 17:00:42.892044       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"events \\\"pod.123\\\" is forbidden: User \\\"system:serviceaccount:vertical-pod-autoscaler:vertical-pod-autoscaler-vpa-updater\\\" cannot patch resource \\\"events\\\" in API group \\\"\\\" in the namespace \\\"kubescape\\\"\" event=\"&Event{Reason:InPlaceResizedByVPA,Message:Pod was resized in place by VPA Updater.}\""}]}}'
      fi
    elif [[ "$url" == *'%3Avelero%3ADeployment%3Avelero'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=\"2026-09-20T01:59:13Z\" level=error msg=\"error encountered while scanning stdout\" backup-storage-location=velero/default cmd=/plugins/velero-plugin-for-aws controller=backup-storage-location error=\"read |0: file already closed\" logSource=\"pkg/plugin/clientmgmt/process/logrus_adapter.go:90\""}]}}'
      fi
    elif [[ "$url" == *'%3Acnpg-system%3ADeployment%3Acloudnative-pg'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/cnpg-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"Reconciler error","attributes":{"Backup.name":"coroot-db-daily-20260820033000","Backup.namespace":"observability","controller":"backup","controllerGroup":"postgresql.cnpg.io","controllerKind":"Backup","error":"terminal error: Backup.postgresql.cnpg.io \"coroot-db-daily-20260820033000\" not found","name":"coroot-db-daily-20260820033000","namespace":"observability","service.name":"/k8s/cnpg-system/cloudnative-pg"}}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"Reconciler error","attributes":{"Backup.name":"coroot-db-daily-20260820033000","Backup.namespace":"observability","controller":"backup","controllerGroup":"postgresql.cnpg.io","controllerKind":"Backup","error":"terminal error: Backup.postgresql.cnpg.io \"a-different-backup\" not found","name":"coroot-db-daily-20260820033000","namespace":"observability","service.name":"/k8s/cnpg-system/cloudnative-pg"}}]}}'
      fi
    elif [[ "$url" == *'%3Akube-system%3ADeployment%3Ahcloud-csi-controller'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/csi-controller-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=2026-09-20T03:30:15.774Z level=ERROR source=/home/runner/work/csi-driver/csi-driver/internal/app/app.go:321 msg=\"handler failed\" component=grpc-server err=\"rpc error: code = Internal desc = failed to publish volume: Get \\\"https://api.hetzner.cloud/v1/volumes/106045084\\\": context canceled\""}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=2026-09-20T03:30:15.774Z level=ERROR source=/home/runner/work/csi-driver/csi-driver/internal/app/app.go:321 msg=\"handler failed\" component=grpc-server err=\"rpc error: code = Internal desc = failed to publish volume: Get \\\"https://api.hetzner.cloud/v1/volumes/106045084\\\": connection reset by peer\""}]}}'
      fi
    elif [[ "$url" == *'%3Akube-system%3ADaemonSet%3Ahcloud-csi-node'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/csi-node-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=2026-09-20T03:30:18.447Z level=ERROR source=/home/runner/work/csi-driver/csi-driver/internal/app/app.go:321 msg=\"handler failed\" component=grpc-server err=\"rpc error: code = Internal desc = failed to publish volume: device \\\"/dev/disk/by-id/scsi-0HC_Volume_106045084\\\" not ready: no such file or directory\""}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"time=2026-09-20T03:30:18.447Z level=ERROR source=/home/runner/work/csi-driver/csi-driver/internal/app/app.go:321 msg=\"handler failed\" component=grpc-server err=\"rpc error: code = Internal desc = failed to publish volume: device \\\"/dev/disk/by-id/scsi-0HC_Volume_106045084\\\" not ready: permission denied\""}]}}'
      fi
    elif [[ "$url" == *'%3Akubescape%3ADeployment%3Aoperator'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/operator-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[
          {"severity":"error","message":"error in SBOMWatch","attributes":{"error":"missing WLID","service.name":"/k8s/kubescape/operator"}},
          {"severity":"error","message":"failed loading pod spec","attributes":{"error":"pods \"velero-daily-full-20260920021704-454s4\" not found","name":"podvolumebackup-velero-daily-full-20260920021704-454s4-d4083f6d-681a-42f6-9abf-27ee721bcf30-d5af-1a63","namespace":"velero","service.name":"/k8s/kubescape/operator","wlid":"wlid://cluster-prod/namespace-velero/pod-velero-daily-full-20260920021704-454s4"}},
          {"severity":"error","message":"failed loading pod spec","attributes":{"error":"pods \"velero-daily-full-20260920021704-kg7tt\" not found","name":"dataupload-velero-daily-full-20260920021704-kg7tt-7ffba5cb-38cf-404e-aa79-eeb6afbb2ff3-4776-a1e1","namespace":"velero","service.name":"/k8s/kubescape/operator","wlid":"wlid://cluster-prod/namespace-velero/pod-velero-daily-full-20260920021704-kg7tt"}}
        ]}}'
      elif [ "$(cat "${dir}/operator-mode")" = "malformed" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"failed loading pod spec","attributes":{"error":"pods \"velero-daily-full-20260920021704-454s4\" not found","name":null,"namespace":"velero","service.name":"/k8s/kubescape/operator","wlid":"wlid://cluster-prod/namespace-velero/pod-velero-daily-full-20260920021704-454s4"}}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"failed loading pod spec","attributes":{"error":"pods \"application-db-0\" not found","name":"containerprofile-application-db-0","namespace":"production","service.name":"/k8s/kubescape/operator","wlid":"wlid://cluster-prod/namespace-production/pod-application-db-0"}}]}}'
      fi
    elif [[ "$url" == *'%3Akubescape%3ADeployment%3Astorage'* ]]; then
      if [ "${severity}" = "fatal" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
      elif [ "$(cat "${dir}/storage-mode")" = "known" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"load metadata error","attributes":{"error":"failed to read metadata file: open /data/spdx.softwarecomposition.kubescape.io/vulnerabilitymanifests/kubescape/ghcr.io-backstage-backstage-1.52.0-993611.m: no such file or directory","service.name":"/k8s/kubescape/storage"}}]}}'
      elif [ "$(cat "${dir}/storage-mode")" = "malformed" ]; then
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"load metadata error","attributes":{"error":null,"service.name":"/k8s/kubescape/storage"}}]}}'
      else
        printf '%s\n' '{"data":{"status":"ok","entries":[{"severity":"error","message":"load metadata error","attributes":{"error":"failed to read metadata file: open /data/spdx.softwarecomposition.kubescape.io/vulnerabilitymanifests/kubescape/ghcr.io-backstage-backstage-1.52.0-993611.m: permission denied","service.name":"/k8s/kubescape/storage"}}]}}'
      fi
    else
      printf '%s\n' '{"data":{"status":"ok","entries":[]}}'
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
    elif [[ "${url}" == *'%3Akube-system%3AStaticPods%3Akube-controller-manager'* ]] &&
      { [ "$(cat "${dir}/controller-mode")" = "near-miss" ] ||
        [ "$(cat "${dir}/response-mode")" != "complete" ]; }; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":5000}]}}'
    elif [[ "${url}" == *'%3Aflux-system%3ADeployment%3Akustomize-controller'* ]] &&
      [ "$(cat "${dir}/kustomize-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
    elif [[ "${url}" == *'%3Akube-system%3ADeployment%3Acluster-autoscaler-hetzner-cluster-autoscaler'* ]] &&
      [ "$(cat "${dir}/autoscaler-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
    elif [[ "${url}" == *'%3Akubescape%3ADeployment%3Aoperator'* ]] &&
      [ "$(cat "${dir}/operator-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":100}]}}'
    elif [[ "${url}" == *'%3Akubescape%3ADeployment%3Astorage'* ]] &&
      [ "$(cat "${dir}/storage-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
    elif [[ "${url}" == *'%3Acnpg-system%3ADeployment%3Acloudnative-pg'* ]] &&
      [ "$(cat "${dir}/cnpg-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
    elif [[ "${url}" == *'%3Akube-system%3ADeployment%3Ahcloud-csi-controller'* ]] &&
      [ "$(cat "${dir}/csi-controller-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
    elif [[ "${url}" == *'%3Akube-system%3ADaemonSet%3Ahcloud-csi-node'* ]] &&
      [ "$(cat "${dir}/csi-node-mode")" != "known" ]; then
      printf '%s\n' '{"form":{"configs":[{"threshold":0},null,{"threshold":10}]}}'
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

absent_log_stream_dir="$(setup_scenario absent-log-stream known null 'context deadline exceeded' null known complete known known known known known valid 'rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found' valid /talos/init known known absent)"
absent_log_stream_output="$(run_scenario "${absent_log_stream_dir}" 2>&1)"
printf '%s\n' "${absent_log_stream_output}" | jq -s -e '
  length > 0 and
  all(.[]; .level == "info") and
  any(.[]; .msg == "autosuppressor-lifecycle skipped: no log stream for 95rsc5yp:observability:CronJob:coroot-alert-autosuppressor")
' >/dev/null || fail 'an absent Coroot log stream produced a recurring reconciler error'
jq -s -e '
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  all(.[]; .url | contains("%3Aobservability%3ACronJob%3Acoroot-alert-autosuppressor/inspection/LogErrors/config") | not)
' "${absent_log_stream_dir}/posts.ndjson" >/dev/null ||
  fail 'an absent log stream either stopped later policies or mutated a nonexistent policy target'
pass 'an absent Coroot log stream is a clean no-policy target'

known_dir="$(setup_scenario known known null)"
known_output="$(run_scenario "${known_dir}")"
printf '%s\n' "${known_output}" | jq -s -e \
  'length > 0 and all(.[]; .level == "info" and (.msg | type == "string" and length > 0))' \
  >/dev/null || fail 'successful reconciliation must emit structured info JSON'
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Akubelet/inspection/NetworkTCPConnections/config")) and .body.configs[2] == null) and
  any(.[]; (.url | contains("%3Akyverno%3ADeployment%3Akyverno-background-controller/inspection/MemoryLeakPercent/config")) and .body.configs[2].threshold == 35) and
  any(.[]; (.url | contains("%3Akyverno%3ADeployment%3Akyverno-cleanup-controller/inspection/MemoryLeakPercent/config")) and .body.configs[2].threshold == 40) and
  any(.[]; (.url | contains("%3Acrossplane-system%3ADeployment%3Acrossplane/inspection/MemoryLeakPercent/config")) and .body.configs[2].threshold == 35) and
  any(.[]; (.url | contains("%3Akube-system%3ADaemonSet%3Acilium/inspection/DnsNxdomainErrors/config")) and .body.configs[2].threshold == 7500) and
  any(.[]; (.url | contains("%3Aobservability%3ACronJob%3Acoroot-alert-autosuppressor/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2].threshold == 1000) and
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-apiserver/inspection/LogErrors/config")) and .body.configs[2].threshold == 100) and
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-controller-manager/inspection/LogErrors/config")) and .body.configs[2].threshold == 5000) and
  any(.[]; (.url | contains("%3Aflux-system%3ADeployment%3Akustomize-controller/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Akube-system%3ADeployment%3Acluster-autoscaler-hetzner-cluster-autoscaler/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Avertical-pod-autoscaler%3ADeployment%3Avertical-pod-autoscaler-vpa-updater/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Avelero%3ADeployment%3Avelero/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Acnpg-system%3ADeployment%3Acloudnative-pg/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Akube-system%3ADeployment%3Ahcloud-csi-controller/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Akube-system%3ADaemonSet%3Ahcloud-csi-node/inspection/LogErrors/config")) and .body.configs[2].threshold == 10) and
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Aoperator/inspection/LogErrors/config")) and .body.configs[2].threshold == 100) and
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Astorage/inspection/LogErrors/config")) and .body.configs[2].threshold == 10)
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

runtime_sandbox_near_miss_dir="$(setup_scenario runtime-sandbox-near-miss known null 'context deadline exceeded' 1000 known complete known known known known known valid 'rpc error: code = PermissionDenied desc = forbidden')"
run_scenario "${runtime_sandbox_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${runtime_sandbox_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'a PodSandboxStatus failure other than the terminal NotFound race did not remain visible'
pass 'the Talos sandbox-status policy rejects non-NotFound failures'

runtime_sandbox_malformed_dir="$(setup_scenario runtime-sandbox-malformed known null 'context deadline exceeded' 1000 known complete known known known known known valid 'rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found' scalar)"
run_scenario "${runtime_sandbox_malformed_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${runtime_sandbox_malformed_dir}/posts.ndjson" >/dev/null ||
  fail 'malformed PodSandboxStatus attributes left the previous Talos threshold active'
pass 'malformed Talos sandbox-status attributes remove the reviewed threshold'

runtime_sandbox_service_dir="$(setup_scenario runtime-sandbox-service known null 'context deadline exceeded' 1000 known complete known known known known known valid 'rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found' valid /talos/other)"
run_scenario "${runtime_sandbox_service_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${runtime_sandbox_service_dir}/posts.ndjson" >/dev/null ||
  fail 'a PodSandboxStatus NotFound from another service did not remain visible'
pass 'the Talos sandbox-status policy rejects other services'

runtime_malformed_dir="$(setup_scenario runtime-malformed known null 'context deadline exceeded' 1000 known complete known known known known known malformed)"
run_scenario "${runtime_malformed_dir}" >/dev/null 2>&1 || true
jq -s -e '
  any(.[]; (.url | contains("%3A_%3AUnknown%3Ainit/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${runtime_malformed_dir}/posts.ndjson" >/dev/null ||
  fail 'malformed Talos attributes left the previous application threshold active'
pass 'malformed Talos runtime attributes remove the reviewed threshold'

controller_near_miss_dir="$(setup_scenario controller-near-miss known null 'context deadline exceeded' null near-miss)"
run_scenario "${controller_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-controller-manager/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${controller_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'a CronJob conflict whose object name differs from the controller key did not remain visible'
pass 'the controller retry policy rejects same-shaped name mismatches'

kustomize_near_miss_dir="$(setup_scenario kustomize-near-miss known null 'context deadline exceeded' null known complete known known known known known valid 'rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found' valid /talos/init near-miss)"
run_scenario "${kustomize_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Aflux-system%3ADeployment%3Akustomize-controller/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${kustomize_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'an uncorrelated Flux cancellation pair did not remain visible'
pass 'the Flux cancellation policy rejects uncorrelated or malformed entries'

autoscaler_near_miss_dir="$(setup_scenario autoscaler-near-miss known null 'context deadline exceeded' null known complete known known known known known valid 'rpc error: code = NotFound desc = an error occurred when try to find sandbox: not found' valid /talos/init known near-miss)"
run_scenario "${autoscaler_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akube-system%3ADeployment%3Acluster-autoscaler-hetzner-cluster-autoscaler/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${autoscaler_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'an uncorrelated CapacityBuffer retry pair did not remain visible'
pass 'the CapacityBuffer retry policy rejects uncorrelated entries'

operator_near_miss_dir="$(setup_scenario operator-near-miss known null 'context deadline exceeded' null known complete near-miss)"
run_scenario "${operator_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Aoperator/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${operator_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'a missing non-Velero pod did not remain visible'
pass 'the Kubescape lifecycle policy rejects unrelated missing workloads'

storage_near_miss_dir="$(setup_scenario storage-near-miss known null 'context deadline exceeded' null known complete known near-miss)"
run_scenario "${storage_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Astorage/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${storage_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'a metadata read failure other than a repaired missing sidecar did not remain visible'
pass 'the Kubescape storage cleanup policy rejects unrelated metadata failures'

operator_malformed_dir="$(setup_scenario operator-malformed known null 'context deadline exceeded' null known complete malformed)"
run_scenario "${operator_malformed_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Aoperator/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${operator_malformed_dir}/posts.ndjson" >/dev/null ||
  fail 'a Kubescape operator entry with malformed attributes did not fail closed'
pass 'malformed Kubescape operator attributes remove the reviewed threshold'

storage_malformed_dir="$(setup_scenario storage-malformed known null 'context deadline exceeded' null known complete known malformed)"
run_scenario "${storage_malformed_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akubescape%3ADeployment%3Astorage/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${storage_malformed_dir}/posts.ndjson" >/dev/null ||
  fail 'a Kubescape storage entry with malformed attributes did not fail closed'
pass 'malformed Kubescape storage attributes remove the reviewed threshold'

lifecycle_near_miss_dir="$(setup_scenario lifecycle-near-miss known null 'context deadline exceeded' null known complete known known near-miss near-miss near-miss)"
run_scenario "${lifecycle_near_miss_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Acnpg-system%3ADeployment%3Acloudnative-pg/inspection/LogErrors/config")) and .body.configs[2] == null) and
  any(.[]; (.url | contains("%3Akube-system%3ADeployment%3Ahcloud-csi-controller/inspection/LogErrors/config")) and .body.configs[2] == null) and
  any(.[]; (.url | contains("%3Akube-system%3ADaemonSet%3Ahcloud-csi-node/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${lifecycle_near_miss_dir}/posts.ndjson" >/dev/null ||
  fail 'a near-miss backup deletion or volume-publish error did not remain visible'
pass 'backup deletion and volume-publish policies reject unrelated failures'

truncated_dir="$(setup_scenario truncated known null 'context deadline exceeded' null known truncated)"
run_scenario "${truncated_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-controller-manager/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${truncated_dir}/posts.ndjson" >/dev/null ||
  fail 'a log response at the raw-message limit did not remain visible'
pass 'a potentially truncated raw-message response fails closed'

invalid_dir="$(setup_scenario invalid known null 'context deadline exceeded' null known invalid)"
invalid_output="$(run_scenario "${invalid_dir}" 2>&1)"
printf '%s\n' "${invalid_output}" | jq -s -e '
  any(.[]; .level == "error" and (.msg | contains("invalid raw-message response")))
' >/dev/null ||
  fail 'a response missing the entries field was accepted as an empty result'
jq -s -e '
  any(.[]; (.url | contains("%3Akube-system%3AStaticPods%3Akube-controller-manager/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${invalid_dir}/posts.ndjson" >/dev/null ||
  fail 'an invalid raw-message response did not remain visible'
pass 'an invalid raw-message response fails closed'

aged_dir="$(setup_scenario aged none 10)"
run_scenario "${aged_dir}" >/dev/null
jq -s -e '
  any(.[]; (.url | contains("%3Adex%3ADeployment%3Adex/inspection/LogErrors/config")) and .body.configs[2] == null)
' "${aged_dir}/posts.ndjson" >/dev/null ||
  fail 'an aged-out Dex pattern did not remove its app-level threshold'
pass 'an aged-out pattern returns to the global zero threshold'
