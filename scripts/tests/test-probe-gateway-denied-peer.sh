#!/usr/bin/env bash
# Pins scripts/probe-gateway-denied-peer.sh with a fake kubectl: each verdict against a one-fixture
# control, what the probe may write, that it cleans up, and that no address reaches the public log.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo}/scripts/probe-gateway-denied-peer.sh"
workflow="${repo}/.github/workflows/probe-gateway-denied-peer.yaml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

mkdir -p "${work}/bin"
cat >"${work}/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
# Records every call, then answers from the fixture directory named by FIXTURES.
printf '%s\n' "$*" >>"${FIXTURES}/calls"
args=" $* "
case "${args}" in
  *' delete '*)
    [[ -f "${FIXTURES}/delete-fails" ]] && exit 1
    exit 0
    ;;
  *' apply '*)
    cat >"${FIXTURES}/applied"
    [[ -f "${FIXTURES}/apply-fails" ]] && exit 1
    exit 0
    ;;
  *' get --raw '*)
    n=$(($(cat "${FIXTURES}/raw-count" 2>/dev/null || echo 0) + 1))
    printf '%s' "${n}" >"${FIXTURES}/raw-count"
    [[ -f "${FIXTURES}/metrics-${n}" ]] || exit 1
    cat "${FIXTURES}/metrics-${n}"
    exit 0
    ;;
  *' get service '*)
    printf '10.96.0.42'
    exit 0
    ;;
  *' get pods,ciliumnetworkpolicies '*)
    cat "${FIXTURES}/leftovers" 2>/dev/null || true
    exit 0
    ;;
  *' get pod gateway-peer-probe-'*)
    printf 'node-secret-name'
    exit 0
    ;;
  *' get pods -l k8s-app=cilium-envoy '*)
    printf 'cilium-envoy-abcde'
    exit 0
    ;;
  *' logs '*)
    cat "${FIXTURES}/logs"
    exit 0
    ;;
esac
printf 'unexpected kubectl call: %s\n' "$*" >&2
exit 9
FAKE
chmod +x "${work}/bin/kubectl"

# Writes a fixture set: the pod log for ATTEMPTS attempts with the given curl exit and HTTP code.
fixture() {
  local name="$1" exit_code="$2" http="$3" before="$4" after="$5" rise="$6" attempts="${7:-3}" i
  local dir="${work}/${name}"
  mkdir -p "${dir}"
  {
    printf 'PROBE control-before %s\nPROBE-READY\n' "${before}"
    i=0
    while ((i < attempts)); do
      i=$((i + 1))
      printf 'PROBE tls %s %s\n' "${exit_code}" "${http}"
    done
    printf 'PROBE control-after %s\nPROBE-DONE\n' "${after}"
  } >"${dir}/logs"
  printf '# TYPE envoy_listener_no_filter_chain_match counter\nenvoy_listener_no_filter_chain_match{envoy_listener_address="0.0.0.0_443"} 7\nother_metric 99\n' >"${dir}/metrics-1"
  printf '# TYPE envoy_listener_no_filter_chain_match counter\nenvoy_listener_no_filter_chain_match{envoy_listener_address="0.0.0.0_443"} %s\nother_metric 99\n' "$((7 + rise))" >"${dir}/metrics-2"
}

# Runs the probe against a fixture set; expects the exit code and the verdict line.
expect() {
  local name="$1" want_rc="$2" want_verdict="$3" rc=0
  local dir="${work}/${name}"
  PATH="${work}/bin:${PATH}" FIXTURES="${dir}" PROBE_POLL_SECONDS=0 GITHUB_STEP_SUMMARY="${dir}/summary" \
    bash "${script}" --context admin@prod --run-id 12345 --attempts 3 >"${dir}/out" 2>&1 || rc=$?
  [[ "${rc}" == "${want_rc}" ]] || fail "${name}: exit ${rc}, want ${want_rc}: $(tr '\n' '|' <"${dir}/out")"
  if [[ -n "${want_verdict}" ]]; then
    grep -q "^VERDICT: ${want_verdict}\$" "${dir}/out" || fail "${name}: no VERDICT ${want_verdict}: $(tr '\n' '|' <"${dir}/out")"
  fi
  if grep -qE '10\.96\.0\.42|node-secret-name|cilium-envoy-abcde' "${dir}/out" "${dir}/summary" 2>/dev/null; then
    fail "${name}: an address or node name reached the public log"
  fi
}

