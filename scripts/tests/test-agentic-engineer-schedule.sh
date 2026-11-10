#!/usr/bin/env bash
# Prove the Agentic Engineer lane's schedule Job keeps kagent's scheduled run equal to its manifest.
#
# kagent keeps scheduled runs in its database, so the Job writes the daily run through kagent's
# ScheduledRunService: it creates the schedule when it is missing, updates it when it differs,
# leaves an equal one alone, and fails rather than guess when the schedules it finds are
# ambiguous or a call fails. This runs the Job's real script under `sh`, against a stub `grpcurl`
# that plays the API, and checks the manifest's decisions: a run has no time limit (maintainer
# decision, 2026-11-09), Flux recreates the Job on every change, and kagent serves reflection.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
lane="${root}/k8s/providers/hetzner/apps/agentic-engineer"
job="${lane}/job-schedule.yaml"
release="${root}/k8s/bases/infrastructure/kagent/helm-release-control-plane.yaml"
work="$(mktemp -d)"
completed=0
# Remove the work directory on exit, and fail any exit that did not reach the final PASS.
cleanup() {
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer schedule: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer schedule: %s\n' "$1" >&2
  exit 1
}
for tool in yq jq sh; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

container='.spec.template.spec.containers[] | select(.name == "schedule")'
# env <name>: the literal value the manifest gives one environment variable.
env_value() { yq "${container} | .env[] | select(.name == \"$1\") | .value" "${job}"; }

yq "${container} | .args[0]" "${job}" >"${work}/schedule.sh"
[[ -s "${work}/schedule.sh" ]] || fail 'the Job has no schedule script'

# The manifest's decisions.
[[ -z "$(find "${lane}" -name '*.yaml' -exec grep -l '^kind: CronJob$' {} +)" ]] ||
  fail 'the lane still has a CronJob; kagent schedules the run'
[[ "$(yq '.metadata.annotations["kustomize.toolkit.fluxcd.io/force"]' "${job}")" == enabled ]] ||
  fail 'Flux cannot recreate the Job, so a change would never reach kagent'
[[ "$(yq '.spec.template.spec.restartPolicy' "${job}")" == Never ]] || fail 'the Job does not run once per attempt'
# Flux substitutes braced variables in apps manifests, which would rewrite the script in prod only.
if grep -Fq "\${" "${work}/schedule.sh"; then
  fail 'the script uses a braced variable, which Flux would substitute'
fi
timeout="$(env_value EXECUTION_TIMEOUT)"
[[ "${timeout}" =~ ^[0-9]+s$ ]] || fail "the execution timeout ${timeout} is not a whole number of seconds"
# kagent cancels a run at its deadline; a deadline under a year would be a run time limit.
((${timeout%s} >= 31536000)) || fail "the execution timeout ${timeout} would cut a run off"
[[ "$(env_value PAUSED)" =~ ^(true|false)$ ]] || fail 'PAUSED is not a boolean'
[[ "$(env_value SCHEDULE)" =~ ^[^[:space:]]+([[:space:]][^[:space:]]+){4}$ ]] ||
  fail 'the schedule is not five cron fields, which kagent requires'
[[ -n "$(env_value TIME_ZONE)" && "$(env_value TIME_ZONE)" != Local ]] ||
  fail 'the time zone is not a fixed IANA zone, which kagent requires'
request_id="$(yq "${container} | .env[] | select(.name == \"REQUEST_ID\") | .valueFrom.fieldRef.fieldPath" "${job}")"
[[ "${request_id}" == "metadata.labels['batch.kubernetes.io/controller-uid']" ]] ||
  fail 'the create request ID is not the Job uid, so a retried create could make a second schedule'
[[ "$(yq '.spec.values.config.server.grpcReflection' "${release}")" == true ]] ||
  fail 'kagent does not serve gRPC reflection, which grpcurl needs to read its API'

# The stub records each call (method, request, flags) and answers from the scenario's files:
# list-<n>.json for the nth ListScheduledRuns, create.json and update.json. A file holding `fail`
# makes that call fail as grpcurl does on a gRPC error.
mkdir -p "${work}/bin"
cat >"${work}/bin/grpcurl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
dir="${STUB_DIR:?}"
method="${*: -1}"
method="${method##*/}"
printf '%s\n' "$*" >>"${dir}/flags"
printf '%s\n' "${method}" >>"${dir}/calls"
jq -c . >>"${dir}/requests"
case "${method}" in
  ListScheduledRuns) reply="${dir}/list-$(grep -c '^ListScheduledRuns$' "${dir}/calls").json" ;;
  CreateScheduledRun) reply="${dir}/create.json" ;;
  UpdateScheduledRun) reply="${dir}/update.json" ;;
  *) printf 'unexpected method %s\n' "${method}" >&2; exit 1 ;;
