#!/usr/bin/env bash
# Exercise the actual adapter and engine, including an unselected tagged source.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
repo="$work/repo"
mkdir -p "$repo/scripts/check-go-package-style" "$repo/nested"
cp "$root/scripts/check-go-package-style/main.go" "$repo/scripts/check-go-package-style/main.go"
cp "$root/scripts/revive-package-context.sh" "$repo/scripts/revive-package-context.sh"
version="$(awk '$1 == "go" {print $2}' "$root/go.mod")"
printf 'module example.test/packagecontext\n\ngo %s\n' "$version" >"$repo/go.mod"
printf '// Package fixture is an offline control.\npackage fixture\n' >"$repo/source.go"
printf '// Package fixture retains tagged source coverage.\npackage fixture\n\n// Check accepts an unused argument.\nfunc Check(_ int) {}\n' >"$repo/nested/only_windows.go"
git -C "$repo" init -q
git -C "$repo" add -- go.mod source.go nested/only_windows.go scripts/check-go-package-style/main.go scripts/revive-package-context.sh
adapter="$repo/scripts/revive-package-context.sh"
[ "$(bash "$adapter" -version)" = "$(revive -version)" ]
for unexpected in --config -set_exit_status; do
  if bash "$adapter" "$unexpected" >"$work/invalid.log" 2>&1; then
    echo 'FAIL: unexpected or empty scan invocation passed' >&2
    exit 1
  fi
done
(cd "$repo" && bash "$adapter" -set_exit_status source.go) >"$work/clean.log" 2>&1
grep -Fq 'REVIVE tracked=3 directories=3' "$work/clean.log"
printf '// Package fixture retains tagged source coverage.\npackage fixture\n\n// Check accepts an unused argument.\nfunc Check(unused int) {}\n' >"$repo/nested/only_windows.go"
if (cd "$repo" && bash "$adapter" -set_exit_status source.go) >"$work/bad.log" 2>&1; then
  echo 'FAIL: selected-file invocation omitted an unselected tagged source' >&2
  exit 1
fi
grep -Fq 'nested/only_windows.go' "$work/bad.log"
grep -Fq 'seems to be unused' "$work/bad.log"
if (cd "$repo" && bash "$adapter" -set_exit_status missing.go) >"$work/missing.log" 2>&1; then
  echo 'FAIL: untracked source selection passed' >&2
  exit 1
fi
grep -Fq 'unknown or untracked' "$work/missing.log"
printf 'PASS: native metadata, complete package coverage, tagged-source refusal and invalid selections\n'
