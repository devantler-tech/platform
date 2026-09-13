#!/usr/bin/env bash
# Pins scripts/probe-cilium-externalauth-datapath.sh and the default-off
# externalauth-probe component (platform#3784). Needs no cluster, no network and
# no secrets: curl is faked from a fixture list of status codes.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/probe-cilium-externalauth-datapath.sh"
readonly component="${root_dir}/k8s/providers/hetzner/apps/whoami/components/externalauth-probe"
readonly apps_kustomization="${root_dir}/k8s/providers/hetzner/apps/kustomization.yaml"
readonly workflow="${root_dir}/.github/workflows/probe-cilium-externalauth-datapath.yaml"

work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

readonly host='whoami-externalauth-probe.example.test'
cases_run=0

fail() {
  printf '\nFAIL: %s\n' "$1" >&2
  exit 1
}

check() {
  cases_run=$((cases_run + 1))
  printf '  ok  %s\n' "$1"
}

# Fake curl: prints the next status code from codes.txt and records its argv.
cat >"${work_dir}/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
dir="${FAKE_DIR}"
printf '%s\n' "$*" >>"${dir}/calls.log"
n=$(($(cat "${dir}/counter" 2>/dev/null || printf 0) + 1))
printf '%s' "${n}" >"${dir}/counter"
code="$(sed -n "${n}p" "${dir}/codes.txt")"
printf '%s' "${code:-000}"
[[ "${code}" == "000" ]] && exit 28
exit 0
FAKE
chmod +x "${work_dir}/curl"
export FAKE_DIR="${work_dir}"
export CURL="${work_dir}/curl"

run_probe() {
  local -a codes
  read -r -a codes <<<"$1"
  shift
  rm -f "${work_dir}/counter" "${work_dir}/calls.log"
  printf '%s\n' "${codes[@]}" >"${work_dir}/codes.txt"
  set +e
  probe_out="$(GITHUB_STEP_SUMMARY='' "${script}" "$@" 2>&1)"
  probe_rc=$?
  set -e
}

require_text() {
  grep -qF -- "$2" <<<"$1" || fail "$3: expected output to contain '$2'. Got: $1"
}

refute_text() {
  if grep -qF -- "$2" <<<"$1"; then
    fail "$3: expected output NOT to contain '$2'. Got: $1"
  fi
}

printf 'test-probe-cilium-externalauth-datapath\n'

# --- Verdicts ----------------------------------------------------------------
run_probe '302 302 401 303 302' --host "${host}" --requests 5
[[ ${probe_rc} -eq 0 ]] || fail "all delivered should exit 0, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'VERDICT: DELIVERED' 'all delivered'
check 'every request answered by oauth2-proxy is DELIVERED (exit 0)'

# Control for the case above: one lost request in the same run flips the verdict.
run_probe '302 302 000 303 302' --host "${host}" --requests 5
[[ ${probe_rc} -eq 1 ]] || fail "one timeout among deliveries should exit 1, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'VERDICT: SUBREQUESTS-LOST' 'one timeout'
require_text "${probe_out}" 'timeout: 1' 'one timeout'
check 'a single timeout among delivered requests is SUBREQUESTS-LOST (exit 1)'

run_probe '302 503 302 403' --host "${host}" --requests 4
[[ ${probe_rc} -eq 1 ]] || fail "5xx and 403 among deliveries should exit 1, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'lost: 2 (403: 1, 5xx: 1, timeout: 0)' '5xx and 403'
check '403 and 5xx responses count as lost subrequests'

run_probe '000 000 000' --host "${host}" --requests 3
[[ ${probe_rc} -eq 3 ]] || fail "nothing delivered should exit 3, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'VERDICT: INCONCLUSIVE' 'nothing delivered'
check 'no request delivered at all is INCONCLUSIVE, not SUBREQUESTS-LOST'

run_probe '302 200 302' --host "${host}" --requests 3
[[ ${probe_rc} -eq 3 ]] || fail "an unfiltered 200 should exit 3, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'filter is not applied' 'unfiltered 200'
refute_text "${probe_out}" 'VERDICT: DELIVERED' 'unfiltered 200'
check 'a 200 means the filter is not applied and is INCONCLUSIVE, never DELIVERED'

