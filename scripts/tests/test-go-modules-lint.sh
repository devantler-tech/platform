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
for location in . nested; do
  printf '%s' "$bad" >"$repo/$location/main.go"
  git -C "$repo" add -- "$location/main.go"
  if bash "$guard" >"$work/bad.log" 2>&1; then
    echo "FAIL: real unchecked error in $location passed" >&2
    exit 1
  fi
  grep -Fq 'os.Chdir' "$work/bad.log"
  grep -Fq 'errcheck' "$work/bad.log"
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
rm "$repo/nested/go.mod"
if bash "$guard" >"$work/missing.log" 2>&1; then
  echo 'FAIL: missing nested boundary fell back to its parent' >&2
  exit 1
fi
grep -Fq 'UNKNOWN: missing or linked manifest/directory for module nested' "$work/missing.log"
printf 'PASS: real clean, root-error, nested-error, new-module and missing-boundary lint controls\n'