# The positive case, and one changed fixture for each thing it depends on.
fixture refused 35 000 301 301 3
expect refused 0 REFUSED
fixture reset 56 000 301 301 5
expect reset 0 REFUSED
fixture accepted 0 200 301 301 0
expect accepted 5 ACCEPTED
# A 502 is still an answer from the listener: the handshake finished.
fixture answered-error 0 502 301 301 3
expect answered-error 5 ACCEPTED
# Accepted wins over a failed control, because it is a finding whatever else happened.
fixture accepted-no-control 0 200 000 000 0
expect accepted-no-control 5 ACCEPTED
fixture no-control-before 35 000 000 301 3
expect no-control-before 3 INCONCLUSIVE
fixture no-control-after 35 000 301 000 3
expect no-control-after 3 INCONCLUSIVE
fixture timeouts 28 000 301 301 3
expect timeouts 3 INCONCLUSIVE
fixture connect-refused 7 000 301 301 3
expect connect-refused 3 INCONCLUSIVE
fixture low-rise 35 000 301 301 2
expect low-rise 3 INCONCLUSIVE
fixture short-log 35 000 301 301 3 2
expect short-log 3 INCONCLUSIVE
fixture no-counter 35 000 301 301 3
rm "${work}/no-counter/metrics-2"
expect no-counter 3 INCONCLUSIVE
# A metrics page without the series is not a zero: a reading of "no rise" must not come from it.
fixture series-missing 35 000 301 301 3
printf 'other_metric 99\n' >"${work}/series-missing/metrics-1"
expect series-missing 3 INCONCLUSIVE

fixture leftovers 35 000 301 301 3
printf 'pod/gateway-peer-probe-1\n' >"${work}/leftovers/leftovers"
expect leftovers 3 INCONCLUSIVE
grep -q ' apply ' "${work}/leftovers/calls" && fail 'leftovers: objects were created over leftovers'
grep -q ' delete ' "${work}/leftovers/calls" && fail 'leftovers: cleanup ran over objects this run did not create'

fixture not-admitted 35 000 301 301 3
touch "${work}/not-admitted/apply-fails"
expect not-admitted 3 INCONCLUSIVE
grep -q ' delete .*gateway-peer-probe=12345' "${work}/not-admitted/calls" || fail 'not-admitted: a partial apply was not cleaned up'

fixture cleanup-fails 35 000 301 301 3
touch "${work}/cleanup-fails/delete-fails"
expect cleanup-fails 4 ''
grep -q '^CLEANUP: FAILED' "${work}/cleanup-fails/out" || fail 'cleanup-fails: not reported'

# Usage errors read and write nothing.
for bad in '--run-id 012' '--run-id abc' '--run-id 12345 --attempts 0' '--run-id 12345 --attempts 31' '--run-id 12345 --attempts 08'; do
  dir="${work}/usage"
  rm -rf "${dir}"
  mkdir -p "${dir}"
  rc=0
  # shellcheck disable=SC2086
  PATH="${work}/bin:${PATH}" FIXTURES="${dir}" bash "${script}" --context admin@prod ${bad} >/dev/null 2>&1 || rc=$?
  [[ "${rc}" == 1 ]] || fail "usage '${bad}': exit ${rc}, want 1"
  [[ ! -e "${dir}/calls" ]] || fail "usage '${bad}': kubectl was called"
done

# What it may do to production: only apply and run-scoped delete, no exec, patch or edit.
calls="${work}/refused/calls"
if grep -vE ' (get|logs|apply|delete) ' "${calls}" | grep -q .; then
  fail "a verb other than get, logs, apply or delete: $(grep -vE ' (get|logs|apply|delete) ' "${calls}" | sed -n 1p)"
