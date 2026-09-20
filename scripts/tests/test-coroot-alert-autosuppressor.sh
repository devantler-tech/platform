#!/usr/bin/env bash

# Behaviour contract for Coroot's declarative alert auto-suppressor.
#
# Coroot attributes the kubelet's localhost health probes for the three static
# kube-schedulers and controller managers as failed outbound TCP connections.
# The live signal is deterministic: scheduler liveness + readiness yields
# 0.6/s, while controller-manager liveness yields 0.3/s. Suppression is safe
# only while those are the complete set of active upstream series. A third
# active upstream must keep the alert visible. Log-pattern exemptions bind to a
# stable Coroot fingerprint plus an exact current representative. IDs rotate
# after resolution; a changed weakly-equal representative must be reopened.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-alert-autosuppressor.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'ok — %s\n' "$1"
}

for tool in yq jq; do
  command -v "${tool}" >/dev/null 2>&1 || {
    printf '::error::%s is required to run this contract test.\n' "${tool}" >&2
    exit 64
  }
done

[ -f "${manifest}" ] || fail "manifest not found: ${manifest}"
script_body="$(yq eval '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' "${manifest}")"
if [ -z "${script_body}" ] || [ "${script_body}" = "null" ]; then
  fail "could not extract the autosuppressor script"
fi

# Flux post-build substitution consumes unescaped ${...} expressions in
# rendered resources. Shell parameter slicing therefore arrives in the live
# CronJob as an empty assignment even though executing this source manifest
# directly works. Keep the embedded script free of brace-form expansions.
# shellcheck disable=SC2016 # The single-quoted sequence is the literal hazard.
[[ "${script_body}" != *'${'* ]] ||
  fail 'the autosuppressor script contains a Flux-consumable shell expansion'

work_root="$(mktemp -d /tmp/tmp.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT

setup_scenario() {
  local name="$1" extra_upstream="$2"
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin"

  cat >"${dir}/user.json" <<'JSON'
{"data":{"projects":[{"id":"95rsc5yp","name":"platform"}]}}
JSON
  cat >"${dir}/alerts.json" <<'JSON'
{"data":{"alerts":[{"id":"kubelet-probe-noise","suppressed":false,"resolved_at":null,"rule_id":"network-tcp-connections","application_id":"95rsc5yp:_:Unknown:kubelet"}]}}
JSON
  cat >"${dir}/history.json" <<'JSON'
{"data":{"alerts":[]}}
JSON
  cat >"${dir}/applications.json" <<'JSON'
{"context":{"alerts":{"critical":2,"warning":3}},"data":{"applications":[
  {"status":"critical"},
  {"status":"critical"},
  {"status":"warning"},
  {"status":"info"},
  {"status":"info"},
  {"status":"info"},
  {"status":"info"},
  {"status":"unknown"},
  {"status":"ok"},
  {"status":"ok"}
]}}
JSON

  jq -cn --argjson extra "${extra_upstream}" '
    {data:{widgets:[{chart:{title:"Failed TCP connections, per second",series:(
      [
        {name:"→kube-scheduler",data:[0.6,0.6]},
        {name:"→kube-controller-manager",data:[0.3,0.3]},
        {name:"→coredns",data:[0,0]}
      ] + (if $extra then [{name:"→external:443",data:[0,0.1]}] else [] end)
    )}}]}}' >"${dir}/detail-kubelet-probe-noise.json"

  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${SCENARIO_DIR}"
url=""
payload=""
prev=""
for arg in "$@"; do
  [ "${prev}" = "-d" ] && payload="${arg}"
  case "${arg}" in http://*) url="${arg}" ;; esac
  prev="${arg}"
done
case "${url}" in
  */api/user) cat "${dir}/user.json" ;;
  *'/overview/applications') cat "${dir}/applications.json" ;;
  *'/alerts?include_resolved=true&limit=500') cat "${dir}/history.json" ;;
  *'/alerts?limit=500') cat "${dir}/alerts.json" ;;
  */alerts/suppress)
    printf '%s' "${payload}" >"${dir}/suppressed.json"
    ;;
  */alerts/reopen)
    printf '%s' "${payload}" >"${dir}/reopened.json"
    ;;
  */alerts/*)
    id="${url##*/}"
    if [ -f "${dir}/detail-${id}.json" ]; then
      cat "${dir}/detail-${id}.json"
    else
      jq -c --arg id "${id}" '{data: ([.data.alerts[] | select(.id == $id)] | first)}' \
        "${dir}/alerts.json"
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

exact_dir="$(setup_scenario exact false)"
exact_output="$(run_scenario "${exact_dir}")"
printf '%s\n' "${exact_output}" | jq -s -e '
  length > 0 and all(.[]; .level == "info" and (.msg | type == "string" and length > 0))
' >/dev/null ||
  fail "normal autosuppressor lifecycle output must be structured info JSON"
printf '%s\n' "${exact_output}" | jq -s -e '
  any(.[];
    .msg == "coroot clean-state counters"
    and .coroot_clean_state == {
      alerts: {critical: 2, warning: 3},
      applications: {
        slo_violations: 2,
        warnings: 1,
        errors_in_logs: 4,
        integration_required: 1,
        ok: 2
      }
    }
  )
' >/dev/null ||
  fail "the autosuppressor did not expose exact Coroot UI counters"
[ -f "${exact_dir}/suppressed.json" ] ||
  fail "the exact kubelet health-probe series were not suppressed"
jq -e '.ids == ["kubelet-probe-noise"]' "${exact_dir}/suppressed.json" >/dev/null ||
  fail "the exact scenario suppressed the wrong alert set"
pass "exact kubelet scheduler/controller-manager probe noise is suppressed"
pass "normal autosuppressor lifecycle output is structured as info"
pass "Coroot alert and application UI counters are emitted as structured evidence"

