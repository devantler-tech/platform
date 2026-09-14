#!/usr/bin/env bash

# Contract for declarative Coroot risk acceptance. The allowlist is reviewed as
# GitOps data; the reconciler may dismiss only those exact application/risk-key
# pairs, reactivate every undeclared dismissal, and correct a declared pair whose
# reason drifted. Newly discovered active risks remain untouched.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly acceptances_manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/config-map-risk-acceptances.yaml"
readonly reconciler_manifest="${root_dir}/k8s/providers/hetzner/infrastructure/coroot/cron-job-risk-acceptance-reconciler.yaml"
readonly infrastructure_kustomization="${root_dir}/k8s/providers/hetzner/infrastructure/kustomization.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

for tool in yq jq shellcheck; do
  command -v "${tool}" >/dev/null 2>&1 || fail "${tool} is required"
done

if [ ! -f "${acceptances_manifest}" ] || [ ! -f "${reconciler_manifest}" ]; then
  fail 'declarative Coroot risk acceptance manifests are missing'
fi

if ! yq e -e '.resources[] == "coroot/config-map-risk-acceptances.yaml"' \
  "${infrastructure_kustomization}" >/dev/null ||
  ! yq e -e '.resources[] == "coroot/cron-job-risk-acceptance-reconciler.yaml"' \
    "${infrastructure_kustomization}" >/dev/null; then
  fail 'production must reconcile the declared Coroot risk acceptances'
fi

work_root="$(mktemp -d /tmp/coroot-risk-acceptance.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT
acceptances_file="${work_root}/risks.json"
yq e -r '.data."risks.json"' "${acceptances_manifest}" >"${acceptances_file}"

jq -e '
  type == "array" and length == 33 and
  all(.[ ];
    (.application | type == "string") and
    (.application | split(":") | length == 3) and
    .category == "Availability" and
    .type == "single-instance-app" and
    (.reason | startswith("platform#3812: "))
  ) and
  ([.[] | [.application, .category, .type] | @tsv] | length) ==
    ([.[] | [.application, .category, .type] | @tsv] | unique | length)
' "${acceptances_file}" >/dev/null ||
  fail 'the availability acceptance allowlist must contain 33 unique, reasoned singleton entries'

expected_applications=(
  'actual-budget:Deployment:actual-budget-actualbudget'
  'backstage:Deployment:backstage'
  'kube-system:Deployment:cluster-autoscaler-hetzner-cluster-autoscaler'
  'observability:Deployment:coroot-cluster-agent'
  'observability:Deployment:coroot-operator'
  'observability:Deployment:coroot-prometheus'
  'crossplane-system:Deployment:crossplane'
  'crossplane-system:Deployment:crossplane-rbac-manager'
  'observability:Deployment:crossplane-sync-exporter'
  'crossview:Deployment:crossview'
  'kube-system:Deployment:descheduler'
  'external-dns:Deployment:external-dns'
  'ascoachingogvaner:Deployment:external-dns'
  'flux-system:Deployment:flux-operator'
  'headlamp:Deployment:headlamp'
  'kube-system:Deployment:hubble-ui'
  'longhorn-system:InstanceManager:instance-manager-0c0363730e10c9272de1f53e608f8860'
  'longhorn-system:InstanceManager:instance-manager-0d470075ebcae8875301aa53419e1d24'
  'longhorn-system:InstanceManager:instance-manager-1d6354ff91a9ad3b1922050e547fde38'
  'longhorn-system:InstanceManager:instance-manager-2c5e0312dd2690e02ad4cfbc6a7b2b25'
  'longhorn-system:InstanceManager:instance-manager-44efed9835a6c0c7c26e0d4ed66b757b'
  'kubescape:Deployment:kubescape'
  'kubescape:Deployment:kubevuln'
  'kubescape:Deployment:operator'
  'cnpg-system:Deployment:plugin-barman-cloud'
  'crossplane-system:Deployment:provider-aws-iam-87e81d392ac1'
  'crossplane-system:Deployment:provider-upjet-github-2516bc50dd55'
  'crossplane-system:Deployment:provider-upjet-unifi-5523eabd691d'
  'flux-system:Deployment:source-controller'
  'kubescape:Deployment:storage'
  'crossplane-system:Deployment:upbound-provider-family-aws-1d3725bd4a0b'
  'velero:Deployment:velero'
  'whoami:Deployment:whoami'
)

for application in "${expected_applications[@]}"; do
  jq -e --arg application "${application}" \
    'any(.[]; .application == $application)' "${acceptances_file}" >/dev/null ||
    fail "missing reviewed risk acceptance for ${application}"
done

for prohibited in alertmanager csi-snapshotter origin-ca-issuer crossview-postgres; do
  if jq -e --arg prohibited "${prohibited}" \
    'any(.[]; .application | contains($prohibited))' "${acceptances_file}" >/dev/null; then
    fail "${prohibited} must be fixed or held, not dismissed"
  fi
done

yq e -e '
  .spec.schedule == "*/15 * * * *" and
  .spec.concurrencyPolicy == "Forbid" and
  .spec.jobTemplate.spec.backoffLimit == 0 and
  .spec.jobTemplate.spec.template.spec.automountServiceAccountToken == false and
  .spec.jobTemplate.spec.template.spec.containers[0].volumeMounts[0].readOnly == true and
  .spec.jobTemplate.spec.template.spec.volumes[0].configMap.name == "coroot-risk-acceptances"
