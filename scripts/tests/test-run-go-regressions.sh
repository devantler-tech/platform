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
    bad-package) printf '%s\n' '-unexpected-option'; exit 0 ;;
  esac
  printf '%s\n' "$module/scripts/a" "$bridge" "$module/scripts/b"
  [[ "${MODE:-}" != failed-packages ]] || exit 19
elif [[ "$*" == "test -list ^(Test|Example|Fuzz) $bridge" ]]; then
  case "${MODE:-}" in
    empty-tests) printf 'ok\t%s\t0.1s\n' "$bridge"; exit 0 ;;
    duplicate-tests) printf 'TestAlpha\nTestAlpha\n' ;;
    bad-test) printf 'TestAlpha;exit 0\n' ;;
    *) printf 'TestAlpha\nTestBeta\nTestGamma\nExampleDelta\nFuzzEpsilon\n' ;;
  esac
  [[ "${MODE:-}" != missing-summary ]] || exit 0
  duration=0.1s
  [[ "${MODE:-}" != bad-summary ]] || duration=0..1s
  printf 'ok\t%s\t%s\n' "$bridge" "$duration"
  [[ "${MODE:-}" != failed-tests ]] || exit 21
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
for part in bridge-0 bridge-1; do
  run "$part"
  [[ "$result" == 0 ]] || {
    cat "$work/output"
    fail "$part failed"
  }
  helper_calls=0 bridge_calls=0
  while IFS= read -r call; do
    if [[ "$call" == "$helpers" ]]; then
      helper_calls=$((helper_calls + 1))
    elif [[ "$call" == 'test -parallel 8 -timeout 20m -run '*" $bridge" ]]; then
      bridge_calls=$((bridge_calls + 1))
      selector="${call#test -parallel 8 -timeout 20m -run }"
      selector="${selector% "$bridge"}"
      for test_name in TestAlpha TestBeta TestGamma ExampleDelta FuzzEpsilon; do
        if [[ "$test_name" =~ $selector ]]; then printf '%s\n' "$test_name" >>"$work/selected"; fi
      done
      [[ ! TestAlphaSuffix =~ $selector && ! PrefixTestAlpha =~ $selector ]] || fail 'selector is not exact'
    else
      fail "unexpected child command: $call"
    fi
  done <"$work/calls"
  [[ "$bridge_calls" == 1 ]] || fail 'partition did not run exactly one bridge subset'
  expected=0
  [[ "$part" != bridge-0 ]] || expected=1
  [[ "$helper_calls" == "$expected" ]] || fail 'helper packages were duplicated or omitted'
done
printf '%s\n' ExampleDelta FuzzEpsilon TestAlpha TestBeta TestGamma >"$work/expected"
LC_ALL=C sort "$work/selected" >"$work/actual"
cmp "$work/expected" "$work/actual" || fail 'partitions overlap or omit a test/example/fuzz seed'
run all
[[ "$result" == 0 && "$(cat "$work/calls")" == 'test -parallel 8 -timeout 20m ./...' ]] || fail 'serial diagnostic changed'
for mode in empty-packages missing-package bad-package failed-packages empty-tests duplicate-tests bad-test bad-summary missing-summary failed-tests; do
  run bridge-0 "$mode"
  [[ "$result" != 0 && ! -s "$work/calls" ]] || fail "$mode did not refuse before execution"
done
for part in bridge-0 bridge-1; do
  run "$part" failed-bridge
  [[ "$result" != 0 ]] || fail 'bridge child failure was lost'
  if [[ "$part" == bridge-0 ]]; then
    [[ "$(cat "$work/calls")" == *"$helpers"* ]] || fail 'a failed bridge omitted helper coverage'
  fi
done
run bridge-0 failed-helpers
[[ "$result" != 0 ]] || fail 'helper child failure was lost'
run unexpected
[[ "$result" != 0 && ! -s "$work/calls" ]] || fail 'unknown partition ran tests'
echo 'PASS: complete disjoint routing, serial control, discovery refusals and child failures'
