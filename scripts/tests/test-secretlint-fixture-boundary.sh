#!/usr/bin/env bash
# Keep every recommended rule; distinguish one exact local-stub URL from others.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
engine="${SECRETLINT_BIN:-secretlint}"
test "$("$engine" --version)" = 13.0.5 || {
  echo 'Secretlint 13.0.5 required' >&2
  exit 1
}
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
config="$root/.secretlintrc.json"
printf '{"rules":[{"id":"@secretlint/secretlint-rule-preset-recommend"}]}\n' >"$work/default.json"

scan() {
  local source="$1" selected_config="$2" expected="$3" status=0
  # --output makes Secretlint return zero even with findings. Redirect stdout.
  "$engine" --secretlintrc "$selected_config" --no-gitignore --format=json "$source" >"$work/report.json" 2>"$work/engine.log" || status=$?
  if [ "$status" != "$expected" ]; then
    echo "FAIL: native Secretlint returned $status, expected $expected" >&2
    exit 1
  fi
  if [ "$expected" = 0 ]; then
    jq -e 'length == 1 and all(.[]; .messages | length == 0)' "$work/report.json" >/dev/null
  else
    jq -e 'length == 1 and any(.[]; any(.messages[]; .ruleId == "@secretlint/secretlint-rule-basicauth" and .messageId == "BasicAuth"))' "$work/report.json" >/dev/null
  fi
}

scan "$root/scripts/tests/test-verify-wedding-backup-denial.sh" "$work/default.json" 1
scan "$root/scripts/tests/test-verify-wedding-backup-denial.sh" "$config" 0
for variant in original user credential host; do
  user=REFUSEDBYSTUB credential=refused-by-stub-secret host=127.0.0.1
  case "$variant" in
    user) user=OTHERSTUB ;;
    credential) credential=changed-stub-secret ;;
    host) host=192.0.2.1 ;;
  esac
  printf '%s://%s:%s@%s:9000\n' http "$user" "$credential" "$host" >"$work/neighbor.txt"
  scan "$work/neighbor.txt" "$work/default.json" 1
  if [ "$variant" = original ]; then
    scan "$work/neighbor.txt" "$config" 0
  else
    scan "$work/neighbor.txt" "$config" 1
  fi
done
echo 'PASS: native Secretlint recommended rules, exact local stub, changed identities, credentials and hosts'
