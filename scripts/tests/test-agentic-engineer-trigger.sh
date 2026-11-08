#!/usr/bin/env bash
# Prove the Agentic Engineer lane's trigger enforces its per-run spend cap.
#
# The trigger's run limit is the lane's per-run spend cap: kagent and Substrate have no task
# deadline of their own, so the trigger cancels the task at the limit. A cancel that fails or is
# ignored must fail the Job rather than report a capped run (devantler-tech/platform#5905). This
# runs the ConfigMap's real script under `sh`, against a stub `curl` that plays kagent's A2A API.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
manifest="${root}/k8s/providers/hetzner/apps/agentic-engineer/config-map-trigger.yaml"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer trigger: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer trigger: %s\n' "$1" >&2
  exit 1
}
for tool in yq jq sh; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

yq '.data["trigger.sh"]' "${manifest}" >"${work}/trigger.sh"
yq '.data["message.txt"]' "${manifest}" >"${work}/message.txt"
grep -q 'method: "CancelTask"' "${work}/trigger.sh" || fail 'the trigger no longer cancels the task'

# The stub answers by JSON-RPC method. GetTask replays one state per call from the scenario's
# `states` file (the last line repeats); after a CancelTask, `states-after-cancel` takes over when
# present. CancelTask either answers, returns a JSON-RPC error, or fails at the HTTP layer.
mkdir -p "${work}/bin"
cat >"${work}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${STUB_DIR:?}"
method="$(jq -r '.method')"
printf '%s\n' "${method}" >>"${dir}/calls"
next_state() {
  local file="$1" count
  count="$(grep -c '^GetTask$' "${dir}/calls")"
  sed -n "${count}p" "${file}" | grep . || tail -n 1 "${file}"
}
case "${method}" in
  SendMessage) printf '{"jsonrpc":"2.0","id":"run","result":{"task":{"id":"task-1"}}}' ;;
  CancelTask)
    case "$(cat "${dir}/cancel")" in
      ok) printf '{"jsonrpc":"2.0","id":"cancel","result":{"id":"task-1"}}'; touch "${dir}/cancelled" ;;
      rpc-error) printf '{"jsonrpc":"2.0","id":"cancel","error":{"code":-32002,"message":"not cancelable"}}' ;;
      http-error) printf 'curl: (22) The requested URL returned error: 503\n' >&2; exit 22 ;;
    esac ;;
  GetTask)
    file="${dir}/states"
    [[ -f "${dir}/cancelled" && -f "${dir}/states-after-cancel" ]] && file="${dir}/states-after-cancel"
    state="$(next_state "${file}")"
    if [[ "${state}" == garbage ]]; then
      printf '<html>bad gateway</html>'
    else
      jq -cn --arg state "${state}" '{jsonrpc: "2.0", id: "get", result: {id: "task-1",
        status: {state: $state, message: {parts: [{text: "run report"}]}}}}'
    fi ;;
  *) printf 'unexpected method %s\n' "${method}" >&2; exit 1 ;;
esac
STUB
chmod +x "${work}/bin/curl"

# run <scenario> <cancel behaviour> <run limit> <states> [states after cancel]: prints the exit
# code. States are space-separated, one per GetTask call.
run() {
  local dir="${work}/$1" status=0 states=() after=()
  mkdir -p "${dir}"
  : >"${dir}/calls"
  printf '%s\n' "$2" >"${dir}/cancel"
  read -ra states <<<"$4"
  printf '%s\n' "${states[@]}" >"${dir}/states"
  if [[ -n "${5:-}" ]]; then
    read -ra after <<<"$5"
    printf '%s\n' "${after[@]}" >"${dir}/states-after-cancel"
  fi
  PATH="${work}/bin:${PATH}" STUB_DIR="${dir}" RUN_LIMIT_SECONDS="$3" POLL_SECONDS=1 \
    MESSAGE_FILE="${work}/message.txt" sh "${work}/trigger.sh" >"${dir}/out" 2>&1 || status=$?
  printf '%s' "${status}"
}
# Count the CancelTask calls a scenario made.
cancels() { grep -c '^CancelTask$' "${work}/$1/calls" || true; }

# A run that finishes inside the limit succeeds without a cancel, even past an unreadable reply.
[[ "$(run finished ok 10 'garbage TASK_STATE_COMPLETED')" == 0 ]] ||
  fail "a completed run did not succeed: $(cat "${work}/finished/out")"
[[ "$(cancels finished)" == 0 ]] || fail 'a completed run was cancelled'
grep -q 'final state: TASK_STATE_COMPLETED' "${work}/finished/out" || fail 'a completed run did not report its state'

# At the limit, a cancel the task honours ends the run as capped.
[[ "$(run capped ok 2 TASK_STATE_WORKING TASK_STATE_CANCELED)" == 1 ]] ||
  fail "a capped run did not fail as capped: $(cat "${work}/capped/out")"
grep -q 'final state: CANCELED_AT_LIMIT' "${work}/capped/out" || fail 'a capped run did not report CANCELED_AT_LIMIT'

# A cancel the task ignores, or that never reaches kagent, must not read as capped.
for behaviour in rpc-error http-error; do
  [[ "$(run "ignored-${behaviour}" "${behaviour}" 2 TASK_STATE_WORKING)" == 2 ]] ||
    fail "an unconfirmed cancel (${behaviour}) did not exit 2: $(cat "${work}/ignored-${behaviour}/out")"
  [[ "$(cancels "ignored-${behaviour}")" == 3 ]] || fail "an unconfirmed cancel (${behaviour}) was not retried three times"
  grep -q 'cancellation not confirmed after 3 attempts' "${work}/ignored-${behaviour}/out" ||
    fail "an unconfirmed cancel (${behaviour}) did not say so"
  if grep -q 'CANCELED_AT_LIMIT' "${work}/ignored-${behaviour}/out"; then
    fail "an unconfirmed cancel (${behaviour}) reported a capped run"
  fi
done

grep -Fq 'bash scripts/tests/test-agentic-engineer-trigger.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer trigger: PASS\n'
