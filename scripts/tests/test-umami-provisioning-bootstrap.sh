#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
umami_dir="${root_dir}/k8s/bases/apps/umami"
exception_dir="${root_dir}/k8s/bases/infrastructure/cluster-security-exceptions"
token_exception="${exception_dir}/umami-provisioning-service-account-token.yaml"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

for dependency in kubectl yq jq; do
  command -v "${dependency}" >/dev/null 2>&1 || fail "${dependency} is required"
done

raw_job_json="$(yq -o=json -I=0 '.' "${umami_dir}/job.yaml")"
jq -e '
  .spec.template.spec |
  (.containers | type == "array" and length > 0) and
  (.restartPolicy == "Never")
' <<<"${raw_job_json}" >/dev/null ||
  fail 'raw bootstrap Job must remain schema-valid before Kustomize replacement'

rendered_file="$(mktemp)"
cron_spec_file="$(mktemp)"
job_spec_file="$(mktemp)"
trap 'rm -f "${rendered_file}" "${cron_spec_file}" "${job_spec_file}"' EXIT

kubectl kustomize "${umami_dir}" >"${rendered_file}" || fail 'Umami base must render'

resource_json() {
  local kind="$1"
  local name="$2"
  yq -o=json -I=0 \
    "select(.kind == \"${kind}\" and .metadata.name == \"${name}\")" \
    "${rendered_file}"
}

cron_json="$(resource_json CronJob umami-provision-tenants)"
job_json="$(resource_json Job umami-provision-tenants-bootstrap)"
lease_json="$(resource_json Lease umami-provision-tenants)"
role_json="$(resource_json Role umami-provision-tenants)"

[[ -n "${cron_json}" ]] || fail 'scheduled Umami provisioner CronJob is missing'
[[ -n "${job_json}" ]] || fail 'immediate Umami bootstrap Job is missing'
[[ -n "${lease_json}" ]] || fail 'shared Umami provisioning Lease is missing'
[[ -n "${role_json}" ]] || fail 'Lease-scoped Umami provisioner Role is missing'

jq -S '.spec.jobTemplate.spec' <<<"${cron_json}" >"${cron_spec_file}"
jq -S '.spec' <<<"${job_json}" >"${job_spec_file}"
cmp -s "${cron_spec_file}" "${job_spec_file}" ||
  fail 'bootstrap Job must use the CronJob controller template verbatim'

jq -e '.spec | has("ttlSecondsAfterFinished") | not' <<<"${job_json}" >/dev/null ||
  fail 'bootstrap Job must remain completed so Flux cannot recreate it after TTL cleanup'
jq -e '.spec.template.spec.serviceAccountName == "umami-provision-tenants"' <<<"${job_json}" >/dev/null ||
  fail 'both provisioners must use the Lease-scoped service account'
jq -e '.spec.template.spec.automountServiceAccountToken == true' <<<"${job_json}" >/dev/null ||
  fail 'the provisioner needs its scoped service-account token to coordinate'
[[ -f "${token_exception}" ]] ||
  fail 'the required provisioner token must carry a workload-scoped Kubescape exception'
yq eval -e '
  (.spec.posture | length) == 1 and
  .spec.posture[0].controlID == "C-0034" and
  .spec.posture[0].action == "ignore" and
  (.spec.match.resources | length) == 2 and
  .spec.match.resources[0].apiGroup == "batch" and
  .spec.match.resources[0].kind == "CronJob" and
  .spec.match.resources[0].name == "umami-provision-tenants" and
  .spec.match.resources[1].apiGroup == "batch" and
  .spec.match.resources[1].kind == "Job" and
  .spec.match.resources[1].name == "umami-provision-tenants-bootstrap"
' "${token_exception}" >/dev/null ||
  fail 'the token exception must cover only the two Umami provisioning workloads'
grep -Fxq '  - umami-provisioning-service-account-token.yaml' "${exception_dir}/kustomization.yaml" ||
  fail 'the workload-scoped token exception must be rendered by the Platform'
jq -e '.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true' <<<"${job_json}" >/dev/null ||
  fail 'bootstrap provisioning must keep the container root filesystem immutable'

jq -e '.metadata.annotations["kustomize.toolkit.fluxcd.io/ssa"] == "IfNotPresent"' <<<"${lease_json}" >/dev/null ||
  fail 'Flux must create the Lease once without overwriting live lock state'
jq -e '.metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "Disabled"' <<<"${lease_json}" >/dev/null ||
  fail 'Flux must not prune a live Lease during a rollout'
jq -e '
  .rules == [{
    apiGroups: ["coordination.k8s.io"],
    resources: ["leases"],
    resourceNames: ["umami-provision-tenants"],
    verbs: ["get", "update"]
  }]
' <<<"${role_json}" >/dev/null || fail 'the service account may only read and update its named Lease'

provisioner_script="$(jq -r '.spec.template.spec.containers[0].command[-1]' <<<"${job_json}")"
grep -Fq 'acquireProvisioningLease' <<<"${provisioner_script}" ||
  fail 'the shared provisioner must acquire the Lease before calling Umami'
grep -Fq 'releaseProvisioningLease' <<<"${provisioner_script}" ||
  fail 'the shared provisioner must release the Lease after calling Umami'

printf 'Umami bootstrap is one-shot, immutable, and serialized with the scheduled reconciler.\n'