invalid_state_dir="$(setup_scenario invalid-state false)"
cat >"${invalid_state_dir}/applications.json" <<'JSON'
{"context":{"alerts":{"critical":"0","warning":0}},"data":{"applications":[]}}
JSON
if run_scenario "${invalid_state_dir}" >/dev/null 2>&1; then
  fail "an invalid Coroot counter response reported a false clean state"
fi

false_critical_dir="$(setup_scenario false-critical false)"
cat >"${false_critical_dir}/applications.json" <<'JSON'
{"context":{"alerts":{"critical":false,"warning":0}},"data":{"applications":[]}}
JSON
if run_scenario "${false_critical_dir}" >/dev/null 2>&1; then
  fail "a boolean critical counter reported a false clean state"
fi

false_warning_dir="$(setup_scenario false-warning false)"
cat >"${false_warning_dir}/applications.json" <<'JSON'
{"context":{"alerts":{"critical":0,"warning":false}},"data":{"applications":[]}}
JSON
if run_scenario "${false_warning_dir}" >/dev/null 2>&1; then
  fail "a boolean warning counter reported a false clean state"
fi

missing_counters_dir="$(setup_scenario missing-counters false)"
cat >"${missing_counters_dir}/applications.json" <<'JSON'
{"context":{"alerts":{}},"data":{"applications":[]}}
JSON
missing_counters_output="$(run_scenario "${missing_counters_dir}")"
printf '%s\n' "${missing_counters_output}" | jq -s -e '
  any(.[];
    .msg == "coroot clean-state counters"
    and .coroot_clean_state.alerts == {critical: 0, warning: 0}
  )
' >/dev/null ||
  fail "missing zero-valued alert counters did not match Coroot UI semantics"
pass "missing counters default to zero while malformed counters fail closed"

extra_dir="$(setup_scenario extra true)"
run_scenario "${extra_dir}" >/dev/null
[ ! -e "${extra_dir}/suppressed.json" ] ||
  fail "a kubelet alert with an additional active upstream was suppressed"
pass "an additional active upstream keeps the kubelet alert visible"

log_pattern_dir="$(setup_scenario log-patterns false)"
cat >"${log_pattern_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {"id":"mixed-controller-patterns","suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager"},
  {"id":"mixed-reflector-patterns","suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager"}
]}}
JSON
cat >"${log_pattern_dir}/detail-mixed-controller-patterns.json" <<'JSON'
{"data":{"details":[{"name":"Sample","value":"cronjob_controllerv2.go:179 Unhandled Error: error syncing CronJobController observability/crossplane-sync-alerter, requeuing: Operation cannot be fulfilled on cronjobs.batch \"crossplane-sync-alerter\": the object has been modified; please apply your changes to the latest version and try again"}]}}
JSON
cat >"${log_pattern_dir}/detail-mixed-reflector-patterns.json" <<'JSON'
{"data":{"details":[{"name":"Sample","value":"reflector.go:229 Failed to watch *v1.PartialObjectMetadata: could not find the requested resource"}]}}
JSON
run_scenario "${log_pattern_dir}" >/dev/null
[ ! -e "${log_pattern_dir}/suppressed.json" ] ||
  fail "a representative sample suppressed a mixed Coroot log-pattern fingerprint"
pass "mixed kube-controller-manager log-pattern fingerprints remain visible"