' "${reconciler_manifest}" >/dev/null ||
  fail 'the reconciler must be bounded, non-overlapping, tokenless, and mount the declared allowlist read-only'

script_body="$(yq e -r '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
  "${reconciler_manifest}" | sed 's/\$\${/${/g')"
if [ -z "${script_body}" ] || [ "${script_body}" = 'null' ]; then
  fail 'could not extract the Coroot risk acceptance reconciler'
fi
printf '%s\n' "${script_body}" | shellcheck -s sh -

setup_scenario() {
  local name="$1" mode="$2" dir
  local backstage_reason
  dir="${work_root}/${name}"
  mkdir -p "${dir}/bin"
  backstage_reason="$(jq -r '.[] | select(.application == "backstage:Deployment:backstage") | .reason' "${acceptances_file}")"

  jq -n '{data:{projects:[{id:"95rsc5yp",name:"platform"}]}}' >"${dir}/user.json"
  if [ "${mode}" = 'normal' ]; then
    jq -n --arg backstage_reason "${backstage_reason}" '{data:{risks:[
      {application_id:"95rsc5yp:actual-budget:Deployment:actual-budget-actualbudget",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:backstage:Deployment:backstage",key:{category:"Availability",type:"single-instance-app"},dismissal:{reason:$backstage_reason}},
      {application_id:"95rsc5yp:retired:Deployment:old-tool",key:{category:"Availability",type:"single-instance-app"},dismissal:{reason:"manual dismissal outside GitOps"}},
      {application_id:"95rsc5yp:new-service:Deployment:new-service",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:database:StatefulSet:database",key:{category:"Security",type:"db-internet-exposure"},dismissal:{reason:"owned by a separate security policy"}}
    ]}}' >"${dir}/risks.json"
  elif [ "${mode}" = 'drift' ]; then
    jq -n '{data:{risks:[
      {application_id:"95rsc5yp:actual-budget:Deployment:actual-budget-actualbudget",key:{category:"Availability",type:"single-instance-app"},dismissal:{reason:"manual operator decision"}}
    ]}}' >"${dir}/risks.json"
  else
    jq -n '{data:{risks:[
      {application_id:"cluster-one:actual-budget:Deployment:actual-budget-actualbudget",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"cluster-two:actual-budget:Deployment:actual-budget-actualbudget",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:retired:Deployment:old-tool",key:{category:"Availability",type:"single-instance-app"},dismissal:{reason:"platform#3812: removed declaration"}}
    ]}}' >"${dir}/risks.json"
  fi

  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
url=''
payload=''
previous=''
for argument in "$@"; do
  [ "${previous}" = '-d' ] && payload="${argument}"
  case "${argument}" in http://*) url="${argument}" ;; esac
  previous="${argument}"
done
case "${url}" in
  */api/user) jq . "${SCENARIO_DIR}/user.json" ;;
  */overview/risks) jq . "${SCENARIO_DIR}/risks.json" ;;
  */risks)
    jq -cn --arg url "${url}" --argjson payload "${payload}" \
      '{url:$url,payload:$payload}' >>"${SCENARIO_DIR}/posts.jsonl"
    printf '{"data":{}}\n'
    ;;
  *) printf 'unstubbed curl URL: %s\n' "${url}" >&2; exit 1 ;;
esac
STUB
  chmod +x "${dir}/bin/curl"
  printf '%s' "${dir}"
}

run_scenario() {
  local dir="$1"
  COROOT_BASE_URL='http://coroot.test' ACCEPTANCES_FILE="${acceptances_file}" \
    SCENARIO_DIR="${dir}" PATH="${dir}/bin:${PATH}" /bin/sh -c "${script_body}"
}

normal_dir="$(setup_scenario normal normal)"
run_scenario "${normal_dir}" >/dev/null
jq -s -e '
  length == 2 and
  any(.[]; .payload.action == "dismiss" and
    .payload.key == {category:"Availability",type:"single-instance-app"} and
    (.url | contains("actual-budget"))) and
  any(.[]; .payload.action == "mark_as_active" and
    (.url | contains("old-tool")))
' "${normal_dir}/posts.jsonl" >/dev/null ||
  fail 'reconciliation must dismiss the declared active risk and reactivate the undeclared dismissed risk only'

drift_dir="$(setup_scenario drift drift)"
run_scenario "${drift_dir}" >/dev/null
actual_reason="$(jq -r '.[] | select(.application == "actual-budget:Deployment:actual-budget-actualbudget") | .reason' "${acceptances_file}")"
jq -s -e --arg reason "${actual_reason}" '
  length == 1 and
  .[0].payload.action == "dismiss" and
  .[0].payload.reason == $reason
' "${drift_dir}/posts.jsonl" >/dev/null ||
  fail 'a drifted dismissal reason must converge to the declared reason'

ambiguous_dir="$(setup_scenario ambiguous ambiguous)"
if run_scenario "${ambiguous_dir}" >/dev/null 2>&1; then
  fail 'an ambiguous live application identity must fail closed'
fi
[ ! -s "${ambiguous_dir}/posts.jsonl" ] ||
  fail 'the ambiguity preflight must complete before any Coroot mutation'

printf 'PASS: Coroot risk dismissals are reconciled exclusively from the reviewed GitOps allowlist\n'
