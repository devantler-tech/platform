#!/usr/bin/env bash
# A disposition never permits a changed build-producing workflow.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
engine="${CHECKOV_NATIVE_BIN:-$(command -v checkov)}"
[ "$("$engine" --version)" = 3.3.15 ] || {
  echo 'native Checkov 3.3.15 is required' >&2
  exit 2
}
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
bash "$root/scripts/guard-checkov-operational-inputs.sh" "$root"
names=(prove-cilium-default-deny.yaml verify-alert-publisher-app.yaml stale-issues.yaml)
listed=() unlisted=()
mkdir -p "$work/source/.github/workflows" "$work/unlisted/.github/workflows"
for name in "${names[@]}"; do
  source="$root/.github/workflows/$name"
  listed+=("$source")
  cp "$source" "$work/source/.github/workflows/$name"
  destination="$work/unlisted/.github/workflows/$name"
  sed '/checkov:skip=CKV_GHA_7:/d' "$source" >"$destination"
  unlisted+=("$destination")
done
"$engine" --file "${listed[@]}" --framework github_actions --check CKV_GHA_7 --output json >"$work/listed.json"
jq -e '.summary.failed == 0 and .summary.skipped == 3 and (.results.skipped_checks | length == 3)' "$work/listed.json" >/dev/null
status=0
"$engine" --file "${unlisted[@]}" --framework github_actions --check CKV_GHA_7 --output json >"$work/unlisted.json" || status=$?
[ "$status" = 1 ]
jq -e '.summary.failed == 3 and all(.results.failed_checks[]; .check_id == "CKV_GHA_7")' "$work/unlisted.json" >/dev/null
bash "$root/scripts/guard-checkov-operational-inputs.sh" "$work/source"
for name in "${names[@]}"; do
  changed="$work/source/.github/workflows/$name"
  cp "$changed" "$work/original"
  # This would inherit the same native annotation but invalidates its premise.
  printf '\n  unreviewed-build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go build ./...\n' >>"$changed"
  if bash "$root/scripts/guard-checkov-operational-inputs.sh" "$work/source" >"$work/refusal" 2>&1; then
    echo 'FAIL: a changed workflow inherited the operational disposition' >&2
    exit 1
  fi
  cp "$work/original" "$changed"
done
echo 'PASS: native operational dispositions, unchanged unlisted findings and changed-producer refusals'