run_probe '404 404' --host "${host}" --requests 2
[[ ${probe_rc} -eq 3 ]] || fail "404s should exit 3, got ${probe_rc}: ${probe_out}"
require_text "${probe_out}" 'not serving the probe' '404'
check 'a 404 (component not referenced) is INCONCLUSIVE'

# --- Request shape --------------------------------------------------------------
run_probe '302 302' --host "${host}" --requests 2 --timeout 7
[[ "$(grep -c . "${work_dir}/calls.log")" -eq 2 ]] || fail 'the probe must send exactly --requests requests'
grep -qF -- "--proto =https --max-time 7 https://${host}/" "${work_dir}/calls.log" ||
  fail "curl must be HTTPS-only, timeout-bounded and aimed at the probe host. Calls: $(cat "${work_dir}/calls.log")"
check 'requests are HTTPS-only, bounded by --timeout, and exactly --requests in number'

# --- Input validation -------------------------------------------------------------
for bad_host in 'https://evil.example' 'evil.example/path' 'evil.example:443' 'UPPER.example' 'x;id' 'nodot' ''; do
  run_probe '302' --host "${bad_host}" --requests 1
  [[ ${probe_rc} -eq 2 ]] || fail "host '${bad_host}' should be a usage error, got ${probe_rc}"
  [[ ! -e "${work_dir}/calls.log" ]] || fail "host '${bad_host}' reached curl"
done
check 'a host that is not a plain lowercase DNS name is refused before any request'

for bad_requests in 0 1001 abc -1; do
  run_probe '302' --host "${host}" --requests "${bad_requests}"
  [[ ${probe_rc} -eq 2 ]] || fail "--requests ${bad_requests} should be a usage error, got ${probe_rc}"
done
check '--requests outside 1..1000 is refused'

# --- The component stays default-off and keeps the known-good filter shape -------
[[ -f "${component}/kustomization.yaml" ]] || fail 'the externalauth-probe component is missing'
[[ "$(yq '.kind' "${component}/kustomization.yaml")" == 'Component' ]] || fail 'externalauth-probe must be a Kustomize Component'
if yq '.components // [] | .[]' "${apps_kustomization}" | grep -qF 'whoami/components/externalauth-probe'; then
  fail 'the externalauth-probe component must not be referenced on merge; enable it only in a short-lived PR'
fi
check 'the probe component is not referenced by the hetzner apps overlay'

route="${component}/http-route.yaml"
[[ "$(yq '.spec.rules[0].filters[] | select(.type == "ExternalAuth") | .externalAuth.backendRef.name' "${route}")" == 'oauth2-proxy' ]] ||
  fail 'the probe route must send ExternalAuth to oauth2-proxy'
[[ "$(yq '.spec.rules[0].filters[] | select(.type == "ExternalAuth") | .externalAuth.http.allowedResponseHeaders | length' "${route}")" -gt 0 ]] ||
  fail 'allowedResponseHeaders must be non-empty (an empty list hung the allow path in #1881)'
# shellcheck disable=SC2016 # `${domain}` is a literal Flux substitution variable, not a shell expansion.
[[ "$(yq '.spec.hostnames[0]' "${route}")" == 'whoami-externalauth-probe.${domain}' ]] ||
  fail 'the probe route must use its own hostname, never a user-facing one'
check 'the probe route targets oauth2-proxy on its own hostname with non-empty allowedResponseHeaders'

# --- The workflow stays dormant and credential-free --------------------------------
[[ "$(yq '.on | keys | join(",")' "${workflow}")" == 'workflow_dispatch' ]] || fail 'the probe workflow must be dispatch-only'
if grep -qE 'secrets\.' "${workflow}"; then
  fail 'the probe workflow must not read any secret'
fi
grep -qF 'probe-production-externalauth-datapath' "${workflow}" || fail 'the probe workflow must require its confirmation phrase'
check 'the workflow is dispatch-only, reads no secret and requires confirmation'

printf '\nAll %d case(s) passed.\n' "${cases_run}"