event_dir="$(setup_scenario exact-events false)"
cat >"${event_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"auto-vpa-write-conflict",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:_:Unknown:auto-vpa",
    "details":[
      {"name":"Event message","value":"policy auto-vpa/generate-vpa-for-deployment fail: Operation cannot be fulfilled on verticalpodautoscalers.autoscaling.k8s.io \"example\": the object has been modified; please apply your changes to the latest version and try again"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"ClusterPolicy\"\nname=\"auto-vpa\"\nreason=\"PolicyError\""}
    ]
  },
  {
    "id":"flux-status-canceled",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"failed to update status: context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"apps\"\nnamespace=\"flux-system\"\nreason=\"Progressing\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"flux-health-canceled",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"health check failed after 9.155610095s: context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"infrastructure\"\nnamespace=\"flux-system\"\nreason=\"HealthCheckFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"flux-health-canceled-milliseconds",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"health check failed after 45.576415ms: context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"ascoachingogvaner\"\nnamespace=\"ascoachingogvaner\"\nreason=\"HealthCheckFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"flux-dryrun-canceled",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"TeamRepository/github-config/maintainers-platform-template dry-run failed: Patch \"https://10.96.0.1:443/apis/team.github.m.upbound.io/v1alpha1/namespaces/github-config/teamrepositories/maintainers-platform-template?dryRun=All&fieldManager=kustomize-controller&force=true\": context canceled\n"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"github-config\"\nnamespace=\"github-config\"\nreason=\"ReconciliationFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"monitor-secret-race",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:crossview:Deployment:external-secrets",
    "details":[
      {"name":"Event message","value":"secrets \"crossview-postgres-coroot-monitor\" already exists"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"ExternalSecret\"\nname=\"crossview-postgres-coroot-monitor\"\nnamespace=\"crossview\"\nreason=\"UpdateFailed\"\nsource=\"external-secrets\""}
    ]
  },
  {
    "id":"openbao-snapshot-device-race",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"MountVolume.SetUp failed for volume \"pvc-4a2f858e-9be5-462c-bc1f-d65e9327ba09\" : rpc error: code = Internal desc = failed to publish volume: device \"/dev/disk/by-id/scsi-0HC_Volume_106045084\" not ready: no such file or directory"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"vault-snapshot-29831250-fq7w6\"\nnamespace=\"openbao\"\nreason=\"FailedMount\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"longhorn-volume-failed-delete",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"persistentvolume pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc is still attached to node autoscale-cx43-59ee3c84869749a0"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"PersistentVolume\"\nname=\"pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc\"\nreason=\"VolumeFailedDelete\"\nsource=\"driver.longhorn.io_csi-provisioner-6c4f4d4c6b-626lh_1c664a9d-a3d9-4b66-af8a-74e3eabeb787\""}
    ]
  },
  {
    "id":"longhorn-clone-awaiting-healthy",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"copied the data from snapshot snapshot-31ab68ae-1c00-4608-a64e-87f35094845e of the source volume pvc-86581c93-a68f-4880-819f-1b3a037157ab. Waiting for volume to be fully HA before marking the clone as completed"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Volume\"\nname=\"pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc\"\nnamespace=\"longhorn-system\"\nreason=\"VolumeCloneCopyCompleteAwaitingHealthy\"\nsource=\"longhorn-volume-controller\""}
    ]
  },
  {
    "id":"snapshot-content-cleanup",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"VolumeSnapshotContent is missing"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"VolumeSnapshot\"\nname=\"\"\nreason=\"SnapshotContentMissing\"\nsource=\"snapshot-controller\""}
    ]
  },
  {
    "id":"flux-build-canceled",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"post build failed for 'ClusterRole.v1.rbac.authorization.k8s.io/kyverno:reports-controller:read-network-routes': substitute from 'ConfigMap/variables-cluster' error: Get \"https://10.96.0.1:443/api/v1/namespaces/flux-system/configmaps/variables-cluster\": context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"infrastructure-controllers\"\nnamespace=\"flux-system\"\nreason=\"BuildFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-vpa",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:_:Unknown:auto-vpa",
    "details":[
      {"name":"Event message","value":"policy auto-vpa/generate-vpa-for-deployment fail: forbidden"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"ClusterPolicy\"\nname=\"auto-vpa\"\nreason=\"PolicyError\""}
    ]
  },
  {
    "id":"near-match-flux",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:flux-system:Deployment:kustomize-controller",
    "details":[
      {"name":"Event message","value":"health check failed after timeout"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"apps\"\nnamespace=\"flux-system\"\nreason=\"HealthCheckFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-flux-dryrun",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"TeamRepository/github-config/maintainers-platform-template dry-run failed: forbidden"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"github-config\"\nnamespace=\"github-config\"\nreason=\"ReconciliationFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-flux-build",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"post build failed for 'Deployment.apps/observability/coroot': validation failed"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"infrastructure-controllers\"\nnamespace=\"flux-system\"\nreason=\"BuildFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-flux-build-reason",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"post build failed for 'ClusterRole.v1.rbac.authorization.k8s.io/kyverno:reports-controller:read-network-routes': substitute from 'ConfigMap/variables-cluster' error: Get \"https://10.96.0.1:443/api/v1/namespaces/flux-system/configmaps/variables-cluster\": context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"infrastructure-controllers\"\nnamespace=\"flux-system\"\nreason=\"ReconciliationFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-flux-application",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:flux-system:Deployment:kustomize-controller",
    "details":[
      {"name":"Event message","value":"TeamRepository/github-config/maintainers-platform-template dry-run failed: Patch \"https://10.96.0.1:443/apis/team.github.m.upbound.io/v1alpha1/namespaces/github-config/teamrepositories/maintainers-platform-template?dryRun=All&fieldManager=kustomize-controller&force=true\": context canceled\n"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Kustomization\"\nname=\"github-config\"\nnamespace=\"github-config\"\nreason=\"ReconciliationFailed\"\nsource=\"kustomize-controller\""}
    ]
  },
  {
    "id":"near-match-secret",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":"95rsc5yp:crossview:Deployment:external-secrets",
    "details":[
      {"name":"Event message","value":"secrets \"crossview-postgres-coroot-monitor\" is forbidden"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"ExternalSecret\"\nname=\"crossview-postgres-coroot-monitor\"\nnamespace=\"crossview\"\nreason=\"UpdateFailed\"\nsource=\"external-secrets\""}
    ]
  },
  {
    "id":"near-match-snapshot-device",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"MountVolume.SetUp failed for volume \"pvc-4a2f858e-9be5-462c-bc1f-d65e9327ba09\" : rpc error: code = Internal desc = failed to publish volume: device \"/dev/disk/by-id/scsi-0HC_Volume_106045084\" not ready: permission denied"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"vault-snapshot-29831250-fq7w6\"\nnamespace=\"openbao\"\nreason=\"FailedMount\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"near-match-volume-delete",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"persistentvolume pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc deletion failed: permission denied"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"PersistentVolume\"\nname=\"pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc\"\nreason=\"VolumeFailedDelete\"\nsource=\"driver.longhorn.io_csi-provisioner-6c4f4d4c6b-626lh_1c664a9d-a3d9-4b66-af8a-74e3eabeb787\""}
    ]
  },
  {
    "id":"near-match-clone-awaiting",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"failed to copy data from snapshot: checksum mismatch"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Volume\"\nname=\"pvc-6c4f1be6-7dd2-44dd-bd1b-f53e2d5204cc\"\nnamespace=\"longhorn-system\"\nreason=\"VolumeCloneCopyCompleteAwaitingHealthy\"\nsource=\"longhorn-volume-controller\""}
    ]
  },
  {
    "id":"near-match-snapshot-content",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"kubernetes-events",
    "application_id":":::",
    "details":[
      {"name":"Event message","value":"VolumeSnapshotContent is missing"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"VolumeSnapshot\"\nname=\"live-snapshot\"\nreason=\"SnapshotContentMissing\"\nsource=\"snapshot-controller\""}
    ]
  }
]}}
JSON
run_scenario "${event_dir}" >/dev/null
[ -f "${event_dir}/suppressed.json" ] ||
  fail "the exact by-design Kubernetes lifecycle events were not suppressed"