esac
[[ -f "${reply}" ]] || { printf 'no reply for %s\n' "${method}" >&2; exit 1; }
if [[ "$(cat "${reply}")" == fail ]]; then
  printf 'ERROR:\n  Code: Unavailable\n  Message: connection refused\n' >&2
  exit 1
fi
cat "${reply}"
STUB
chmod +x "${work}/bin/grpcurl"

# run <scenario>: runs the script with the manifest's environment and prints its exit code. The
# scenario's reply files must be in place first.
run() {
  local dir="${work}/$1" status=0 name
  mkdir -p "${dir}"
  : >"${dir}/calls"
  : >"${dir}/requests"
  : >"${dir}/flags"
  local -a environment=(PATH="${work}/bin:${PATH}" STUB_DIR="${dir}" REQUEST_ID=job-uid-1)
  for name in NAME AGENT_NAMESPACE AGENT_NAME SCHEDULE TIME_ZONE PAUSED EXECUTION_TIMEOUT PROMPT; do
    environment+=("${name}=$(env_value "${name}")")
  done
  env -i "${environment[@]}" sh "${work}/schedule.sh" >"${dir}/out" 2>&1 || status=$?
  printf '%s' "${status}"
}
# calls <scenario> <method>: how many times a scenario called one method.
calls() { grep -c "^$2\$" "${work}/$1/calls" || true; }
# request <scenario> <method>: the last request a scenario sent to one method.
request() {
  paste -d '\t' "${work}/$1/calls" "${work}/$1/requests" | awk -F '\t' -v m="$2" '$1 == m {r = $2} END {print r}'
}
# scenario <name>: makes a scenario's directory and prints its path.
scenario() { mkdir -p "${work}/$1" && printf '%s' "${work}/$1"; }
# schedule <config>: a ScheduledRun as the API returns it, for the lane's agent.
schedule() {
  jq -cn --argjson config "$1" --arg id "${2:-run-1}" '{id: $id, creator: "admin@kagent.dev",
    agent: {namespace: "agentic-engineer", name: "agentic-engineer"}, config: $config,
    etag: "etag-1", nextExecutionTime: null}'
}
# reply <schedule>: a create or update reply carrying one schedule.
reply() { jq -c '{scheduledRun: .}' <<<"$1"; }

# A missing schedule is created, once, with the manifest's config and the Job's request ID.
dir="$(scenario missing)"
printf '{"scheduledRuns":[],"page":{"nextPageToken":""}}' >"${dir}/list-1.json"
want_placeholder='{"name":"agentic-engineer-daily"}'
reply "$(schedule "${want_placeholder}")" >"${dir}/create.json"
[[ "$(run missing)" == 0 ]] || fail "a missing schedule was not created: $(cat "${dir}/out")"
[[ "$(calls missing CreateScheduledRun)" == 1 && "$(calls missing UpdateScheduledRun)" == 0 ]] ||
  fail 'a missing schedule was not created exactly once'
created="$(request missing CreateScheduledRun)"
want="$(jq -c .config <<<"${created}")"
expected="$(jq -cn --arg name "$(env_value NAME)" --arg schedule "$(env_value SCHEDULE)" \
  --arg timeZone "$(env_value TIME_ZONE)" --arg prompt "$(env_value PROMPT)" \
  --argjson paused "$(env_value PAUSED)" --arg timeout "$(env_value EXECUTION_TIMEOUT)" \
  '{name: $name, schedule: $schedule, timeZone: $timeZone, prompt: $prompt, paused: $paused,
    executionTimeout: $timeout}')"
[[ "${want}" == "${expected}" ]] || fail "the created config is not the manifest's: ${want}"
[[ "$(jq -c .agent <<<"${created}")" == '{"namespace":"agentic-engineer","name":"agentic-engineer"}' ]] ||
  fail "the schedule does not target the lane's agent: ${created}"
[[ "$(jq -r .requestId <<<"${created}")" == job-uid-1 ]] || fail 'the create did not carry the Job uid'
grep -q '^created scheduled run run-1: ' "${dir}/out" || fail "a create was not reported: $(cat "${dir}/out")"

# An equal schedule is left alone.
dir="$(scenario equal)"
jq -c '{scheduledRuns: [.], page: {nextPageToken: ""}}' <<<"$(schedule "${want}")" >"${dir}/list-1.json"
[[ "$(run equal)" == 0 ]] || fail "an equal schedule failed: $(cat "${dir}/out")"
[[ "$(calls equal CreateScheduledRun)" == 0 && "$(calls equal UpdateScheduledRun)" == 0 ]] ||
  fail 'an equal schedule was written'
grep -q '^unchanged scheduled run run-1: ' "${dir}/out" || fail "an unchanged schedule was not reported: $(cat "${dir}/out")"

