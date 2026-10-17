#!/usr/bin/env bash

# Contract for the crossplane-provider-unhealthy-alert CronJob (#3457).
#
# Flux does not gate on Crossplane Provider health, so during #3454 every
# Provider sat Healthy=False while everything else read green. This CronJob is
# the only signal for that shape. It follows the cnpg-degraded-alert pattern,
# and this test follows test-cnpg-degraded-alert.sh: the script is extracted
# from the manifest and run against a stubbed kube API and a stubbed curl.
#
# Asserted in both directions:
#   FIRES on a Provider not Healthy (False, Unknown or missing) past the grace.
#   STAYS QUIET on a healthy fleet and on a Provider unhealthy only inside the
#   grace, which is what a provider upgrade looks like.
#   FAILS LOUDLY on a non-200 or malformed API read and on a failed delivery,
#   instead of recording a silent zero.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly coroot_dir="${root_dir}/k8s/bases/infrastructure/controllers/coroot"
readonly manifest="${coroot_dir}/cron-job-crossplane-provider-unhealthy-alert.yaml"
readonly cluster_role="${coroot_dir}/cluster-role-crossplane-provider-unhealthy-alert.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
readonly mount_path='/etc/crossplane-provider-unhealthy-alert'

# Not a hooks.slack.com-shaped literal (GitHub push protection blocks those);
# `.invalid` is RFC 2606 reserved, so it can never resolve.
readonly REAL_WEBHOOK='https://hooks.test.invalid/delivery-target'
readonly PLACEHOLDER_WEBHOOK='https://example.invalid/no-slack-configured'

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

# ---------------------------------------------------------------------------
# Structural assertions.
# ---------------------------------------------------------------------------

container_path='.spec.jobTemplate.spec.template.spec.containers[0]'
pod_path='.spec.jobTemplate.spec.template.spec'

script_body="$(yq eval "${container_path}.command[2]" "${manifest}")"
[ -n "${script_body}" ] && [ "${script_body}" != "null" ] ||
  fail "could not extract the container script from ${manifest}"

if yq eval "${container_path}.env" "${manifest}" | grep -q 'secretKeyRef'; then
  fail "CKV_K8S_35: the webhook must arrive as a mounted file, not a secretKeyRef"
