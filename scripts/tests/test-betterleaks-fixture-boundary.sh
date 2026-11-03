#!/usr/bin/env bash
# Use the real engine: every disposition requires both its path and exact value.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
engine="${BETTERLEAKS_BIN:-betterleaks}"
test "$("$engine" version)" = 1.7.3 || {
  echo 'Betterleaks 1.7.3 required' >&2
  exit 1
}
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
config="$root/.betterleaks.toml"
printf '[extend]\nuseDefault = true\n' >"$work/default.toml"

scan() {
  local directory="$1" selected_config="$2" expected="$3" rule="$4" status=0
  rm -f -- "$work/report.json"
  (cd "$directory" && "$engine" --redact dir -c "$selected_config" --report-format json --report-path "$work/report.json" .) >"$work/engine.log" 2>&1 || status=$?
  if [ "$status" != "$expected" ]; then
    echo "FAIL: native $rule control returned $status, expected $expected" >&2
    exit 1
  fi
  # Node is supplied by the pinned MegaLinter image; jq is not.
  node - "$work/report.json" "$expected" "$rule" <<'NODE'
const fs = require('node:fs');
const [path, expected, rule] = process.argv.slice(2);
const report = JSON.parse(fs.readFileSync(path, 'utf8'));
// Betterleaks serializes its nil finding slice as null on a clean scan.
const valid = expected === '0'
  ? report === null || (Array.isArray(report) && report.length === 0)
  : Array.isArray(report) && report.length > 0 && report.some(row => row.RuleID === rule);
if (!valid) throw new Error('native Betterleaks report did not establish the expected control');
NODE
}

payload() {
  case "$1" in
    curl-auth-user) printf 'curl --%s %s https://example.invalid\n' user "$2" ;;
    slack-webhook-url) printf '%s\n' "$2" ;;
    generic-api-key) printf 'api_%s = %s\n' key "$2" ;;
    *) exit 2 ;;
  esac
}

paths=(
  '.github/workflows/validate-coroot-security-progress.yaml'
  'scripts/tests/test-private-mail-server.sh'
  'scripts/tests/test-private-mail-configuration.sh'
  'scripts/tests/test-trivyignore-configmap-content-boundary.sh'
  'scripts/prove-kyverno-cache-recovery/credentials_test.go'
  'scripts/tests/test-verify-wedding-backup-denial.sh'
  'k8s/bases/infrastructure/controllers/kyverno/background-v1.19.2-downstream-cleanup.patch'
  'scripts/tests/test-private-mail-server.sh'
  'scripts/prove-kyverno-cache-recovery/credentials_test.go'
  'k8s/bases/infrastructure/controllers/kyverno/background-v1.19.2-downstream-cleanup.patch'
)
rules=(curl-auth-user curl-auth-user curl-auth-user slack-webhook-url generic-api-key generic-api-key generic-api-key curl-auth-user generic-api-key generic-api-key)
values=(
  '65532:65532'
  'fixture:private-mail-fixture-only'
  'fixture:private-mail-recovery-fixture-only'
  'hooks.''slack.com/services/T00000000/B00000000/AbCdEfGhIjKlMnOpQrStUvWx'
  'cHJvYmU6cHJpdmF0ZS10ZXN0LXRva2Vu'
  'ZGVkaWNhdGVkLXNlY3JldA=='
  'LiyJx32VU3cwQfLchn/513qKhc25hq0pEANYJoWNnnI='
  'fixture:wrong-fixture-password'
  'b3RoZXI6dG9rZW4='
  'eMw0Xo+IfbbMlKmW7aHvpyQRv9RCXuWx/vs8AD+0x9A='
)
for index in "${!paths[@]}"; do
  directory="$work/case-$index"
  source="${paths[$index]}"
  rule="${rules[$index]}"
  value="${values[$index]}"
  mkdir -p "$directory/$(dirname "$source")"
  cp "$root/$source" "$directory/$source"
  scan "$directory" "$config" 0 "$rule"
  # Each declared value is really detected by the unchanged inherited rule.
  payload "$rule" "$value" >"$directory/$source"
  scan "$directory" "$work/default.toml" 1 "$rule"
  scan "$directory" "$config" 0 "$rule"
  # Changing the value in the allowed path must restore the original rule.
  changed="X${value:1}"
  if [ "$rule" = slack-webhook-url ]; then
    changed="${value%?}Y"
  fi
  payload "$rule" "$changed" >"$directory/$source"
  scan "$directory" "$config" 1 "$rule"
  # The exact allowed value in another path must also restore that rule.
  rm -- "$directory/$source"
  payload "$rule" "$value" >"$directory/neighbor.txt"
  scan "$directory" "$config" 1 "$rule"
done
# The native MegaLinter exclusions are applied directly, without an inheritance
# layer that drops Betterleaks 1.7.3's targeted rule dispositions.
for excluded in .agents .jscpd .megalinter_github_conf megalinter-reports report; do
  for prefix in '' nested/; do
    directory="$work/exclusions"
    rm -rf -- "$directory"
    mkdir -p "$directory/$prefix$excluded"
    payload generic-api-key 'ZGVkaWNhdGVkLXNlY3JldA==' >"$directory/$prefix$excluded/probe.txt"
    scan "$directory" "$config" 0 generic-api-key
    mv "$directory/$prefix$excluded" "$directory/$prefix$excluded-neighbor"
    scan "$directory" "$config" 1 generic-api-key
  done
done
echo 'PASS: native Betterleaks defaults, exact fixture values, changed values and neighboring paths'
