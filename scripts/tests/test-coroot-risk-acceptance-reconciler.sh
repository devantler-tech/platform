#!/usr/bin/env bash

# Contract for declarative Coroot risk acceptance. The allowlist is reviewed as
# GitOps data; the reconciler may dismiss only declared exact pairs or the two
# anchored generated-name patterns, reactivate every undeclared dismissal,
# and correct a declared pair whose reason drifted. Other new risks stay active.

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
  type == "array" and length == 39 and
  all(.[] | select(has("application"));
    (.application | type == "string") and
    (.application | split(":") | length == 3) and
    .category == "Availability" and
    (.type | IN("single-instance-app", "unreplicated-database")) and
    (.reason | startswith("platform#3812: "))
  ) and
  ([.[] | select(has("application_pattern"))] | length == 2) and
  any(.[]; .application_pattern? == "^longhorn-system:InstanceManager:instance-manager-[0-9a-f]{32}\\z" and
    .category == "Availability" and .type == "single-instance-app" and
    (.reason | startswith("platform#4117: "))) and
  any(.[]; .application_pattern? == "^crossplane-system:Deployment:provider-upjet-github-[0-9a-f]{12}\\z" and
    .category == "Availability" and .type == "single-instance-app" and
    (.reason | startswith("platform#4145: "))) and
  ([.[] | [(.application // .application_pattern), .category, .type] | @tsv] | length) ==
    ([.[] | [(.application // .application_pattern), .category, .type] | @tsv] | unique | length)
' "${acceptances_file}" >/dev/null ||
  fail 'the availability acceptance allowlist must contain exact entries and two narrow generated-name patterns'

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
  'crossview:Deployment:crossview-postgres'
  'kube-system:Deployment:descheduler'
  'external-dns:Deployment:external-dns'
  'ascoachingogvaner:Deployment:external-dns'
  'flux-system:Deployment:flux-operator'
  'headlamp:Deployment:headlamp'
  'kube-system:Deployment:hubble-ui'
  'longhorn-system:Deployment:longhorn-driver-deployer'
  'longhorn-system:InstanceManager:instance-manager-0c0363730e10c9272de1f53e608f8860'
  'longhorn-system:InstanceManager:instance-manager-0d470075ebcae8875301aa53419e1d24'
  'longhorn-system:InstanceManager:instance-manager-1d6354ff91a9ad3b1922050e547fde38'
  'longhorn-system:InstanceManager:instance-manager-2c5e0312dd2690e02ad4cfbc6a7b2b25'
  'longhorn-system:InstanceManager:instance-manager-44efed9835a6c0c7c26e0d4ed66b757b'
  'longhorn-system:InstanceManager:instance-manager-c210015368cfb1db9feb55fd74623baa'
  'kubescape:Deployment:kubescape'
  'kubescape:Deployment:kubevuln'
  'kubescape:Deployment:operator'
  'cnpg-system:Deployment:plugin-barman-cloud'
  'crossplane-system:Deployment:provider-aws-iam-87e81d392ac1'
  'crossplane-system:Deployment:provider-upjet-github-2801aa72907d'
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

if ! jq -e '
  . as $acceptances |
  ["unreplicated-database", "single-instance-app"] |
  all(.[]; . as $type |
    any($acceptances[];
      .application == "crossview:Deployment:crossview-postgres" and
      .category == "Availability" and
      .type == $type
    )
  )
' "${acceptances_file}" >/dev/null; then
  fail 'Crossview PostgreSQL must bind both reviewed availability risks exactly'
fi

if ! jq -e '
  any(.[];
    .application == "longhorn-system:Deployment:longhorn-driver-deployer" and
    .category == "Availability" and
    .type == "single-instance-app"
  )
' "${acceptances_file}" >/dev/null; then
  fail 'Longhorn driver deployer must bind the reviewed single-instance risk exactly'
fi

for prohibited in alertmanager csi-snapshotter origin-ca-issuer; do
  if jq -e --arg prohibited "${prohibited}" \
    'any(.[]; (.application // .application_pattern) | contains($prohibited))' "${acceptances_file}" >/dev/null; then
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
  elif [ "${mode}" = 'longhorn' ]; then
    jq -n '{data:{risks:[
      {application_id:"95rsc5yp:longhorn-system:InstanceManager:instance-manager-9c4995fb1b807430b1d54d466d640e9f",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:longhorn-system:InstanceManager:instance-manager-9c4995fb1b807430b1d54d466d640e9f\n",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:longhorn-system:Deployment:instance-manager-9c4995fb1b807430b1d54d466d640e9f",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:other:InstanceManager:instance-manager-9c4995fb1b807430b1d54d466d640e9f",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:longhorn-system:InstanceManager:instance-manager-not-a-hash",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:longhorn-system:InstanceManager:instance-manager-9c4995fb1b807430b1d54d466d640e9f",key:{category:"Security",type:"single-instance-app"}}
    ]}}' >"${dir}/risks.json"
  elif [ "${mode}" = 'github-provider' ]; then
    jq -n '{data:{risks:[
      {application_id:"95rsc5yp:crossplane-system:Deployment:provider-upjet-github-2801aa72907d",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:other:Deployment:provider-upjet-github-2801aa72907d",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:crossplane-system:StatefulSet:provider-upjet-github-2801aa72907d",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:crossplane-system:Deployment:provider-upjet-github-not-a-revision",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:crossplane-system:Deployment:provider-upjet-unifi-2801aa72907d",key:{category:"Availability",type:"single-instance-app"}},
      {application_id:"95rsc5yp:crossplane-system:Deployment:provider-upjet-github-2801aa72907d",key:{category:"Security",type:"single-instance-app"}}
    ]}}' >"${dir}/risks.json"
  elif [ "${mode}" = 'retired-pattern' ]; then
    jq -n '{data:{risks:[
      {application_id:"95rsc5yp:longhorn-system:InstanceManager:instance-manager-9c4995fb1b807430b1d54d466d640e9f",key:{category:"Availability",type:"single-instance-app"},dismissal:{reason:"platform#4117: previously accepted"}}
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

longhorn_dir="$(setup_scenario longhorn longhorn)"
run_scenario "${longhorn_dir}" >/dev/null
jq -s -e '
  length == 1 and
  .[0].payload.action == "dismiss" and
  .[0].payload.key == {category:"Availability",type:"single-instance-app"} and
  (.[0].payload.reason | startswith("platform#4117: ")) and
  (.[0].url | contains("longhorn-system%3AInstanceManager%3Ainstance-manager-9c4995fb1b807430b1d54d466d640e9f"))
' "${longhorn_dir}/posts.jsonl" >/dev/null ||
  fail 'a new node-scoped Longhorn instance manager must be accepted without matching lookalikes'

github_provider_dir="$(setup_scenario github-provider github-provider)"
run_scenario "${github_provider_dir}" >/dev/null
jq -s -e '
  length == 1 and
  .[0].payload.action == "dismiss" and
  .[0].payload.key == {category:"Availability",type:"single-instance-app"} and
  (.[0].payload.reason | startswith("platform#4145: ")) and
  (.[0].url | contains("crossplane-system%3ADeployment%3Aprovider-upjet-github-2801aa72907d"))
' "${github_provider_dir}/posts.jsonl" >/dev/null ||
  fail 'a rotated GitHub provider revision must be accepted without matching lookalikes'

broad_file="${work_root}/broad-risks.json"
jq 'map(if has("application_pattern") then .application_pattern = ".*" else . end)' \
  "${acceptances_file}" >"${broad_file}"
broad_dir="$(setup_scenario broad longhorn)"
if COROOT_BASE_URL='http://coroot.test' ACCEPTANCES_FILE="${broad_file}" \
  SCENARIO_DIR="${broad_dir}" PATH="${broad_dir}/bin:${PATH}" \
  /bin/sh -c "${script_body}" >/dev/null 2>&1; then
  fail 'widening the reviewed Longhorn name pattern must fail closed'
fi
[ ! -s "${broad_dir}/posts.jsonl" ] ||
  fail 'a widened pattern must be rejected before any Coroot mutation'

wrong_type_file="${work_root}/wrong-type-risks.json"
jq 'map(if has("application_pattern") then .type = "single-node-app" else . end)' \
  "${acceptances_file}" >"${wrong_type_file}"
wrong_type_dir="$(setup_scenario wrong-type longhorn)"
if COROOT_BASE_URL='http://coroot.test' ACCEPTANCES_FILE="${wrong_type_file}" \
  SCENARIO_DIR="${wrong_type_dir}" PATH="${wrong_type_dir}/bin:${PATH}" \
  /bin/sh -c "${script_body}" >/dev/null 2>&1; then
  fail 'changing the reviewed Longhorn risk type must fail closed'
fi
[ ! -s "${wrong_type_dir}/posts.jsonl" ] ||
  fail 'a changed Longhorn risk type must be rejected before any Coroot mutation'

without_pattern_file="${work_root}/without-pattern-risks.json"
jq 'map(select(has("application_pattern") | not))' \
  "${acceptances_file}" >"${without_pattern_file}"
retired_pattern_dir="$(setup_scenario retired-pattern retired-pattern)"
COROOT_BASE_URL='http://coroot.test' ACCEPTANCES_FILE="${without_pattern_file}" \
  SCENARIO_DIR="${retired_pattern_dir}" PATH="${retired_pattern_dir}/bin:${PATH}" \
  /bin/sh -c "${script_body}" >/dev/null ||
  fail 'removing the reviewed Longhorn pattern must remain reconcilable'
jq -s -e '
  length == 1 and
  .[0].payload.action == "mark_as_active" and
  (.[0].url | contains("longhorn-system%3AInstanceManager%3Ainstance-manager-9c4995fb1b807430b1d54d466d640e9f"))
' "${retired_pattern_dir}/posts.jsonl" >/dev/null ||
  fail 'removing the Longhorn pattern must reactivate its previously dismissed risks'

ambiguous_dir="$(setup_scenario ambiguous ambiguous)"
if run_scenario "${ambiguous_dir}" >/dev/null 2>&1; then
  fail 'an ambiguous live application identity must fail closed'
fi
[ ! -s "${ambiguous_dir}/posts.jsonl" ] ||
  fail 'the ambiguity preflight must complete before any Coroot mutation'

printf 'PASS: Coroot risk dismissals follow the reviewed exact and narrow generated-name allowlist\n'
