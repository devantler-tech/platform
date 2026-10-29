#!/usr/bin/env bash
# A fixed public metadata capability proof; no registry, Git or cluster writes.
set +x
set -euo pipefail
umask 077
fail() { printf 'FAIL: public CI identity: %s\n' "$1" >&2; exit 1; }
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted &&
   "${GITHUB_REPOSITORY:-}" == devantler-tech/platform ]] || fail 'requires the existing isolated hosted job'
[[ -n "${RUNNER_TEMP:-}" && -d "$RUNNER_TEMP" && ! -L "$RUNNER_TEMP" ]] || fail 'missing regular runner scratch directory'
task_read_token="${PUBLIC_CI_READ_TOKEN:-}"
[[ "$task_read_token" =~ ^[A-Za-z0-9_]+$ && "${#task_read_token}" -ge 20 && "${#task_read_token}" -le 512 ]] || fail 'missing or unsafe explicit job token'
work="$(mktemp -d "$RUNNER_TEMP/public-ci-identity.XXXXXX")"
trap 'rm -rf "$work"' EXIT
printf 'header = "Authorization: Bearer %s"\n' "$task_read_token" > "$work/auth.curlrc"
unset PUBLIC_CI_READ_TOKEN task_read_token
readonly run_url=https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665
header() {
  awk -v key="$1" '
    index(tolower($0), key ":") == 1 {count++; value=substr($0,length(key)+2); sub(/^[ \t]+/,"",value); sub(/\r$/,"",value)}
    END {if(count!=1)exit 1; print value}
  ' "$work/headers"
}
validate_run() {
  jq -se 'length==1 and (.[0]|
    .id==39603006665 and .run_attempt==1 and
    .head_sha=="1d34013bb6d40dbf1736c21d677734fe27bfc982" and .head_branch=="main" and
    .event=="push" and .status=="completed" and .conclusion=="success" and .path==".github/workflows/ci.yaml" and
    .repository.full_name=="devantler-tech/business-site" and .repository.private==false and
    .head_repository.full_name=="devantler-tech/business-site" and .head_repository.private==false)' "$work/body" >/dev/null ||
    fail 'public source, attempt or success mismatch'
}
read_observation() {
  local url="$1" authenticated="$2" status=0 code
  local args
  args=(-q --fail --silent --show-error --connect-timeout 10 --max-time 20 --max-filesize 1048576 --proto '=https'
    --dump-header "$work/headers" --output "$work/body" --write-out '%{http_code}' -H 'Accept: application/vnd.github+json')
  if [[ "$authenticated" == true ]]; then args+=(--config "$work/auth.curlrc"); fi
  code="$(curl "${args[@]}" "$url")" || status=$?
  observed_limit="$(header x-ratelimit-limit)" || fail 'missing or ambiguous quota identity'
  [[ "$observed_limit" =~ ^[0-9]{1,9}$ ]] || fail 'invalid quota identity'
  [[ "$((10#$observed_limit))" -gt 0 ]] || fail 'invalid zero quota identity'
  if [[ "$authenticated" == false && "$status" == 22 && ( "$code" == 403 || "$code" == 429 ) ]]; then
    [[ "$(header x-ratelimit-remaining)" == 0 ]] || fail 'anonymous control is not an explicit quota refusal'
    return
  fi
  [[ "$status" == 0 && "$code" == 200 ]] || fail 'complete public observation unavailable'
  validate_run
}
observed_limit=''
read_observation "$run_url" false
anonymous_limit="$observed_limit"
# Three authenticated observations mirror the maintained verifier's run,
# exact-attempt and final-rerun checks. No refusal falls back to another identity.
for url in "$run_url" "$run_url/attempts/1" "$run_url"; do
  read_observation "$url" true
  [[ "$((10#$observed_limit))" -gt "$((10#$anonymous_limit))" ]] || fail 'authenticated quota bucket was not established'
done
printf 'PASS: existing hosted job identity reads the fixed public run, exact attempt and final recheck in a separate authenticated quota bucket.\n'