fi
[ "$(yq eval "${container_path}.volumeMounts[] | select(.name == \"webhook\") | .mountPath" "${manifest}")" = "${mount_path}" ] ||
  fail "the webhook volume is not mounted at ${mount_path}"
[ "$(yq eval "${container_path}.volumeMounts[] | select(.name == \"webhook\") | .readOnly" "${manifest}")" = "true" ] ||
  fail "the webhook mount must be readOnly"
[ "$(yq eval "${pod_path}.volumes[] | select(.name == \"webhook\") | .secret.secretName" "${manifest}")" = "crossplane-provider-unhealthy-alert-webhook" ] ||
  fail "the webhook volume does not name the crossplane-provider-unhealthy-alert-webhook Secret"
# Mode and fsGroup are one invariant: kubelet owns the file root:fsGroup.
[ "$(yq eval "${pod_path}.volumes[] | select(.name == \"webhook\") | .secret.defaultMode" "${manifest}")" = "288" ] ||
  fail "expected the webhook volume defaultMode 0440 (288 decimal)"
[ "$(yq eval "${pod_path}.securityContext.fsGroup" "${manifest}")" = "$(yq eval "${pod_path}.securityContext.runAsUser" "${manifest}")" ] ||
  fail "fsGroup must match runAsUser, or the mounted webhook is unreadable"
pass "webhook arrives as a readOnly 0440 file readable by the pod's fsGroup"

[ "$(yq eval '.metadata.annotations."kustomize.toolkit.fluxcd.io/substitute"' "${manifest}")" = "disabled" ] ||
  fail "Flux substitution must be disabled, or \${GRACE_SECONDS} is blanked on deploy"
pass "Flux substitution is disabled for the CronJob"

# Line-anchored: the curl call spans continuation lines (see the CNPG test).
if grep -Eq '^[[:space:]]*"\$\{?WEBHOOK_URL\}?"[[:space:]]*$' <<<"${script_body}" ||
  grep -Eq 'curl.*"\$\{?WEBHOOK_URL\}?"' <<<"${script_body}"; then
  fail "the webhook URL is passed to curl as an argument"
fi
grep -Fq -- '--config -' <<<"${script_body}" ||
  fail "the delivery does not read its URL from a stdin config"
pass "webhook URL is never passed to curl in argv"

# Least privilege: exactly get/list on providers.pkg.crossplane.io.
[ "$(yq eval -o=json -I=0 '.rules' "${cluster_role}")" = '[{"apiGroups":["pkg.crossplane.io"],"resources":["providers"],"verbs":["get","list"]}]' ] ||
  fail "the ClusterRole must grant only get/list on providers.pkg.crossplane.io"
pass "the ClusterRole is read-only on Providers alone"

# ---------------------------------------------------------------------------
# Behavioural scenarios.
# ---------------------------------------------------------------------------

# Under /tmp so the /tmp/ rewrite below reproduces the Linux CI condition.
work_root="$(mktemp -d /tmp/tmp.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT

# The stub tells the API read (-o <file>) from the delivery, as the CNPG test does.
setup_scenario() {
  local name="$1" api_code="$2" api_body="$3" webhook="$4"
  local dir="${work_root}/${name}"

  mkdir -p "${dir}/bin" "${dir}/sa" "${dir}/webhook" "${dir}/tmp"
  printf 'fake-ca' >"${dir}/sa/ca.crt"
  printf 'fake-token' >"${dir}/sa/token"
  printf '%s' "${webhook}" >"${dir}/webhook/url"
  printf '%s' "${api_body}" >"${dir}/api-body.json"
  printf '%s' "${api_code}" >"${dir}/api-code"

  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
dir="${SCENARIO_DIR}"
printf '%s\n' "$*" >>"${dir}/curl-argv.log"
out_file=""
prev=""
for arg in "$@"; do
  [ "${prev}" = "-o" ] && out_file="${arg}"
  prev="${arg}"
done
if [ -n "${out_file}" ]; then
  printf '%s\n' "$*" >"${dir}/api-request.log"
  cat "${dir}/api-body.json" >"${out_file}"
  printf '%s' "$(cat "${dir}/api-code")"
  exit 0
fi
cat >>"${dir}/curl-stdin.log"
for arg in "$@"; do
  case "${arg}" in
    @*) cp "${arg#@}" "${dir}/delivered-payload.json" 2>/dev/null || true ;;
  esac
done
printf 'delivered\n' >>"${dir}/deliveries.log"
exit "${STUB_DELIVERY_EXIT:-0}"
STUB
  chmod +x "${dir}/bin/curl"
  printf '%s' "${dir}"
}

# Redirect only the in-cluster paths; the `/tmp/` rewrite must run first (see
# the CNPG test for the doubled-path bug that ordering prevents).
run_scenario() {
  local dir="$1"
  local patched="${dir}/script.sh"

  {
    printf '%s\n' 'set -eu'
    printf 'GRACE_SECONDS=%s\n' 900
    printf 'SA_OVERRIDE=%q\n' "${dir}/sa"
    # shellcheck disable=SC2016  # `$SA_OVERRIDE` is emitted INTO the patched script.
    printf '%s\n' "${script_body}" |
      sed -e 's#^\( *\)SA=/var/run/secrets/kubernetes.io/serviceaccount$#\1SA="$SA_OVERRIDE"#' \
        -e 's#/tmp/#'"${dir}"'/tmp/#g' \
        -e 's#'"${mount_path}"'/url#'"${dir}"'/webhook/url#g'
  } >"${patched}"

  # shellcheck disable=SC2016  # matching the LITERAL `$SA_OVERRIDE`.
  grep -q 'SA="\$SA_OVERRIDE"' "${patched}" ||
    fail "serviceaccount path redirection did not apply"
  grep -q "${dir}/webhook/url" "${patched}" ||
    fail "webhook path redirection did not apply"
  ! grep -q -- "${dir}.*${dir}" "${patched}" ||
    fail "a redirection rewrote another rule's output"

  (
    cd "${dir}"
    PATH="${dir}/bin:${PATH}" SCENARIO_DIR="${dir}" bash "${patched}" >"${dir}/stdout.log" 2>"${dir}/stderr.log"
  )
}

deliveries() {
  local dir="$1"
  [ -f "${dir}/deliveries.log" ] && wc -l <"${dir}/deliveries.log" | tr -d ' ' || printf '0'
}

expect_alert() {
  local name="$1" body="$2" pattern="$3"
  local dir
  dir="$(setup_scenario "${name}" 200 "${body}" "${REAL_WEBHOOK}")"
  run_scenario "${dir}" || fail "scenario '${name}': script exited non-zero: $(cat "${dir}/stderr.log")"
  [ "$(deliveries "${dir}")" = "1" ] ||
    fail "scenario '${name}': expected exactly 1 delivery, got $(deliveries "${dir}")"
  jq -e --arg p "${pattern}" '.text | test($p)' "${dir}/delivered-payload.json" >/dev/null ||
    fail "scenario '${name}': payload does not match /${pattern}/"
  printf '%s' "${dir}"
}

expect_quiet() {
  local name="$1" body="$2"
  local dir
  dir="$(setup_scenario "${name}" 200 "${body}" "${REAL_WEBHOOK}")"
  run_scenario "${dir}" || fail "scenario '${name}': script exited non-zero: $(cat "${dir}/stderr.log")"
  [ "$(deliveries "${dir}")" = "0" ] || fail "scenario '${name}': expected no delivery"
  printf '%s' "${dir}"
}

expect_failure() {
  local name="$1" code="$2" body="$3" webhook="$4" stderr_pattern="$5"
  local dir
  dir="$(setup_scenario "${name}" "${code}" "${body}" "${webhook}")"
  if run_scenario "${dir}"; then
    fail "scenario '${name}': expected the Job to fail"
  fi
  [ "$(deliveries "${dir}")" = "0" ] || fail "scenario '${name}': delivered despite failing"
  grep -q "${stderr_pattern}" "${dir}/stderr.log" ||
    fail "scenario '${name}': stderr does not report '${stderr_pattern}'"
}

old_stamp="$(date -u -r 0 +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d @0 +%Y-%m-%dT%H:%M:%SZ)"
now_stamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
readonly old_stamp now_stamp
readonly incident_message='cannot initialize dependency graph from the packages in the lock: node xpkg.upbound.io/upbound/provider-family-aws already exists'

# provider <name> <Healthy status|""> <Healthy lastTransitionTime> <creationTimestamp>
# An empty status omits the Healthy condition entirely.
provider() {
  jq -n --arg n "$1" --arg s "$2" --arg t "$3" --arg c "$4" --arg m "${incident_message}" '{
    apiVersion: "pkg.crossplane.io/v1",
    kind: "Provider",
    metadata: { name: $n, creationTimestamp: $c },
    spec: { package: ("xpkg.upbound.io/upbound/" + $n + ":v2.0.0") },
    status: {
      conditions: (
        [{ type: "Installed", status: "True", reason: "ActivePackageRevision", lastTransitionTime: $c }]
        + (if $s == "" then []
           else [{ type: "Healthy", status: $s,
                   reason: (if $s == "True" then "HealthyPackageRevision" else "UnhealthyPackageRevision" end),
                   lastTransitionTime: $t }
                 + (if $s == "True" then {} else { message: $m } end)]
           end)
      )
    }
  }'
}

fleet() {
  jq -s '{ apiVersion: "pkg.crossplane.io/v1", kind: "ProviderList", items: . }'
}

healthy_fleet="$(
  {
    provider provider-family-aws True "${old_stamp}" "${old_stamp}"
    provider provider-aws-iam True "${old_stamp}" "${old_stamp}"
  } | fleet
)"
incident_fleet="$(
  {
    provider provider-family-aws False "${old_stamp}" "${old_stamp}"
    provider provider-aws-iam False "${old_stamp}" "${old_stamp}"
  } | fleet
)"
readonly healthy_fleet incident_fleet

