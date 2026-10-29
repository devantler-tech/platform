#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/temp"
token=ghs_fixture_read_only_token_never_log_123456789
# The existing job grant is unchanged; the token reaches only the fixed probe.
# shellcheck disable=SC2016
for workflow in ci.yaml validate-main.yaml; do
yq -o=json '.' "$root/.github/workflows/$workflow" | jq -e '
  .jobs["validate-webapp-compiler"] as $job |
  $job.permissions=={contents:"read"} and
  ([$job.steps[]|select(.env.PUBLIC_CI_READ_TOKEN?)] | length==1 and
    .[0].env.PUBLIC_CI_READ_TOKEN=="${{ github.token }}" and
    .[0].run=="bash scripts/probe-public-ci-read-identity.sh" and
    (.[0]|has("continue-on-error")|not)) and
  ([$job.steps[]|select(.env.PREBUILT_CI_READ_TOKEN?)] | length==1 and
    .[0].env.PREBUILT_CI_READ_TOKEN=="${{ github.token }}" and
    .[0].run=="bash scripts/tests/test-webapp-runtime-hosted.sh" and
    (.[0]|has("continue-on-error")|not))
' >/dev/null
done

# Execute the real hosted script's complete credential preamble. A child must
# receive neither the input token nor a copied variable's inherited export flag.
awk '/^root=/{exit} {print}' "$root/scripts/tests/test-webapp-runtime-hosted.sh" > "$work/preamble.sh"
cat >> "$work/preamble.sh" <<'PREAMBLE'
[[ "$hosted_ci_read_token" == "$FIXTURE_TOKEN" ]]
bash -c '[[ -z "${PREBUILT_CI_READ_TOKEN:-}" && -z "${hosted_ci_read_token:-}" ]]'
PREAMBLE
PREBUILT_CI_READ_TOKEN="$token" FIXTURE_TOKEN="$token" hosted_ci_read_token=ambient_export_attribute \
  bash -x "$work/preamble.sh" > "$work/preamble.log" 2>&1
if grep -Fq "$token" "$work/preamble.log"; then printf 'FAIL: hosted credential reached traced output\n' >&2; exit 1; fi
cat > "$work/expected-reads" <<'READS'
anonymous https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665
auth https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665
auth https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665/attempts/1
auth https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665
READS
jq -n '{id:39603006665,run_attempt:1,head_sha:"1d34013bb6d40dbf1736c21d677734fe27bfc982",head_branch:"main",event:"push",status:"completed",conclusion:"success",path:".github/workflows/ci.yaml",repository:{full_name:"devantler-tech/business-site",private:false},head_repository:{full_name:"devantler-tech/business-site",private:false}}' > "$work/run.json"
cat > "$work/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
config='' output='' headers=''
[[ "$*" != *--location* && "$*" != *' -L '* ]] || exit 81
[[ "$*" == *'--proto =https'* && "$*" == *'--max-filesize 1048576'* ]] || exit 82
url="${*: -1}"
case "$url" in
  https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665|https://api.github.com/repos/devantler-tech/business-site/actions/runs/39603006665/attempts/1) ;;
  *) exit 83 ;;
esac
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) config="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --dump-header) headers="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "$([[ -n "$config" ]] && echo auth || echo anonymous) $url" >> "$FIXTURE/reads"
count="$(wc -l < "$FIXTURE/reads" | tr -d ' ')"
status=200 limit=60 remaining=59
if [[ -n "$config" ]]; then
  [[ "$(ls -l "$config" | awk '{print substr($1,2,9)}')" == rw------- ]] || exit 84
  grep -Fxq "header = \"Authorization: Bearer $FIXTURE_TOKEN\"" "$config" || exit 85
  [[ -z "${PUBLIC_CI_READ_TOKEN:-}" ]] || exit 86
  limit=1000 remaining=999
fi
if [[ "$FAULT" == anonymous-quota && "$count" == 1 ]]; then status=403; remaining=0; fi
if [[ "$FAULT" == forbidden && "$count" == 2 ]]; then status=403; fi
if [[ "$FAULT" == redirect && "$count" == 2 ]]; then status=302; fi
if [[ "$FAULT" == same-bucket ]]; then limit=60; fi
printf 'HTTP/2 %s\r\nx-ratelimit-limit: %s\r\nx-ratelimit-remaining: %s\r\n\r\n' "$status" "$limit" "$remaining" > "$headers"
cp "$FIXTURE/run.json" "$output"
case "$FAULT:$count" in
  private:2) jq '.repository.private=true' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  missing-private:2) jq 'del(.head_repository.private)' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  wrong-source:2) jq '.head_sha="0000000000000000000000000000000000000000"' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  failed:2) jq '.conclusion="failure"' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  attempt-mismatch:3) jq '.run_attempt=2' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  final-rerun:4) jq '.run_attempt=2' "$output" > "$output.next"; mv "$output.next" "$output" ;;
  malformed:2) printf 'not json\n' > "$output" ;;
esac
printf '%s' "$status"
[[ "$status" -lt 400 ]] || exit 22
CURL
chmod +x "$work/bin/curl"
for scenario in valid stateless-token anonymous-quota forbidden redirect same-bucket private missing-private wrong-source failed attempt-mismatch final-rerun malformed missing-token injected-token quoted-token escaped-token wrong-context; do
  : > "$work/reads"
  task_token="$token" task_actions=true
  case "$scenario" in
    stateless-token) task_token="ghs_123456_$(printf '%0600d' 0).opaque-fixture.signature-fixture" ;;
    missing-token) task_token='' ;;
    injected-token) task_token=$'ghs_bad\r\nheader = malicious' ;;
    quoted-token) task_token='opaque"header' ;;
    escaped-token) task_token='opaque\header' ;;
    wrong-context) task_actions=false ;;
  esac
  if PATH="$work/bin:$PATH" FIXTURE="$work" FIXTURE_TOKEN="$task_token" FAULT="$scenario" \
    GITHUB_ACTIONS="$task_actions" RUNNER_ENVIRONMENT=github-hosted GITHUB_REPOSITORY=devantler-tech/platform \
    GITHUB_EVENT_NAME=pull_request RUNNER_TEMP="$work/temp" PUBLIC_CI_READ_TOKEN="$task_token" \
    bash -x "$root/scripts/probe-public-ci-read-identity.sh" > "$work/result" 2>&1; then verdict=accepted; else verdict=rejected; fi
  if [[ -n "$task_token" ]] && grep -Fq "$task_token" "$work/result"; then printf 'FAIL: credential reached output\n' >&2; exit 1; fi
  case "$scenario" in
    valid|stateless-token|anonymous-quota)
      [[ "$verdict" == accepted && "$(wc -l < "$work/reads" | tr -d ' ')" == 4 ]] || { cat "$work/result"; exit 1; }
      [[ "$(grep -c '^auth ' "$work/reads")" == 3 ]] || exit 1
      cmp -s "$work/expected-reads" "$work/reads" || exit 1 ;;
    *) [[ "$verdict" == rejected ]] || { printf 'FAIL: accepted %s\n' "$scenario" >&2; exit 1; } ;;
  esac
  printf '%s: %s\n' "$scenario" "$verdict"
done