# A schedule that differs, for example one paused or edited in the UI, is updated to the manifest.
dir="$(scenario drifted)"
drifted="$(jq -c '.paused = (.paused | not) | .schedule = "0 0 * * *"' <<<"${want}")"
jq -c '{scheduledRuns: [.], page: {nextPageToken: ""}}' <<<"$(schedule "${drifted}")" >"${dir}/list-1.json"
reply "$(schedule "${want}")" >"${dir}/update.json"
[[ "$(run drifted)" == 0 ]] || fail "a drifted schedule was not updated: $(cat "${dir}/out")"
[[ "$(calls drifted UpdateScheduledRun)" == 1 && "$(calls drifted CreateScheduledRun)" == 0 ]] ||
  fail 'a drifted schedule was not updated exactly once'
[[ "$(request drifted UpdateScheduledRun)" == "$(jq -cn --argjson config "${want}" \
  '{scheduledRunId: "run-1", etag: "etag-1", config: $config}')" ]] ||
  fail "the update did not send the manifest's config with the schedule's etag: $(request drifted UpdateScheduledRun)"
grep -q '^updated scheduled run run-1: ' "${dir}/out" || fail "an update was not reported: $(cat "${dir}/out")"

# The schedule is found on a later page, and the next page is asked for with the token given.
dir="$(scenario paged)"
other="$(schedule "$(jq -c '.name = "other"' <<<"${want}")" run-2)"
jq -c '{scheduledRuns: [.], page: {nextPageToken: "page-2"}}' <<<"${other}" >"${dir}/list-1.json"
jq -c '{scheduledRuns: [.], page: {nextPageToken: ""}}' <<<"$(schedule "${want}")" >"${dir}/list-2.json"
[[ "$(run paged)" == 0 ]] || fail "a schedule on a later page failed: $(cat "${dir}/out")"
[[ "$(calls paged ListScheduledRuns)" == 2 ]] || fail 'the script did not read every page'
[[ "$(calls paged CreateScheduledRun)" == 0 && "$(calls paged UpdateScheduledRun)" == 0 ]] ||
  fail 'a schedule on a later page was written as if missing'
[[ "$(request paged ListScheduledRuns)" == '{"page":{"limit":100,"pageToken":"page-2"}}' ]] ||
  fail "the second page was not asked for with its token: $(request paged ListScheduledRuns)"

# Two schedules with the lane's name, or one for another agent, fail without a write.
dir="$(scenario duplicated)"
jq -cn --argjson a "$(schedule "${want}")" --argjson b "$(schedule "${want}" run-3)" \
  '{scheduledRuns: [$a, $b], page: {nextPageToken: ""}}' >"${dir}/list-1.json"
[[ "$(run duplicated)" == 1 ]] || fail "duplicate schedules did not fail the Job: $(cat "${dir}/out")"
grep -q 'found 2 scheduled runs named agentic-engineer-daily' "${dir}/out" || fail 'duplicates were not named'
dir="$(scenario foreign)"
jq -c '.agent.name = "someone-else" | {scheduledRuns: [.], page: {nextPageToken: ""}}' \
  <<<"$(schedule "${want}")" >"${dir}/list-1.json"
[[ "$(run foreign)" == 1 ]] || fail "a schedule for another agent did not fail the Job: $(cat "${dir}/out")"

# A failed call fails the Job: before a write, during one, and when a write returns no schedule.
dir="$(scenario unreachable)"
printf 'fail' >"${dir}/list-1.json"
[[ "$(run unreachable)" != 0 ]] || fail 'an unreachable API did not fail the Job'
dir="$(scenario refused)"
printf '{"scheduledRuns":[],"page":{"nextPageToken":""}}' >"${dir}/list-1.json"
printf 'fail' >"${dir}/create.json"
[[ "$(run refused)" != 0 ]] || fail 'a refused create did not fail the Job'
dir="$(scenario empty)"
printf '{"scheduledRuns":[],"page":{"nextPageToken":""}}' >"${dir}/list-1.json"
printf '{}' >"${dir}/create.json"
[[ "$(run empty)" != 0 ]] || fail 'a create that returned no schedule did not fail the Job'

for name in missing equal drifted paged duplicated foreign; do
  case "${name}" in duplicated | foreign)
    [[ "$(calls "${name}" CreateScheduledRun)" == 0 && "$(calls "${name}" UpdateScheduledRun)" == 0 ]] ||
      fail "the ${name} scenario wrote a schedule"
    ;;
  esac
  # The UI shows only its own user's schedules, which a call without X-User-Id gets.
  if grep -q -e '-H' -e 'X-User-Id' "${work}/${name}/flags"; then
    fail "the ${name} scenario sent a header, so its schedule would not be the UI user's"
  fi
  grep -q -- '-plaintext' "${work}/${name}/flags" || fail "the ${name} scenario did not call kagent's plaintext port"
done

grep -Fq 'bash scripts/tests/test-agentic-engineer-schedule.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer schedule: PASS\n'