# 1 — the #3454 shape: every Provider Healthy=False past the grace. One delivery
# naming each Provider and its message; the URL travels on stdin, not argv.
dir="$(expect_alert incident "${incident_fleet}" 'provider-family-aws. — Healthy=False since')"
jq -e '.text | test("provider-aws-iam") and test("already exists")' "${dir}/delivered-payload.json" >/dev/null ||
  fail "scenario 'incident': payload does not name every unhealthy Provider with its message"
grep -Fq "/apis/pkg.crossplane.io/v1/providers" "${dir}/api-request.log" ||
  fail "scenario 'incident': the script did not list providers.pkg.crossplane.io/v1"
grep -Fq "${REAL_WEBHOOK}" "${dir}/curl-stdin.log" ||
  fail "scenario 'incident': the webhook URL never reached curl's stdin config"
if grep -Fq "${REAL_WEBHOOK}" "${dir}/curl-argv.log"; then
  fail "scenario 'incident': the webhook URL appeared in curl's argv"
fi
pass "an unhealthy-past-grace fleet delivers one alert naming every Provider"

# 2 — Unknown is not healthy.
expect_alert unknown "$(provider provider-upjet-unifi Unknown "${old_stamp}" "${old_stamp}" | fleet)" \
  'provider-upjet-unifi. — Healthy=Unknown' >/dev/null