fi
grep -E ' delete ' "${calls}" | grep -qv 'platform.devantler.tech/gateway-peer-probe=12345' && fail 'a delete not scoped to the run id'
grep -q ' delete .*gateway-peer-probe=12345' "${calls}" || fail 'a finished run did not clean up'
kinds="$(awk '/^kind:/ { print $2 }' "${work}/refused/applied" | sort | tr '\n' ' ')"
[[ "${kinds}" == 'CiliumNetworkPolicy Pod ' ]] || fail "applied kinds changed: ${kinds}"
[[ "$(grep -c 'platform.devantler.tech/gateway-peer-probe: "12345"' "${work}/refused/applied")" -ge 2 ]] || fail 'an applied object is not labelled with the run id'
grep -q 'automountServiceAccountToken: false' "${work}/refused/applied" || fail 'the pod mounts a service account token'
grep -qE 'image: .*@sha256:[0-9a-f]{64}$' "${work}/refused/applied" || fail 'the pod image is not pinned by digest'

# The pod's curl must report a closed connection as its own exit code, and the attempt must leave
# certificate checks off so a finished handshake reads as accepted, whatever certificate came back.
pod_script="$(sed -n '/- |$/,/^      securityContext:/p' "${work}/refused/applied")"
grep -q 'curl -sk ' <<<"${pod_script}" || fail 'the TLS attempt no longer ignores the certificate'
# shellcheck disable=SC2016 # the literal text of the pod script
grep -q -- '--resolve "\$TLS_HOST:443:\$GATEWAY_IP"' <<<"${pod_script}" || fail 'the TLS attempt no longer dials the gateway Service directly'

# Run the pod's own script against a fake curl, because the verdicts above read what it prints: a
# fixture alone cannot show that the script really reports curl's exit code.
mkdir -p "${work}/pod/bin"
cat >"${work}/pod/bin/curl" <<'FAKE'
#!/usr/bin/env bash
case "$*" in
  *--resolve*)
    printf '000'
    exit 35
    ;;
esac
printf '301'
FAKE
printf '#!/usr/bin/env bash\n' >"${work}/pod/bin/sleep"
chmod +x "${work}/pod/bin/curl" "${work}/pod/bin/sleep"
sed -n '/^        - |$/,/^      securityContext:$/p' "${work}/refused/applied" | sed '1d;$d' | sed 's/^          //' >"${work}/pod/script.sh"
pod_out="$(PATH="${work}/pod/bin:${PATH}" ATTEMPTS=2 WARMUP_ATTEMPTS=1 TIMEOUT=1 SETTLE_SECONDS=0 GATEWAY_IP=192.0.2.1 TLS_HOST=example.test CONTROL_HOST=x.invalid sh "${work}/pod/script.sh")"
want_pod="$(printf 'PROBE control-before 301\nPROBE-READY\nPROBE tls 35 000\nPROBE tls 35 000\nPROBE control-after 301\nPROBE-DONE')"
[[ "${pod_out}" == "${want_pod}" ]] || fail "the pod script printed: $(tr '\n' '|' <<<"${pod_out}")"

# The workflow: dormant, main only, confirmed, serialised with deploys.
grep -qE '^  workflow_dispatch:$' "${workflow}" || fail 'workflow: not dispatch-only'
if grep -qE '^  (schedule|pull_request|push|merge_group):' "${workflow}"; then
  fail 'workflow: gained an automatic trigger'
fi
grep -q 'group: prod-deploy' "${workflow}" || fail 'workflow: not serialised with deploys'
grep -q 'refs/heads/main' "${workflow}" || fail 'workflow: no main-branch guard'
grep -q 'probe-production-gateway-denied-peer' "${workflow}" || fail 'workflow: no confirmation phrase'
grep -q 'gateway-receiver-binding verify --context admin@prod' "${workflow}" || fail 'workflow: the allowed path is not verified'

if ((failures > 0)); then
  printf '%s failure(s)\n' "${failures}" >&2
  exit 1
fi
printf 'OK: probe-gateway-denied-peer\n'
