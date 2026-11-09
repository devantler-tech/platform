#!/usr/bin/env bash
# Prove the Agentic Engineer lane's trigger starts the daily run and watches it to its end.
#
# A run has no time limit: cutting one off mid-task wastes what it has already spent, so the
# Anthropic workspace's monthly limit is the lane's only spend cap (maintainer decision,
# 2026-11-09). The trigger therefore never cancels the task, retries a failed or unreadable poll,
# resends a lost opening message with the same message ID, and fails the Job when it never learns
# of the run or the run does not complete. This runs the ConfigMap's real script under `sh`,
# against a stub `curl` that plays kagent's A2A API.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
lane="${root}/k8s/providers/hetzner/apps/agentic-engineer"
manifest="${lane}/config-map-trigger.yaml"
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
[[ -s "${work}/trigger.sh" ]] || fail 'the ConfigMap has no trigger.sh'
# The trigger must not cut a run off: no cancel, and no run limit for the CronJob to set.
if grep -q 'CancelTask' "${work}/trigger.sh"; then
  fail 'the trigger cancels the task; a run has no time limit'
fi
if grep -q 'RUN_LIMIT_SECONDS' "${work}/trigger.sh" "${lane}/cron-job.yaml"; then
  fail 'a run limit is still configured; a run has no time limit'
fi

# The stub answers by JSON-RPC method. GetTask replays one state per call from the scenario's
# `states` file (the last line repeats): a task state, `garbage` for an unreadable reply, or
# `http-error` for a failed request. A completed task's report arrives as two streamed parts.
mkdir -p "${work}/bin"
cat >"${work}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${STUB_DIR:?}"
body="$(cat)"
method="$(jq -r '.method' <<<"${body}")"
printf '%s\n' "${method}" >>"${dir}/calls"
if [[ "${method}" == SendMessage ]]; then
  jq -r '.params.message.messageId' <<<"${body}" >>"${dir}/message-ids"
fi
case "${method}" in
  SendMessage)
    behaviour=ok
    if [[ -f "${dir}/send" ]]; then
      count="$(grep -c '^SendMessage$' "${dir}/calls")"
      behaviour="$(sed -n "${count}p" "${dir}/send" | grep . || tail -n 1 "${dir}/send")"
    fi
    case "${behaviour}" in
      ok) printf '{"jsonrpc":"2.0","id":"run","result":{"task":{"id":"task-1"}}}' ;;
      garbage) printf '<html>bad gateway</html>' ;;
      http-error) printf 'curl: (52) Empty reply from server\n' >&2; exit 52 ;;
    esac ;;
  GetTask)
    count="$(grep -c '^GetTask$' "${dir}/calls")"
    state="$(sed -n "${count}p" "${dir}/states" | grep . || tail -n 1 "${dir}/states")"
    case "${state}" in
      garbage) printf '<html>bad gateway</html>' ;;
      http-error) printf 'curl: (22) The requested URL returned error: 503\n' >&2; exit 22 ;;
      *)
        jq -cn --arg state "${state}" '{jsonrpc: "2.0", id: "get", result: {id: "task-1",
          status: {state: $state, message: {parts: [{text: "run "}, {text: "report"}]}}}}' ;;
    esac ;;
  *) printf 'unexpected method %s\n' "${method}" >&2; exit 1 ;;
esac
STUB
chmod +x "${work}/bin/curl"

# run <scenario> <states>: prints the exit code. States are space-separated, one per GetTask call.
# SEND="<behaviour>..." scripts SendMessage replies (ok, garbage, http-error; the last repeats).
run() {
  local dir="${work}/$1" status=0 states=()
  mkdir -p "${dir}"
  : >"${dir}/calls"
  if [[ -n "${SEND:-}" ]]; then
    local sends=()
    read -ra sends <<<"${SEND}"
    printf '%s\n' "${sends[@]}" >"${dir}/send"
  fi
  read -ra states <<<"$2"
  printf '%s\n' "${states[@]}" >"${dir}/states"
  PATH="${work}/bin:${PATH}" STUB_DIR="${dir}" POLL_SECONDS=0 \
    MESSAGE_FILE="${work}/message.txt" sh "${work}/trigger.sh" >"${dir}/out" 2>&1 || status=$?
  printf '%s' "${status}"
}
# Count the calls of one method a scenario made.
calls() { grep -c "^$2\$" "${work}/$1/calls" || true; }

