#!/usr/bin/env bash
# Exercise the real runner's arguments, inventory refusals and child failures.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}
[[ -f "$root/scripts/run-go-regressions.sh" ]] || fail 'missing complete Go regression runner'
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/tree/scripts" "$work/bin"
cp "$root/scripts/run-go-regressions.sh" "$work/tree/scripts/"
cat >"$work/bin/go" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail
module=github.com/devantler-tech/platform
bridge="$module/scripts/tests/refresh-flux-ghcr-auth"
if [[ "$*" == 'list ./...' ]]; then
  case "${MODE:-}" in
    empty-packages) exit 0 ;;
    missing-package) printf '%s\n' "$module/scripts/a"; exit 0 ;;
    only-bridge) printf '%s\n' "$bridge"; exit 0 ;;
    bad-package) printf '%s\n' '-unexpected-option'; exit 0 ;;
    duplicate-package) printf '%s\n' "$bridge" "$module/scripts/a" "$module/scripts/a"; exit 0 ;;
  esac
  printf '%s\n' "$module/scripts/a" "$bridge" "$module/scripts/b"
  [[ "${MODE:-}" != failed-packages ]] || exit 19
else
  printf '%s\n' "$*" >>"$CALLS"
  case "${MODE:-}:$*" in
    failed-bridge:*refresh-flux-ghcr-auth) exit 23 ;;
    failed-helpers:*scripts/b) exit 29 ;;
  esac
fi
BASH
chmod +x "$work/bin/go"
run() {
  local part="$1" mode="${2:-}"
  : >"$work/calls"
  result=0
  PATH="$work/bin:$PATH" CALLS="$work/calls" MODE="$mode" \
    bash "$work/tree/scripts/run-go-regressions.sh" "$part" >"$work/output" 2>&1 || result=$?
}
module=github.com/devantler-tech/platform
bridge="$module/scripts/tests/refresh-flux-ghcr-auth"
helpers="test -parallel 8 -timeout 20m $module/scripts/a $module/scripts/b"
run bridge
[[ "$result" == 0 && "$(cat "$work/calls")" == "test -parallel 8 -timeout 20m $bridge" ]] ||
  fail 'the bridge runner must run the full bridge package once without a test filter'
run helpers
[[ "$result" == 0 && "$(cat "$work/calls")" == "$helpers" ]] ||
  fail 'the helper runner must run every other package once without a test filter'
run all
[[ "$result" == 0 && "$(cat "$work/calls")" == 'test -parallel 8 -timeout 20m ./...' ]] ||
  fail 'serial diagnostic changed'
for mode in empty-packages missing-package only-bridge bad-package duplicate-package failed-packages; do
  for part in bridge helpers; do
    run "$part" "$mode"
    [[ "$result" != 0 && ! -s "$work/calls" ]] || fail "$part: $mode did not refuse before execution"
  done
done
run bridge failed-bridge
[[ "$result" == 23 ]] || fail 'bridge child failure was lost'
run helpers failed-helpers
[[ "$result" == 29 ]] || fail 'helper child failure was lost'
run unexpected
[[ "$result" != 0 && ! -s "$work/calls" ]] || fail 'unknown partition ran tests'
echo 'PASS: complete disjoint package routing, serial control, discovery refusals and child failures'