pass "Healthy=Unknown past the grace alerts"

# 3 — a Provider that never reports Healthy alerts once its creation is past the
# grace, and stays quiet while it is new.
expect_alert missing_old "$(provider provider-upjet-unifi "" "" "${old_stamp}" | fleet)" \
  'provider-upjet-unifi. — Healthy=missing since' >/dev/null
expect_quiet missing_new "$(provider provider-upjet-unifi "" "" "${now_stamp}" | fleet)" >/dev/null
pass "a missing Healthy condition is timed from creationTimestamp"

# 4 — GUARDRAIL: mid-upgrade. The new revision is Installed but not yet Healthy,
# and the transition is recent (about a minute on the 2026-10-17 upgrade).
dir="$(expect_quiet mid_upgrade "$(
  {
    provider provider-family-aws True "${old_stamp}" "${old_stamp}"
    provider provider-upjet-unifi False "${now_stamp}" "${old_stamp}"
  } | fleet
)")"
grep -q 'none unhealthy beyond 900s' "${dir}/stdout.log" ||
  fail "scenario 'mid_upgrade': the quiet verdict was not reported"
pass "a Provider unhealthy only inside the grace (an upgrade) does not page"

# 5 — a healthy fleet is quiet and reports what it checked.
dir="$(expect_quiet healthy "${healthy_fleet}")"
grep -q 'Checked 2 Crossplane provider' "${dir}/stdout.log" ||
  fail "scenario 'healthy': the checked count was not reported"
pass "a healthy fleet is quiet and reports what it checked"

# 6 — the reserved placeholder host stays inert.
dir="$(setup_scenario placeholder 200 "${incident_fleet}" "${PLACEHOLDER_WEBHOOK}")"
run_scenario "${dir}" || fail "scenario 'placeholder': script exited non-zero"
[ "$(deliveries "${dir}")" = "0" ] || fail "scenario 'placeholder': delivered to example.invalid"
grep -q 'No Slack webhook configured' "${dir}/stdout.log" ||
  fail "scenario 'placeholder': the skip was not reported"
pass "the example.invalid placeholder stays inert"

# 7 — a cluster without Crossplane is a supported state.
dir="$(setup_scenario no_crd 404 '{}' "${REAL_WEBHOOK}")"
run_scenario "${dir}" || fail "scenario 'no_crd': a 404 should exit 0"
[ "$(deliveries "${dir}")" = "0" ] || fail "scenario 'no_crd': alerted with no Provider CRD"
pass "a cluster without the Provider CRD exits cleanly"

# 8 — SILENT-ZERO guards: each must fail the Job, never read as healthy.
expect_failure api_error 500 '{}' "${REAL_WEBHOOK}" 'kube API returned HTTP 500'
pass "a failing kube API read fails the Job"
expect_failure bad_shape 200 '{"unexpected":true}' "${REAL_WEBHOOK}" 'unexpected response shape'
expect_failure items_not_array 200 '{"items":{}}' "${REAL_WEBHOOK}" 'unexpected response shape'
pass "a malformed API response, including a non-array items, fails the Job"