# A run that completes succeeds, past an unreadable reply, and its report prints whole.
[[ "$(run finished 'garbage TASK_STATE_COMPLETED')" == 0 ]] ||
  fail "a completed run did not succeed: $(cat "${work}/finished/out")"
grep -q 'final state: TASK_STATE_COMPLETED' "${work}/finished/out" || fail 'a completed run did not report its state'
grep -qx 'run report' "${work}/finished/out" || fail "a streamed report did not print whole: $(cat "${work}/finished/out")"

# A long run is watched until it ends, across every poll, and never cancelled.
long='TASK_STATE_WORKING TASK_STATE_WORKING TASK_STATE_WORKING TASK_STATE_WORKING TASK_STATE_WORKING'
long+=' TASK_STATE_WORKING TASK_STATE_WORKING TASK_STATE_WORKING TASK_STATE_COMPLETED'
[[ "$(run long "${long}")" == 0 ]] || fail "a long run was not watched to its end: $(cat "${work}/long/out")"
[[ "$(calls long GetTask)" == 9 ]] || fail "a long run was polled $(calls long GetTask) times instead of 9"

# A failed or unreadable poll is retried, never a reason to stop watching.
[[ "$(run flaky 'http-error garbage http-error TASK_STATE_WORKING TASK_STATE_COMPLETED')" == 0 ]] ||
  fail "a run with failed polls was not watched to its end: $(cat "${work}/flaky/out")"

# A run that ends in any other way fails the Job, with the state it ended in.
[[ "$(run failed 'TASK_STATE_WORKING TASK_STATE_FAILED')" == 1 ]] ||
  fail "a failed run did not fail the Job: $(cat "${work}/failed/out")"
grep -q 'final state: TASK_STATE_FAILED' "${work}/failed/out" || fail 'a failed run did not report its state'

# A send whose reply is lost is resent with the same message ID, which kagent answers with the
# task the first send started, so the run is watched as usual.
[[ "$(SEND='http-error garbage ok' run resent 'TASK_STATE_WORKING TASK_STATE_COMPLETED')" == 0 ]] ||
  fail "a resent run was not watched to its end: $(cat "${work}/resent/out")"
[[ "$(calls resent SendMessage)" == 3 ]] || fail 'a lost send was not resent until it gave a task'
# kagent returns the first send's task only for the same message ID; a new ID would start a run.
[[ "$(sort -u "${work}/resent/message-ids" | grep -c '^run-[0-9]\{8\}$')" == 1 &&
  "$(sort -u "${work}/resent/message-ids" | wc -l | tr -d ' ')" == 1 ]] ||
  fail "a resend changed the message ID: $(tr '\n' ' ' <"${work}/resent/message-ids")"

# A run the trigger never learns of cannot read as anything but a failure.
[[ "$(SEND=garbage run unknown TASK_STATE_WORKING)" == 2 ]] ||
  fail "a send that never gave a task did not exit 2: $(cat "${work}/unknown/out")"
grep -q 'no task id for run-' "${work}/unknown/out" || fail 'a send that never gave a task did not say so'
[[ "$(calls unknown GetTask)" == 0 ]] || fail 'the trigger watched a task it never learned of'

# No scenario cancelled anything.
for scenario in finished long flaky failed resent unknown; do
  [[ "$(calls "${scenario}" CancelTask)" == 0 ]] || fail "the ${scenario} scenario cancelled the task"
done

grep -Fq 'bash scripts/tests/test-agentic-engineer-trigger.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer trigger: PASS\n'
