#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
umami_dir="${root_dir}/k8s/bases/apps/umami"
exception_dir="${root_dir}/k8s/bases/infrastructure/cluster-security-exceptions"
token_exception="${exception_dir}/umami-provisioning-service-account-token.yaml"
apps_flux_kustomization="${root_dir}/k8s/clusters/base/flux-kustomization-apps.yaml"

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
jq -e '.metadata.annotations["kustomize.toolkit.fluxcd.io/force"] == "enabled"' \
  <<<"${raw_job_json}" >/dev/null ||
  fail 'bootstrap Job must use Flux lowercase force annotation for immutable updates'
jq -e '
  .metadata.annotations["checkov.io/skip1"] == "CKV_K8S_8=inert source template is replaced before apply" and
  .metadata.annotations["checkov.io/skip2"] == "CKV_K8S_9=inert source template is replaced before apply" and
  ((.spec.template.metadata.annotations // {}) | has("checkov.io/skip1") | not) and
  ((.spec.template.metadata.annotations // {}) | has("checkov.io/skip2") | not)
' <<<"${raw_job_json}" >/dev/null ||
  fail 'raw Job Checkov suppressions must be resource-scoped at top-level metadata'

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
jq -e '.spec.activeDeadlineSeconds >= 1500' <<<"${job_json}" >/dev/null ||
  fail 'bootstrap Job deadline must cover legacy drain, a contending worker, its own login retries, and recovery margin'
yq eval -e '.spec.timeout == "30m"' "${apps_flux_kustomization}" >/dev/null ||
  fail 'apps reconciliation timeout must outlive the bootstrap Job deadline'

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
  .spec.match.resources[0].name == "^umami-provision-tenants$" and
  .spec.match.resources[1].apiGroup == "batch" and
  .spec.match.resources[1].kind == "Job" and
  .spec.match.resources[1].name == "^umami-provision-tenants-bootstrap$"
' "${token_exception}" >/dev/null ||
  fail 'the token exception must cover only the two Umami provisioning workloads'
grep -Fxq '  - umami-provisioning-service-account-token.yaml' "${exception_dir}/kustomization.yaml" ||
  fail 'the workload-scoped token exception must be rendered by the Platform'
jq -e '.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true' <<<"${job_json}" >/dev/null ||
  fail 'bootstrap provisioning must keep the container root filesystem immutable'
jq -e '
  .spec.template.spec.containers[0].securityContext |
  .allowPrivilegeEscalation == false and
  .runAsNonRoot == true and
  .capabilities.drop == ["ALL"]
' <<<"${job_json}" >/dev/null ||
  fail 'bootstrap provisioning must satisfy the enforced container security policy'

jq -e '.metadata.annotations["kustomize.toolkit.fluxcd.io/ssa"] == "IfNotPresent"' <<<"${lease_json}" >/dev/null ||
  fail 'Flux must create the Lease once without overwriting live lock state'
jq -e '.metadata.annotations["kustomize.toolkit.fluxcd.io/prune"] == "disabled"' <<<"${lease_json}" >/dev/null ||
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
grep -Fq 'const leaseDurationSeconds = 60;' <<<"${provisioner_script}" ||
  fail 'a crashed provisioner must leave only a short-lived Lease'
grep -Fq "const leaseMicroTime = () => new Date().toISOString().replace('Z', '000Z');" \
  <<<"${provisioner_script}" ||
  fail 'Lease timestamps must use the six fractional digits Kubernetes MicroTime requires'
[[ "$(grep -Fc 'leaseMicroTime' <<<"${provisioner_script}")" -ge 3 ]] ||
  fail 'Lease acquisition and renewal must share the Kubernetes-compatible clock'
grep -Fq 'async function renewProvisioningLease()' <<<"${provisioner_script}" ||
  fail 'an active provisioner must renew its short-lived Lease'
grep -Fq 'setInterval' <<<"${provisioner_script}" ||
  fail 'Lease renewal must continue throughout provisioning'
grep -Fq 'clearInterval' <<<"${provisioner_script}" ||
  fail 'Lease renewal must stop before release'
grep -Fq 'const leaseRenewalSafetyMarginMilliseconds = 15000;' <<<"${provisioner_script}" ||
  fail 'renewal retries must stop safely before the held Lease can expire'
grep -Fq 'lost.leaseOwnershipLost = true;' <<<"${provisioner_script}" ||
  fail 'Lease ownership loss must remain distinguishable from transient API failures'
grep -Fq 'let renewalWatchdog = setTimeout' <<<"${provisioner_script}" ||
  fail 'a transient renewal failure needs an independent Lease-expiry watchdog'
grep -Fq 'clearTimeout(renewalWatchdog);' <<<"${provisioner_script}" ||
  fail 'successful renewal and shutdown must clear the prior expiry watchdog'
grep -Fq 'if (e && e.leaseOwnershipLost) process.exit(provisioningFailureExitCode);' \
  <<<"${provisioner_script}" ||
  fail 'lost Lease ownership must stop provisioning immediately'
grep -Fq 'provisioning Lease renewal will retry:' <<<"${provisioner_script}" ||
  fail 'transient renewal failures must stay observable while later heartbeats retry'
grep -Fq 'req.setTimeout(10000' <<<"${provisioner_script}" ||
  fail 'a hung Lease API request must fail before the Lease can expire'
grep -Fq 'const umamiRequestTimeoutMilliseconds = 10000;' <<<"${provisioner_script}" ||
  fail 'a hung Umami HTTP operation must remain bounded inside the Job deadline'
grep -Fq 'const umamiProvisioningDeadline = Date.now() + 1200000;' <<<"${provisioner_script}" ||
  fail 'all Umami retries must share a deadline with recovery margin before the Job deadline'
grep -Fq 'const remainingRequestMilliseconds = umamiProvisioningDeadline - Date.now();' <<<"${provisioner_script}" ||
  fail 'each Umami fetch attempt must respect the shared provisioning deadline'
grep -Fq 'signal: AbortSignal.timeout(Math.min(umamiRequestTimeoutMilliseconds, remainingRequestMilliseconds))' <<<"${provisioner_script}" ||
  fail 'every Umami fetch attempt must bound both headers and response-body reads'
grep -Fq 'const fetchOnce = async (u, o) =>' <<<"${provisioner_script}" ||
  fail 'non-idempotent Umami writes need a bounded single-attempt primitive'
grep -Fq "const c = await fetchOnce(base + '/api/teams'," <<<"${provisioner_script}" ||
  fail 'team creation must not blindly retry an ambiguous POST'
grep -Fq "console.warn('team creation response ambiguous; re-listing before retry:'," <<<"${provisioner_script}" ||
  fail 'an ambiguous team create must remain observable'
grep -Fq 'return await ensureTeam(token, name);' <<<"${provisioner_script}" ||
  fail 'an ambiguous team create must re-list before deciding to retry'
grep -Fq "if (Date.now() >= umamiProvisioningDeadline) throw new Error('Umami provisioning deadline exceeded while waiting for Lease');" <<<"${provisioner_script}" ||
  fail 'Lease contention must reach workload-specific failure handling before the Job deadline'
grep -Fq 'waiting for Umami provisioning Lease holder:' <<<"${provisioner_script}" ||
  fail 'a contending bootstrap must wait for the current Lease holder'
grep -Fq 'await sleep(Math.min(5000, Math.max(250, expiresAt - Date.now())))' \
  <<<"${provisioner_script}" ||
  fail 'Lease contention must retry on a bounded interval'
grep -Fq 'read.status === 403 || read.status === 404' <<<"${provisioner_script}" ||
  fail 'bootstrap must wait when Flux has not applied the Lease prerequisites yet'
grep -Fq 'waiting for Umami provisioning Lease prerequisites:' <<<"${provisioner_script}" ||
  fail 'Lease prerequisite retries must remain observable'
grep -Fq 'const leasePrerequisiteWaitMilliseconds = 60000;' <<<"${provisioner_script}" ||
  fail 'missing Lease prerequisites must stop retrying before the Job deadline'
grep -Fq 'Date.now() >= leasePrerequisiteWaitDeadline' <<<"${provisioner_script}" ||
  fail 'Lease prerequisite retries must hand control back to workload-specific failure handling'
grep -Fq 'const legacyProvisionerDrainSeconds = 480;' <<<"${provisioner_script}" ||
  fail 'the initial rollout must drain pre-Lease CronJob workers before provisioning'
grep -Fq "const leaseCreatedMillis = Date.parse((lease.metadata || {}).creationTimestamp || '');" \
  <<<"${provisioner_script}" ||
  fail 'legacy-worker drain must be scoped to the first creation of the persistent Lease'
grep -Fq 'waiting for pre-Lease Umami provisioners to drain' <<<"${provisioner_script}" ||
  fail 'legacy-worker drain must remain observable'
if grep -Fq 'return false;' <<<"${provisioner_script}"; then
  fail 'Lease contention must not complete a one-shot bootstrap without provisioning'
fi

jq -e '
  .spec.template.spec.containers[0].env[] |
  select(.name == "UMAMI_PROVISION_JOB_NAME") |
  .valueFrom.fieldRef.fieldPath == "metadata.labels['"'"'batch.kubernetes.io/job-name'"'"']"
' <<<"${job_json}" >/dev/null ||
  fail 'the shared template must identify the owning Job from its controller label'
grep -Fq "const bestEffortBootstrap = process.env.UMAMI_PROVISION_JOB_NAME === 'umami-provision-tenants-bootstrap';" \
  <<<"${provisioner_script}" ||
  fail 'only the immediate bootstrap may downgrade a provisioning failure'
grep -Fq 'const provisioningFailureExitCode = bestEffortBootstrap ? 0 : 1;' \
  <<<"${provisioner_script}" ||
  fail 'the bootstrap must complete while scheduled failures remain visible'
error_boundary_line="$(grep -nF '(async () => {' <<<"${provisioner_script}" | head -n 1 | cut -d: -f1)"
secret_initialization_line="$(grep -nF "const newPw = readSecretFile('umami-admin/password');" <<<"${provisioner_script}" | cut -d: -f1)"
tenant_initialization_line="$(grep -nF 'const tenants = JSON.parse(process.env.TENANTS);' <<<"${provisioner_script}" | cut -d: -f1)"
[[ -n "${error_boundary_line}" && -n "${secret_initialization_line}" && -n "${tenant_initialization_line}" ]] ||
  fail 'bootstrap initialization and its workload-specific error boundary must remain present'
[[ "${error_boundary_line}" -lt "${secret_initialization_line}" && "${error_boundary_line}" -lt "${tenant_initialization_line}" ]] ||
  fail 'secret and tenant initialization errors must use workload-specific failure handling'
[[ "$(grep -Fc 'process.exit(provisioningFailureExitCode);' <<<"${provisioner_script}")" -ge 2 ]] ||
  fail 'both heartbeat and provisioning failures must use the workload-specific result'
if grep -Fq 'process.exit(1)' <<<"${provisioner_script}"; then
  fail 'an unconditional failure path can retain a failed bootstrap and wedge Flux'
fi
grep -Fq 'best-effort bootstrap failed; the scheduled reconciler will retry' \
  <<<"${provisioner_script}" ||
  fail 'a failed immediate bootstrap must explain the scheduled recovery path'
grep -Fq 'provisioning Lease release failed:' <<<"${provisioner_script}" ||
  fail 'Lease release failure must be logged without replacing the provisioning result'
grep -Fq 'try { await releaseProvisioningLease(); }' <<<"${provisioner_script}" ||
  fail 'best-effort Lease release must preserve the original provisioning error'

job_health_count="$(yq eval '[.spec.healthCheckExprs[] | select(.apiVersion == "batch/v1" and .kind == "Job")] | length' "${apps_flux_kustomization}")"
[[ "${job_health_count}" == 0 ]] ||
  fail 'Flux native Job health takes precedence over CEL overrides; do not install a dead bypass rule'

printf 'Umami bootstrap is one-shot, immutable, and serialized with the scheduled reconciler.\n'