# 9 — webhook guards: a control character or a non-https URL fails before delivery.
expect_failure url_control_chars 200 '{"items":[]}' "${REAL_WEBHOOK}
output = ${work_root}/injected-output" 'control characters'
[ ! -e "${work_root}/injected-output" ] || fail "scenario 'url_control_chars': injected directive took effect"
expect_failure plain_http 200 "${incident_fleet}" 'http://hooks.test.invalid:443/delivery-target' 'must use https://'
pass "a control character or non-https webhook URL fails before any delivery"

# 10 — a failed delivery fails the Job.
dir="$(setup_scenario delivery_fails 200 "${incident_fleet}" "${REAL_WEBHOOK}")"
if STUB_DELIVERY_EXIT=22 run_scenario "${dir}"; then
  fail "scenario 'delivery_fails': a failed webhook POST was reported as success"
fi
pass "a failed webhook delivery fails the Job"

# 11 — the full API response shape. provider()/fleet() build only the fields the
# script reads, so they cannot show that the extra fields a real response carries
# leave the verdict alone. This fixture has exactly the structure of a
# `GET /apis/pkg.crossplane.io/v1/providers` body read from prod on 2026-10-17
# (Crossplane v2.4.2, metadata.managedFields aside): every field path that
# response carried, including the list metadata and each Provider's spec and
# status extras. The values are invented; production data is not committed.
readonly shape_fleet_file="${root_dir}/scripts/tests/fixtures/crossplane-provider-list/provider-list-api-shape.json"
[ -f "${shape_fleet_file}" ] || fail "ProviderList shape fixture not found: ${shape_fleet_file}"
shape_fleet="$(cat "${shape_fleet_file}")"
jq -e '.kind == "ProviderList" and (.items | length) == 4
  and all(.items[]; any(.status.conditions[]; .type == "Healthy" and .status == "True"))' \
  <<<"${shape_fleet}" >/dev/null ||
  fail "the shape fixture is not the four-Provider healthy ProviderList this scenario asserts on"
dir="$(expect_quiet shape_healthy "${shape_fleet}")"
grep -q 'Checked 4 Crossplane provider' "${dir}/stdout.log" ||
  fail "scenario 'shape_healthy': the script did not count the four Providers"
grep -q 'none unhealthy beyond 900s' "${dir}/stdout.log" ||
  fail "scenario 'shape_healthy': the quiet verdict was not reported"
pass "a full-shape ProviderList reads as four healthy Providers"

# The same objects with one Healthy condition flipped, keeping its
# lastTransitionTime (already past the grace) and every other field.
dir="$(expect_alert shape_one_unhealthy "$(
  jq --arg m "${incident_message}" '(.items[] | select(.metadata.name == "upbound-provider-family-aws")
      | .status.conditions[] | select(.type == "Healthy"))
    |= (.status = "False" | .reason = "UnhealthyPackageRevision" | .message = $m)' <<<"${shape_fleet}"
)" 'upbound-provider-family-aws. — Healthy=False since')"
if jq -e '.text | test("provider-aws-iam|provider-upjet-github|provider-upjet-unifi")' "${dir}/delivered-payload.json" >/dev/null; then
  fail "scenario 'shape_one_unhealthy': a healthy Provider was named in the alert"
fi
pass "one unhealthy Provider in the full shape alerts and names only that Provider"

# ---------------------------------------------------------------------------
# Wiring.
# ---------------------------------------------------------------------------

grep -Fq "'scripts/tests/test-crossplane-provider-unhealthy-alert.sh'" "${ci_workflow}" ||
  fail "this test is not in the CI paths filter"
grep -Fq "bash scripts/tests/test-crossplane-provider-unhealthy-alert.sh" "${ci_workflow}" ||
  fail "this test is never executed by a CI step"
for f in service-account secret cluster-role cluster-role-binding cron-job; do
  grep -Fq -- "- ${f}-crossplane-provider-unhealthy-alert.yaml" "${coroot_dir}/kustomization.yaml" ||
    fail "${f}-crossplane-provider-unhealthy-alert.yaml is not in the coroot kustomization"
done
pass "the contract is wired into CI and every resource is deployed"

printf '\nAll crossplane-provider-unhealthy-alert contract assertions passed.\n'