jq -e '.ids | sort == ["auto-vpa-write-conflict", "flux-build-canceled", "flux-dryrun-canceled", "flux-health-canceled", "flux-health-canceled-milliseconds", "flux-status-canceled", "longhorn-clone-awaiting-healthy", "longhorn-volume-failed-delete", "monitor-secret-race", "openbao-snapshot-device-race", "snapshot-content-cleanup"]' \
  "${event_dir}/suppressed.json" >/dev/null || {
  jq -c '.ids | sort' "${event_dir}/suppressed.json" >&2
  fail "the event exemptions were broader than their exact label and message contracts"
}
pass "exact self-healing lifecycle events are suppressed while near matches stay visible"

controlled_dir="$(setup_scenario controlled-platform-lifecycle false)"
cat >"${controlled_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"autoscale-delete-anchor","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1000000,"updated_at":1000000,
    "details":[
      {"name":"Event message","value":"Deleting node autoscale-cx43-deadbeef"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-deadbeef\"\nnamespace=\"longhorn-system\"\nreason=\"Delete\"\nsource=\"longhorn-node-controller\""}
    ]
  },
  {
    "id":"autoscale-schedulable","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1000000,"updated_at":1000000,
    "details":[
      {"name":"Event message","value":"Waiting for disk default-disk-fa0100000000 (/var/lib/longhorn) on node autoscale-cx43-deadbeef to be schedulable"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-deadbeef\"\nnamespace=\"longhorn-system\"\nreason=\"Schedulable\"\nsource=\"longhorn-node-controller\""}
    ]
  },
  {
    "id":"autoscale-ready","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1000000,"updated_at":1000000,
    "details":[
      {"name":"Event message","value":"Kubernetes node autoscale-cx43-deadbeef not ready: NodeStatusUnknown"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-deadbeef\"\nnamespace=\"longhorn-system\"\nreason=\"Ready\"\nsource=\"longhorn-node-controller\""}
    ]
  },
  {
    "id":"autoscale-provider-anchor","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2000000,"updated_at":2000000,
    "details":[
      {"name":"Event message","value":"Node could not be added to Load Balancer for service cilium-gateway-platform because the provider ID does not match any known format"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-cafebabe\"\nreason=\"UnknownProviderIDPrefix\"\nsource=\"hcloud-cloud-controller-manager\""}
    ]
  },
  {
    "id":"autoscale-invalid-disk","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2000000,"updated_at":2000000,
    "details":[
      {"name":"Event message","value":"invalid capacity 0 on image filesystem"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-cafebabe\"\nreason=\"InvalidDiskCapacity\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"autoscale-reboot-anchor","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":3000000,"updated_at":3000000,
    "details":[
      {"name":"Event message","value":"Node autoscale-cx43-feedface has been rebooted, boot id: 216ffb86-e3bf-4975-8cb1-5c4f51bc5c22"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-feedface\"\nreason=\"Rebooted\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"autoscale-failed-daemon","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":3000000,"updated_at":3000000,
    "details":[
      {"name":"Event message","value":"Found failed daemon pod observability/coroot-node-agent-abcde on node autoscale-cx43-feedface, will try to kill it"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"FailedDaemonPod\"\nsource=\"daemonset-controller\""}
    ]
  },
  {
    "id":"autoscale-endpoint-slice","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1000000,"updated_at":1000000,
    "details":[
      {"name":"Event message","value":"Error updating Endpoint Slices for Service kube-system/tetragon: skipping Pod tetragon-abcde for Service kube-system/tetragon: Node autoscale-cx43-deadbeef Not Found"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Service\"\nname=\"tetragon\"\nnamespace=\"kube-system\"\nreason=\"FailedToUpdateEndpointSlices\"\nsource=\"endpoint-slice-controller\""}
    ]
  },
  {
    "id":"correlated-network-not-ready","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2005000,"updated_at":2005000,
    "details":[
      {"name":"Event message","value":"network is not ready: container runtime network not ready: NetworkReady=false reason:NetworkPluginNotReady message:Network plugin returns error: cni plugin not initialized"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"coroot-node-agent-abcde\"\nnamespace=\"observability\"\nreason=\"NetworkNotReady\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-failed-mount","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2005000,"updated_at":2005000,
    "details":[
      {"name":"Event message","value":"MountVolume.SetUp failed for volume \"kube-api-access-zln7b\" : object \"observability\"/\"kube-root-ca.crt\" not registered"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"coroot-node-agent-abcde\"\nnamespace=\"observability\"\nreason=\"FailedMount\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-failed-scheduling","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":3005000,"updated_at":3005000,
    "details":[
      {"name":"Event message","value":"0/8 nodes are available: 1 node(s) didn't match pod affinity rules, 7 node(s) didn't satisfy plugin(s) [NodeAffinity]. no new claims to deallocate, preemption: 0/8 nodes are available: 8 Preemption is not helpful for scheduling."},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"cilium-envoy-abcde\"\nnamespace=\"kube-system\"\nreason=\"FailedScheduling\""}
    ]
  },
  {
    "id":"correlated-node-shutdown","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":3005000,"updated_at":3005000,
    "details":[
      {"name":"Event message","value":"Pod was rejected as the node is shutting down."},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"coroot-node-agent-abcde\"\nnamespace=\"observability\"\nreason=\"NodeShutdown\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-grace-period","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":3005000,"updated_at":3005000,
    "details":[
      {"name":"Event message","value":"Container runtime did not kill the pod within specified grace period."},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"ExceededGracePeriod\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-node-not-ready","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Node is not ready"},
      {"name":"Labels","value":"affected=\"cilium-envoy-abcde, cilium-abcde, engine-image-ei-a4d05f02-abcde, longhorn-manager-abcde, tetragon-abcde\"\ncluster=\"platform\"\nreason=\"NodeNotReady\"\nsource=\"node-controller\""}
    ]
  },
  {
    "id":"correlated-cilium-startup","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2005000,"updated_at":2005000,
    "details":[
      {"name":"Event message","value":"Startup probe failed: Get \"http://127.0.0.1:9879/healthz\": dial tcp 127.0.0.1:9879: connect: connection refused"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"cilium-abcde\"\nnamespace=\"kube-system\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-node-agent-reset","fingerprint":"40e9576c5418c1af","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:kubescape:DaemonSet:node-agent","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"http://10.244.12.14:7888/readyz\": read tcp 10.244.12.1:41234->10.244.12.14:7888: read: connection reset by peer"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-engine-rpc-canceled","fingerprint":"4674ee962cf0d212","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:longhorn-system:DaemonSet:engine-image-ei-a4d05f02","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Readiness probe errored and resulted in unknown state: rpc error: code = Canceled desc = context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"correlated-engine-health-refused","fingerprint":"4beaceba9155d4c3","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:longhorn-system:DaemonSet:engine-image-ei-493e04e7","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"https://10.244.12.15:9502/v1/healthz\": dial tcp 10.244.12.15:9502: connect: connection refused"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"quiet-ksail-rollout","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:ksail-operator:Deployment:ksail-operator","opened_at":4000000,"updated_at":4000000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"http://10.244.23.12:8081/readyz\": dial tcp 10.244.23.12:8081: connect: connection refused"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"active-ksail-rollout","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:ksail-operator:Deployment:ksail-operator","opened_at":4102444800000,"updated_at":4102444800000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"http://10.244.23.99:8081/readyz\": dial tcp 10.244.23.99:8081: connect: connection refused"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"fixed-node-delete","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":1000000,"updated_at":1000000,
    "details":[
      {"name":"Event message","value":"Deleting node prod-worker-1"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"prod-worker-1\"\nnamespace=\"longhorn-system\"\nreason=\"Delete\"\nsource=\"longhorn-node-controller\""}
    ]
  },
  {
    "id":"uncorrelated-node-shutdown","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":9000000,"updated_at":9000000,
    "details":[
      {"name":"Event message","value":"Pod was rejected as the node is shutting down."},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"database-0\"\nnamespace=\"production\"\nreason=\"NodeShutdown\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"near-node-agent-fingerprint","fingerprint":"not-the-reviewed-fingerprint","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:kubescape:DaemonSet:node-agent","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"http://10.244.12.14:7888/readyz\": read tcp 10.244.12.1:41234->10.244.12.14:7888: read: connection reset by peer"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"uncorrelated-engine-probe","fingerprint":"4674ee962cf0d212","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:longhorn-system:DaemonSet:engine-image-ei-a4d05f02","opened_at":9000000,"updated_at":9000000,
    "details":[
      {"name":"Event message","value":"Readiness probe errored and resulted in unknown state: rpc error: code = Canceled desc = context canceled"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"wrong-engine-application","fingerprint":"4beaceba9155d4c3","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":"95rsc5yp:production:StatefulSet:database","opened_at":1005000,"updated_at":1005000,
    "details":[
      {"name":"Event message","value":"Readiness probe failed: Get \"https://10.244.12.15:9502/v1/healthz\": dial tcp 10.244.12.15:9502: connect: connection refused"},
      {"name":"Labels","value":"cluster=\"platform\"\nreason=\"Unhealthy\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"near-failed-mount","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":2005000,"updated_at":2005000,
    "details":[
      {"name":"Event message","value":"MountVolume.SetUp failed for volume \"data\" : permission denied"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"database-0\"\nnamespace=\"production\"\nreason=\"FailedMount\"\nsource=\"kubelet\""}
    ]
  },
  {
    "id":"history-correlated-node-shutdown","suppressed":false,"resolved_at":null,"rule_id":"kubernetes-events","application_id":":::","opened_at":12005000,"updated_at":12005000,
    "details":[
      {"name":"Event message","value":"Pod was rejected as the node is shutting down."},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Pod\"\nname=\"coroot-node-agent-history\"\nnamespace=\"observability\"\nreason=\"NodeShutdown\"\nsource=\"kubelet\""}
    ]
  }
]}}
JSON
cat >"${controlled_dir}/history.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"resolved-autoscale-reboot-anchor","suppressed":false,"resolved_at":12001000,"rule_id":"kubernetes-events","application_id":":::","opened_at":12000000,"updated_at":12000000,
    "details":[
      {"name":"Event message","value":"Node autoscale-cx43-history has been rebooted, boot id: 216ffb86-e3bf-4975-8cb1-5c4f51bc5c22"},
      {"name":"Labels","value":"cluster=\"platform\"\nkind=\"Node\"\nname=\"autoscale-cx43-history\"\nreason=\"Rebooted\"\nsource=\"kubelet\""}
    ]
  }
]}}
JSON
run_scenario "${controlled_dir}" >/dev/null
[ -f "${controlled_dir}/suppressed.json" ] ||
  fail "the exact controlled lifecycle alerts were not suppressed"
jq -e '.ids | sort == ["autoscale-delete-anchor", "autoscale-endpoint-slice", "autoscale-failed-daemon", "autoscale-invalid-disk", "autoscale-provider-anchor", "autoscale-ready", "autoscale-reboot-anchor", "autoscale-schedulable", "correlated-cilium-startup", "correlated-engine-health-refused", "correlated-engine-rpc-canceled", "correlated-failed-mount", "correlated-failed-scheduling", "correlated-grace-period", "correlated-network-not-ready", "correlated-node-agent-reset", "correlated-node-not-ready", "correlated-node-shutdown", "history-correlated-node-shutdown", "quiet-ksail-rollout"]' \
  "${controlled_dir}/suppressed.json" >/dev/null || {
  jq -c '.ids | sort' "${controlled_dir}/suppressed.json" >&2
  fail "controlled lifecycle classification admitted an active or unrelated alert"
}
pass "controlled autoscaler and rollout lifecycle alerts require exact shapes, correlation, and quiescence"

remaining_dir="$(setup_scenario remaining-operational-warnings false)"
cat >"${remaining_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"cilium-search-expansion",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"dns-nxdomain-errors",
    "application_id":"95rsc5yp:kube-system:DaemonSet:cilium",
    "details":[]
  },
  {
    "id":"coredns-real-nxdomain",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"dns-nxdomain-errors",
    "application_id":"95rsc5yp:kube-system:Deployment:coredns",
    "details":[]
  },
  {
    "id":"rotated-controller-alert",
    "fingerprint":"04241ff02d662139",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager",
    "details":[
      {"name":"Sample","value":"E0919 02:51:35.804560       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"flux-system/kustomize-controller-74dff55bb4\\\" failed with read version: 294462871 is not as new as written version: 294462886 for group resource replicasets.apps\" logger=\"UnhandledError\""}
    ]
  },
  {
    "id":"another-controller-log-alert",
    "fingerprint":"not-the-reviewed-fingerprint",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager",
    "details":[
      {"name":"Sample","value":"E0919 02:51:35.804560       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"flux-system/kustomize-controller-74dff55bb4\\\" failed with read version: 294462871 is not as new as written version: 294462886 for group resource replicasets.apps\" logger=\"UnhandledError\""}
    ]
  },
  {
    "id":"wge5tiucp7k5",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"dns-server-errors",
    "application_id":"95rsc5yp:kubescape:StatefulSet:alertmanager",
    "opened_at":1787841164000,
    "updated_at":1787846982000,
    "details":[]
  },
  {
    "id":"future-alertmanager-dns-error",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"dns-server-errors",
    "application_id":"95rsc5yp:kubescape:StatefulSet:alertmanager",
    "opened_at":1789810000000,
    "updated_at":1789810000000,
    "details":[]
  },
  {
    "id":"kubescape-memory-growth",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"memory-leak",
    "application_id":"95rsc5yp:kubescape:Deployment:kubescape",
    "details":[]
  }
]}}
JSON
run_scenario "${remaining_dir}" >/dev/null
[ -f "${remaining_dir}/suppressed.json" ] ||
  fail "the exact remaining by-design operational warnings were not suppressed"
jq -e '.ids | sort == ["cilium-search-expansion", "rotated-controller-alert", "wge5tiucp7k5"]' \
  "${remaining_dir}/suppressed.json" >/dev/null ||
  fail "the operational exemptions suppressed a neighboring DNS, log, or memory alert"
pass "remaining exemptions are bound to Cilium, one revalidated controller fingerprint, and one historical record"

pinned_logs_dir="$(setup_scenario pinned-control-plane-and-runtime-logs false)"
cat >"${pinned_logs_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"rotated-runtime-alert",
    "fingerprint":"3eade000863df726",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:_:Unknown:init",
    "details":[{"name":"Sample","value":"ContainerStatus from runtime service failed"}]
  },
  {
    "id":"sandbox-resize-alert",
    "fingerprint":"3eade000863df726",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:_:Unknown:init",
    "details":[{"name":"Sample","value":"UpdatePodSandboxResources from runtime service failed"}]
  },
  {
    "id":"9qrrrlq3eooh",
    "fingerprint":"c2ca6857aa3bdbbe",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:_:Unknown:init",
    "details":[{"name":"Sample","value":"ContainerStatus for \"fbff0a8e783183c662e92422987872377643c5e40704f3b8dfd7b0152502d05e\" failed"}]
  },
  {
    "id":"7f3auk0cezgo",
    "fingerprint":"66adf607437525e7",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:_:Unknown:init",
    "details":[{"name":"Sample","value":"DeleteContainer returned error"}]
  },
  {
    "id":"rotated-post-timeout-alert",
    "fingerprint":"1da0a938819a7336",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-apiserver",
    "details":[{"name":"Sample","value":"E0919 11:20:10.747072       1 timeout.go:140] \"Post-timeout activity\" logger=\"UnhandledError\" timeElapsed=\"3.449701ms\" method=\"GET\" path=\"/apis/apps/v1/namespaces/kubescape/deployments/kubevuln\" result=null"}]
  },
  {
    "id":"h9q7onv4l20i",
    "fingerprint":"3222402a73798da3",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-apiserver",
    "details":[{"name":"Sample","value":"E0919 11:20:10.744070       1 status.go:71] \"Unhandled Error\" err=\"apiserver received an error that is not an metav1.Status: &errors.errorString{s:\\\"context canceled\\\"}: context canceled\" logger=\"UnhandledError\""}]
  },
  {
    "id":"rotated-vpa-event-alert",
    "fingerprint":"4caedf68675462a5",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:vertical-pod-autoscaler:Deployment:vertical-pod-autoscaler-vpa-updater",
    "details":[{"name":"Sample","value":"E0915 11:38:42.856589       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"events \\\"kyverno-reports-controller.example\\\" is forbidden: User \\\"system:serviceaccount:vertical-pod-autoscaler:vertical-pod-autoscaler-vpa-updater\\\" cannot patch resource \\\"events\\\" in API group \\\"\\\" in the namespace \\\"kyverno\\\"\" event=\"&Event{ObjectMeta:{...},Reason:InPlaceResizedByVPA,Message:Pod was resized in place by VPA Updater.,Source:EventSource{Component:vpa-updater}}\""}]
  },
  {
    "id":"provisioner-delete-retry-alert",
    "fingerprint":"023675248cbc16ad",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:longhorn-system:Deployment:csi-provisioner",
    "details":[{"name":"Sample","value":"E0920 02:21:50.040962       1 controller.go:1569] \"Volume deletion failed\" err=\"persistentvolume pvc-092a5e54-701b-47b7-92a1-047ab444b15f is still attached to node autoscale-cx43-55e35623532a65dc\" PV=\"pvc-092a5e54-701b-47b7-92a1-047ab444b15f\""}]
  },
  {
    "id":"longhorn-clone-log-alert",
    "fingerprint":"7a7736f01bfa9b65",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:longhorn-system:DaemonSet:longhorn-csi-plugin",
    "details":[{"name":"Sample","value":"time=\"2026-09-20T02:19:20.538217933Z\" level=error msg=\"ControllerPublishVolume: err: rpc error: code = Aborted desc = volume pvc-092a5e54-701b-47b7-92a1-047ab444b15f is not ready for workloads: volume request cloning data but has not finished copying data\" func=csi.logGRPC file=\"server.go:136\""}]
  },
  {
    "id":"nested-clone-retry-alert",
    "fingerprint":"0e29ac857c924dfa",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager",
    "details":[{"name":"Sample","value":"E0920 02:19:21.019896       1 nestedpendingoperations.go:348] Operation for \"{volumeName:kubernetes.io/csi/driver.longhorn.io^pvc-092a5e54-701b-47b7-92a1-047ab444b15f podName: nodeName:}\" failed. No retries permitted until 2026-09-20 02:19:21.519875328 +0000 UTC m=+12182.116106049 (durationBeforeRetry 500ms). Error: AttachVolume.Attach failed for volume \"pvc-092a5e54-701b-47b7-92a1-047ab444b15f\" (UniqueName: \"kubernetes.io/csi/driver.longhorn.io^pvc-092a5e54-701b-47b7-92a1-047ab444b15f\") from node \"autoscale-cx43-55e35623532a65dc\" : rpc error: code = Aborted desc = volume pvc-092a5e54-701b-47b7-92a1-047ab444b15f is not ready for workloads: volume request cloning data but has not finished copying data"}]
  },
  {
    "id":"metrics-removed-autoscale-node",
    "fingerprint":"67863977ffafb8f1",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:kube-system:Deployment:metrics-server",
    "details":[{"name":"Sample","value":"E0920 08:47:44.544358       1 scraper.go:147] \"Failed to scrape node, timeout to access kubelet\" err=\"Get \\\"https://10.0.1.10:10250/metrics/resource\\\": context deadline exceeded\" node=\"autoscale-cx43-78218cc7ab3a29bc\" timeout=\"10s\""}]
  },
  {
    "id":"longhorn-reboot-reconnect",
    "fingerprint":"46c10dbc7f271afe",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:longhorn-system:DaemonSet:longhorn-manager",
    "details":[{"name":"Sample","value":"E0920 17:49:12.384473       1 instance_manager_controller.go:305] \"Unhandled Error\" err=\"failed to sync instance manager for longhorn-system/instance-manager-c210015368cfb1db9feb55fd74623baa: failed to initialize process manager client for instance-manager-c210015368cfb1db9feb55fd74623baa IP 10.244.18.40: failed to check process manager client connection for instance-manager-c210015368cfb1db9feb55fd74623baa IP 10.244.18.40: rpc error: code = Unavailable desc = connection error: desc = \\\"transport: Error while dialing: dial tcp 10.244.18.40:8500: connect: connection refused\\\"\" logger=\"UnhandledError\""}]
  },
  {
    "id":"longhorn-deleted-autoscale-node",
    "fingerprint":"2a2851e9f89ad845",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:longhorn-system:DaemonSet:longhorn-manager",
    "details":[{"name":"Sample","value":"time=\"2026-09-20T08:33:08.234578652Z\" level=error msg=\"Failed to sync Kubernetes node\" func=controller.handleReconcileErrorLogging file=\"utils.go:181\" KubernetesNode=longhorn-system/autoscale-cx43-55e35623532a65dc controller=longhorn-kubernetes-node error=\"failed to sync node longhorn-system/autoscale-cx43-55e35623532a65dc: nodes.longhorn.io \\\"autoscale-cx43-55e35623532a65dc\\\" not found\" node=prod-worker-3"}]
  },
  {
    "id":"kubescape-deleted-runtime-probe",
    "fingerprint":"1d72238cd236f016",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:kubescape:Deployment:operator",
    "details":[{"name":"Sample","value":"failed loading pod spec"}]
  },
  {
    "id":"backstage-controlled-db-failover",
    "fingerprint":"019fd2a540f61820",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:backstage:Deployment:backstage",
    "details":[{"name":"Sample","value":"Connection Error: Connection ended unexpectedly"}]
  },
  {
    "id":"active-metrics-timeout",
    "fingerprint":"67863977ffafb8f1",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":4102444800000,
    "application_id":"95rsc5yp:kube-system:Deployment:metrics-server",
    "details":[{"name":"Sample","value":"E0920 08:47:44.544358       1 scraper.go:147] \"Failed to scrape node, timeout to access kubelet\" err=\"Get \\\"https://10.0.1.10:10250/metrics/resource\\\": context deadline exceeded\" node=\"autoscale-cx43-78218cc7ab3a29bc\" timeout=\"10s\""}]
  },
  {
    "id":"fixed-node-metrics-timeout",
    "fingerprint":"67863977ffafb8f1",
    "suppressed":false,"resolved_at":null,"rule_id":"new-log-patterns","updated_at":1000000,
    "application_id":"95rsc5yp:kube-system:Deployment:metrics-server",
    "details":[{"name":"Sample","value":"E0920 08:47:44.544358       1 scraper.go:147] \"Failed to scrape node, timeout to access kubelet\" err=\"Get \\\"https://10.0.1.4:10250/metrics/resource\\\": context deadline exceeded\" node=\"prod-worker-1\" timeout=\"10s\""}]
  },
  {
    "id":"near-runtime-alert",
    "fingerprint":"not-the-reviewed-fingerprint",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:_:Unknown:init",
    "details":[{"name":"Sample","value":"ContainerStatus from runtime service failed"}]
  },
  {
    "id":"near-apiserver-alert",
    "fingerprint":"1da0a938819a7336",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-apiserver",
    "details":[{"name":"Sample","value":"E0919 11:20:10.747072       1 timeout.go:140] \"Post-timeout activity\" logger=\"UnhandledError\" timeElapsed=\"2.1s\" method=\"POST\" path=\"/api/v1/secrets\" result=null"}]
  },
  {
    "id":"near-vpa-event-alert",
    "fingerprint":"4caedf68675462a5",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:vertical-pod-autoscaler:Deployment:vertical-pod-autoscaler-vpa-updater",
    "details":[{"name":"Sample","value":"E0915 11:38:42.856589       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"forbidden: cannot patch resource \\\"pods\\\"\""}]
  },
  {
    "id":"near-provisioner-delete-alert",
    "fingerprint":"023675248cbc16ad",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:longhorn-system:Deployment:csi-provisioner",
    "details":[{"name":"Sample","value":"E0920 02:21:50.040962       1 controller.go:1569] \"Volume deletion failed\" err=\"persistentvolume deletion forbidden\" PV=\"pvc-092a5e54-701b-47b7-92a1-047ab444b15f\""}]
  },
  {
    "id":"near-longhorn-clone-log",
    "fingerprint":"7a7736f01bfa9b65",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:longhorn-system:DaemonSet:longhorn-csi-plugin",
    "details":[{"name":"Sample","value":"time=\"2026-09-20T02:19:20Z\" level=error msg=\"ControllerPublishVolume: permission denied\" func=csi.logGRPC file=\"server.go:136\""}]
  },
  {
    "id":"near-nested-clone-retry",
    "fingerprint":"0e29ac857c924dfa",
    "suppressed":false,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager",
    "details":[{"name":"Sample","value":"E0920 02:19:21.019896       1 nestedpendingoperations.go:348] Operation failed: permission denied"}]
  }
]}}
JSON
run_scenario "${pinned_logs_dir}" >/dev/null
[ -f "${pinned_logs_dir}/suppressed.json" ] ||
  fail "the exact control-plane and runtime log fingerprints were not suppressed"
