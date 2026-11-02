#!/usr/bin/env bash
# Exercise the shipped non-persisting permission probe with controlled HTTP responses.
set -euo pipefail
root="$(git rev-parse --show-toplevel)"
workflow="${1:-$root/.github/workflows/publish-release-recovery.yaml}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() {
  printf 'release writer refusal: %s\n' "$1" >&2
  exit 1
}
yq -o=json '.' "$workflow" >"$work/workflow.json"
jq -e '.jobs.recover.steps | map(select(.id == "read-only-refusal")) | length == 1' "$work/workflow.json" >/dev/null ||
  fail 'required read-only capability control is missing'
jq -er '.jobs.recover.steps[] | select(.id == "read-only-refusal") | .run' "$work/workflow.json" >"$work/probe.sh"
mkdir -p "$work/bin" "$work/runner"
cat >"$work/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
fixture="$(cd "$(dirname "$0")/.." && pwd -P)"
[[ -z "${GH_TOKEN-}${GITHUB_TOKEN-}${CURL_HOME-}${HTTPS_PROXY-}" ]]
[[ "${1-}" == --disable ]]
method='' endpoint='' output='' payload='' auth=0 accept=0 content=0 version=0 bounded=0 format=0 size=0 proto=0 connect=0
while (($#)); do
  case "$1" in
    --disable|--silent|--show-error) shift ;;
    --proto) [[ "$2" == '=https' ]]; proto=1; shift 2 ;;
    --connect-timeout) [[ "$2" == 10 ]]; connect=1; shift 2 ;;
    --max-time) [[ "$2" == 30 ]]; bounded=1; shift 2 ;;
    --max-filesize) [[ "$2" == 1048576 ]]; size=1; shift 2 ;;
    --request) method="$2"; shift 2 ;;
    --header)
      case "$2" in
        'Authorization: Bearer fixture-read-token') auth=1 ;;
        'Accept: application/vnd.github+json') accept=1 ;;
        'Content-Type: application/json') content=1 ;;
        'X-GitHub-Api-Version: 2022-11-28') version=1 ;;
        *) exit 1 ;;
      esac
      shift 2 ;;
    --data-binary) payload="${2#@}"; [[ "$2" == @* ]]; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --write-out) [[ "$2" == '%{http_code}' ]]; format=1; shift 2 ;;
    https://api.github.com/repos/devantler-tech/platform/releases/generate-notes) endpoint="$1"; shift ;;
    *) exit 1 ;;
  esac
done
[[ "$method" == POST && -n "$endpoint" && -n "$output" && -n "$payload" && "$auth$accept$content$version$bounded$format$size$proto$connect" == 111111111 ]]
jq -e '. == {tag_name:"release-recovery-capability-probe",target_commitish:"main"}' "$payload" >/dev/null
printf 'request\n' >>"$fixture/calls"
dirname "$output" >"$fixture/private-directory"
case "$(cat "$fixture/case")" in
  forbidden) status=403; body='{"message":"Resource not accessible by integration"}' ;;
  hidden) status=404; body='{"message":"Not Found"}' ;;
  authorized) status=200; body='{"name":"probe","body":""}' ;;
  limited) status=403; body='{"message":"API rate limit exceeded"}' ;;
  throttled) status=429; body='{"message":"Resource not accessible by integration"}' ;;
  unavailable) status=502; body='{"message":"Resource not accessible by integration"}' ;;
  redirected) status=307; body='{"message":"Not Found"}' ;;
  unauthenticated) status=401; body='{"message":"Bad credentials"}' ;;
  malformed) status=403; body='not JSON' ;;
  ambiguous) status=403; body='{} {"message":"Resource not accessible by integration"}' ;;
  empty) status=403; body='{}' ;;
  sensitive) status=403; body='{"message":"fixture-read-token"}' ;;
  transport) exit 7 ;;
  *) exit 1 ;;
esac
printf '%s' "$body" >"$output"
printf '%s' "$status"
CURL
chmod +x "$work/bin/curl"
exercise() {
  local scenario="$1" credential="${2-fixture-read-token}" repository="${3-devantler-tech/platform}" branch="${4-main}"
  printf '%s' "$scenario" >"$work/case"
  rm -f "$work/calls" "$work/private-directory"
  local result=0
  PATH="$work/bin:$PATH" RUNNER_TEMP="$work/runner" GH_TOKEN="$credential" GITHUB_TOKEN=ambient-token HTTPS_PROXY=invalid-proxy \
    REPOSITORY="$repository" DEFAULT_BRANCH="$branch" bash -euo pipefail "$work/probe.sh" >"$work/output" 2>&1 || result=$?
  if grep -Fq 'fixture-read-token' "$work/output"; then fail 'credential or response content reached the log'; fi
  if [[ -f "$work/private-directory" && -e "$(cat "$work/private-directory")" ]]; then fail 'private response files were retained'; fi
  if [[ "$scenario" == forbidden || "$scenario" == hidden ]] && [[ -n "$credential" && "$repository" == devantler-tech/platform && "$branch" == main ]]; then
    [[ "$result" == 0 ]] || fail "supported refusal failed: $scenario"
    [[ "$(cat "$work/calls")" == request ]] || fail 'probe was retried or skipped'
    grep -Fxq 'Read-only credential refused by the non-persisting writer capability endpoint.' "$work/output" || fail 'no verified refusal result'
  else
    [[ "$result" != 0 ]] || fail "unsafe proof accepted: $scenario"
  fi
}
for scenario in forbidden hidden authorized limited throttled unavailable redirected unauthenticated malformed ambiguous empty sensitive transport; do
  exercise "$scenario"
done
exercise forbidden ''
[[ ! -e "$work/calls" ]] || fail 'empty credential reached the transport'
exercise forbidden fixture-read-token other/repo
[[ ! -e "$work/calls" ]] || fail 'wrong repository reached the transport'
exercise forbidden fixture-read-token devantler-tech/platform other
[[ ! -e "$work/calls" ]] || fail 'wrong branch reached the transport'
printf 'PASS: shipped read-only probe refuses unexpected authorization, ambiguous errors and unsafe scope without persisting or logging responses\n'
