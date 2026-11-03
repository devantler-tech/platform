#!/usr/bin/env bash
# Validate the registered old-compiler control as data; materialize only privately.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
control=scripts/tests/fixtures/govulncheck-negative-control
mode="${1:-}"
case "$mode:$#" in
  --check:1|--lint:1|--style:1|--tidy:1|--prepare-scan:1|--materialize:2) ;;
  *) echo 'usage: check-go-negative-control.sh --check|--lint|--style|--tidy|--prepare-scan|--materialize <absolute-new-directory>' >&2; exit 2 ;;
esac
work="$(mktemp -d)"
created=''
cleanup() {
  if [ -n "$created" ]; then rm -rf -- "$created"; fi
  rm -rf -- "$work"
}
trap cleanup EXIT
fail() { echo "negative control: $*" >&2; exit 2; }
git -C "$root" ls-files -z >"$work/tracked" || fail 'complete tracked inventory is unavailable'
count=0
while IFS= read -r -d '' path; do
  case "$path" in
    "$control/go.mod.tmpl"|"$control/main.go.tmpl") count=$((count+1)) ;;
    "$control/"*|*.go.tmpl|*/go.mod.tmpl|go.mod.tmpl) fail 'unknown, mixed or orphaned control templates' ;;
  esac
done <"$work/tracked"
[ "$count" = 2 ] || fail 'the registered pair must be tracked exactly once'
for directory in scripts scripts/tests scripts/tests/fixtures "$control"; do
  [ -d "$root/$directory" ] && [ ! -L "$root/$directory" ] || fail 'linked or missing control directory'
done
shopt -s dotglob nullglob
for path in "$root/$control/"*; do
  case "$path" in
    "$root/$control/go.mod.tmpl"|"$root/$control/main.go.tmpl") ;;
    *) fail 'unexpected control input, including untracked source' ;;
  esac
done
shopt -u dotglob nullglob
for name in go.mod main.go; do
  path="$root/$control/$name.tmpl"
  [ -f "$path" ] && [ -r "$path" ] && [ ! -L "$path" ] || fail 'linked, missing or unreadable control input'
  cp "$path" "$work/$name"
  actual="$(shasum -a 256 "$work/$name")"
  actual="${actual%% *}"
  case "$name:$actual" in
    go.mod:7d0d20dc4a43282847ac0c7c70d879878048395d80bd5190e29239f7cf43edec|main.go:08c36b098b7e3286a6f8601c1e29eab65cda4a7803dc14260dee5a213fe81fd3) ;;
    *) fail 'control source or Go 1.24.0 profile changed' ;;
  esac
done
if [ "$mode" = --check ]; then
  echo 'CHECKED one complete byte-bound old-compiler control factory'
  exit 0
fi
destination="$work/module"
if [ "$mode" = --materialize ]; then
  destination="$2"
  case "$destination" in /*) ;; *) fail 'destination must be absolute' ;; esac
  parent="$(cd "$(dirname "$destination")" && pwd -P)" || fail 'destination parent does not exist'
  destination="$parent/$(basename "$destination")"
  case "$destination" in "$root"|"$root/"*) fail 'validation must remain outside the checkout' ;; esac
elif [ "$mode" = --prepare-scan ]; then
  [ "${GITHUB_ACTIONS:-}" = true ] && [ "${GITHUB_REPOSITORY:-}" = devantler-tech/platform ] || fail 'checkout generation is reserved for the separate native scan job'
  destination="$root/.govulncheck-negative-control"
fi
[ ! -e "$destination" ] && [ ! -L "$destination" ] || fail 'destination already exists'
mkdir -m 0700 "$destination" || fail 'destination could not be reserved'
created="$destination"
for name in go.mod main.go; do
  (set -C; cat "$work/$name" >"$destination/$name")
  cmp "$work/$name" "$destination/$name" || fail 'materialized bytes differ'
done
if [ "$mode" = --materialize ] || [ "$mode" = --prepare-scan ]; then
  created=''
  if [ "$mode" = --prepare-scan ]; then echo .govulncheck-negative-control; else printf '%s\n' "$destination"; fi
  exit 0
fi
export GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local
case "$mode" in
  --tidy) go -C "$destination" mod tidy -diff ;;
  --lint)
    golangci-lint --version | grep -Eq 'has version 2[.]14[.]0( |$)'
    go -C "$destination" list -mod=readonly -e -json ./... >"$work/packages.json"
    jq -se 'length == 1 and all(.Error == null and ((.DepsErrors // []) | length == 0) and (.GoFiles | length > 0))' "$work/packages.json" >/dev/null
    (cd "$destination" && golangci-lint run --no-config --modules-download-mode=readonly --timeout=5m --max-issues-per-linter=0 --max-same-issues=0 ./...) ;;
  --style)
    git -C "$destination" init -q
    git -C "$destination" add -- go.mod main.go
    (cd "$destination" && go run "$root/scripts/check-go-package-style/main.go") ;;
esac
echo 'CHECKED private old-compiler control validation'
