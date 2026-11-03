#!/usr/bin/env bash
# Exercise the installed real linter against disposable indexed modules.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
repo="$work/repo"
mkdir -p "$repo/scripts" "$repo/nested" "$repo/tools"
cp "$here/check-go-modules-lint.sh" "$repo/scripts/check-go-modules-lint.sh"
git -C "$repo" init -q
printf 'module example.test/root\n\ngo 1.26.10\n' >"$repo/go.mod"
printf 'module example.test/nested\n\ngo 1.26.10\n' >"$repo/nested/go.mod"
printf 'module example.test/tools\n\ngo 1.26.10\n\ntool example.test/tool\n' >"$repo/tools/go.mod"
clean='package fixture
import "os"
func Check() error { return os.Chdir("/") }
'
bad='package fixture
import "os"
func Check() { os.Chdir("/") }
'
printf '%s' "$clean" >"$repo/main.go"
printf '%s' "$clean" >"$repo/nested/main.go"
git -C "$repo" add -- go.mod main.go nested/go.mod nested/main.go tools/go.mod
guard="$repo/scripts/check-go-modules-lint.sh"
bash "$guard" >"$work/clean.log" 2>&1
grep -Fxq 'CHECKED 2 Go source modules; 1 tools-only modules have no first-party packages' "$work/clean.log"
git -C "$repo" diff --exit-code -- go.mod nested/go.mod tools/go.mod
for partition in part-0 part-1; do
  bash "$guard" "$partition" >"$work/$partition.log" 2>&1
  if ! grep -Fq 'CHECKED 1 Go source modules;' "$work/$partition.log"; then
    echo "FAIL: $partition did not select exactly one fixture source module" >&2
    exit 1
  fi
done
grep -Fxq 'Checking Go lint module .' "$work/part-0.log"
grep -Fxq 'Checking Go lint module tools' "$work/part-0.log"
grep -Fxq 'Checking Go lint module nested' "$work/part-1.log"
grep '^Checking Go lint module ' "$work/clean.log" | LC_ALL=C sort >"$work/all-modules"
cat "$work/part-0.log" "$work/part-1.log" | grep '^Checking Go lint module ' | LC_ALL=C sort >"$work/partitioned-modules"
diff -u "$work/all-modules" "$work/partitioned-modules"
if bash "$guard" other >"$work/invalid.log" 2>&1 ||
  bash "$guard" part-0 extra >"$work/invalid.log" 2>&1; then
  echo 'FAIL: invalid lint partition was accepted' >&2
  exit 1
fi
for location in . nested; do
  printf '%s' "$bad" >"$repo/$location/main.go"
  git -C "$repo" add -- "$location/main.go"
  if bash "$guard" >"$work/bad.log" 2>&1; then
    echo "FAIL: real unchecked error in $location passed" >&2
    exit 1
  fi
  grep -Fq 'os.Chdir' "$work/bad.log"
  grep -Fq 'errcheck' "$work/bad.log"
  partition=part-0
  if [ "$location" = nested ]; then partition=part-1; fi
  if bash "$guard" "$partition" >"$work/partition-error.log" 2>&1; then
    echo "FAIL: $partition missed the real unchecked error in $location" >&2
    exit 1
  fi
  grep -Fq 'errcheck' "$work/partition-error.log"
  git -C "$repo" diff --exit-code -- go.mod main.go nested/go.mod nested/main.go tools/go.mod
  printf '%s' "$clean" >"$repo/$location/main.go"
  git -C "$repo" add -- "$location/main.go"
done
mkdir "$repo/added"
cp "$repo/nested/go.mod" "$repo/added/go.mod"
printf '%s' "$bad" >"$repo/added/main.go"
git -C "$repo" add -- added/go.mod added/main.go
if bash "$guard" >"$work/added.log" 2>&1; then
  echo 'FAIL: new indexed module escaped lint' >&2
  exit 1
fi
grep -Fq 'FAIL: Go lint refused module added' "$work/added.log"
failed_parts=0
for partition in part-0 part-1; do
  if ! bash "$guard" "$partition" >"$work/added-$partition.log" 2>&1; then
    grep -Fq 'FAIL: Go lint refused module added' "$work/added-$partition.log"
    failed_parts=$((failed_parts + 1))
  fi
done
[ "$failed_parts" -eq 1 ] || {
  echo 'FAIL: new indexed module must belong to exactly one lint partition' >&2
  exit 1
}
rm "$repo/nested/go.mod"
if bash "$guard" >"$work/missing.log" 2>&1; then
  echo 'FAIL: missing nested boundary fell back to its parent' >&2
  exit 1
fi
grep -Fq 'UNKNOWN: missing or linked manifest/directory for module nested' "$work/missing.log"
for partition in part-0 part-1; do
  if bash "$guard" "$partition" >"$work/missing-$partition.log" 2>&1; then
    echo "FAIL: $partition skipped a missing manifest in complete discovery" >&2
    exit 1
  fi
  grep -Fq 'UNKNOWN: missing or linked manifest/directory for module nested' "$work/missing-$partition.log"
done
printf 'PASS: real clean, disjoint complete partitions, root-error, nested-error, new-module and missing-boundary lint controls\n'