jq -e '.ids | sort == ["7f3auk0cezgo", "9qrrrlq3eooh", "backstage-controlled-db-failover", "h9q7onv4l20i", "kubescape-deleted-runtime-probe", "longhorn-clone-log-alert", "longhorn-deleted-autoscale-node", "longhorn-reboot-reconnect", "metrics-removed-autoscale-node", "nested-clone-retry-alert", "provisioner-delete-retry-alert", "rotated-post-timeout-alert", "rotated-runtime-alert", "sandbox-resize-alert"]' \
  "${pinned_logs_dir}/suppressed.json" >/dev/null ||
  fail "the reviewed log exemptions were broader than their exact fingerprints and message shapes"
pass "exact benign control-plane and runtime fingerprints survive ID rotation while near matches stay visible"

changed_controller_dir="$(setup_scenario changed-controller-pattern false)"
cat >"${changed_controller_dir}/alerts.json" <<'JSON'
{"data":{"alerts":[
  {
    "id":"rotated-controller-alert",
    "fingerprint":"04241ff02d662139",
    "suppressed":true,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:kube-system:StaticPods:kube-controller-manager",
    "details":[
      {"name":"Sample","value":"E0919 03:00:00.000000       1 replica_set.go:640] \"Unhandled Error\" err=\"sync \\\"kube-system/example\\\" failed: forbidden\" logger=\"UnhandledError\""}
    ]
  },
  {
    "id":"rotated-vpa-event-alert",
    "fingerprint":"4caedf68675462a5",
    "suppressed":true,
    "resolved_at":null,
    "rule_id":"new-log-patterns",
    "application_id":"95rsc5yp:vertical-pod-autoscaler:Deployment:vertical-pod-autoscaler-vpa-updater",
    "details":[
      {"name":"Sample","value":"E0919 03:00:00.000000       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"events \\\"kyverno-reports-controller.example\\\" is forbidden: User \\\"system:serviceaccount:vertical-pod-autoscaler:vertical-pod-autoscaler-vpa-updater\\\" cannot patch resource \\\"events\\\" in API group \\\"\\\" in the namespace \\\"kyverno\\\"\" event=\"&Event{ObjectMeta:{...},Reason:InPlaceResizedByVPA,Message:Pod was resized in place by VPA Updater.,Source:EventSource{Component:vpa-updater}}\""}
    ]
  }
]}}
JSON
run_scenario "${changed_controller_dir}" >/dev/null
[ -f "${changed_controller_dir}/reopened.json" ] ||
  fail "a suppressed controller alert whose representative pattern changed was not reopened"
jq -e '.ids | sort == ["rotated-controller-alert", "rotated-vpa-event-alert"]' "${changed_controller_dir}/reopened.json" >/dev/null ||
  fail "the controller-pattern revalidation reopened the wrong alert set"
[ ! -e "${changed_controller_dir}/suppressed.json" ] ||
  fail "a changed controller pattern was suppressed again"
pass "changed patterns and the retired VPA denial exemption automatically become visible again"
