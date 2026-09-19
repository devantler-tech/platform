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
[ -f "${exact_dir}/suppressed.json" ] ||
  fail "the exact kubelet health-probe series were not suppressed"
jq -e '.ids == ["kubelet-probe-noise"]' "${exact_dir}/suppressed.json" >/dev/null ||
  fail "the exact scenario suppressed the wrong alert set"
pass "exact kubelet scheduler/controller-manager probe noise is suppressed"
pass "normal autosuppressor lifecycle output is structured as info"

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
  }
]}}
JSON
run_scenario "${event_dir}" >/dev/null
[ -f "${event_dir}/suppressed.json" ] ||
  fail "the exact by-design Kubernetes lifecycle events were not suppressed"
jq -e '.ids | sort == ["auto-vpa-write-conflict", "flux-build-canceled", "flux-dryrun-canceled", "flux-health-canceled", "flux-status-canceled", "monitor-secret-race"]' \
  "${event_dir}/suppressed.json" >/dev/null || {
  jq -c '.ids | sort' "${event_dir}/suppressed.json" >&2
  fail "the event exemptions were broader than their exact label and message contracts"
}
pass "exact self-healing lifecycle events are suppressed while near matches stay visible"

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
  }
]}}
JSON
run_scenario "${pinned_logs_dir}" >/dev/null
[ -f "${pinned_logs_dir}/suppressed.json" ] ||
  fail "the exact control-plane and runtime log fingerprints were not suppressed"
jq -e '.ids | sort == ["7f3auk0cezgo", "9qrrrlq3eooh", "h9q7onv4l20i", "rotated-post-timeout-alert", "rotated-runtime-alert", "rotated-vpa-event-alert", "sandbox-resize-alert"]' \
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
      {"name":"Sample","value":"E0919 03:00:00.000000       1 event.go:359] \"Server rejected event (will not retry!)\" err=\"forbidden: cannot patch resource \\\"pods\\\"\""}
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
pass "a changed weakly-equal controller pattern automatically becomes visible again"
