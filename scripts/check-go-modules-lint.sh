#!/usr/bin/env bash
# Lint first-party packages in every indexed module, without fixing source.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
if ! git -C "$root" ls-files -z -- go.mod ':(glob)**/go.mod' >"$work/modules" || [ ! -s "$work/modules" ]; then
  echo 'UNKNOWN: cannot discover a complete tracked Go module set' >&2
  exit 1
fi
: >"$work/validated"
while IFS= read -r -d '' modfile; do
  directory="${modfile%/go.mod}"
  if [ "$modfile" = go.mod ]; then directory=.; fi
  module_root="$root"
  if [ "$directory" != . ]; then module_root="$root/$directory"; fi
  if [ ! -f "$module_root/go.mod" ] || [ -L "$module_root/go.mod" ] ||
    ! physical_root="$(cd -- "$module_root" && pwd -P)" || [ "$physical_root" != "$module_root" ]; then
    printf 'UNKNOWN: missing or linked manifest/directory for module %s\n' "$directory" >&2
    exit 1
  fi
  printf '%s\0%s\0' "$directory" "$module_root" >>"$work/validated"
done <"$work/modules"

# The workflow installs this exact catalogue-aligned release. Never permit a
# different local default or ambient workspace/config to change the verdict.
golangci-lint --version | grep -Eq 'has version 2[.]14[.]0( |$)'
export GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local
printf 'Go lint target %s/%s\n' "$(go env GOOS)" "$(go env GOARCH)"
checked=0 tooling=0
while IFS= read -r -d '' directory && IFS= read -r -d '' module_root; do
  printf 'Checking Go lint module %s\n' "$directory"
  if ! go -C "$module_root" list -mod=readonly -e -json ./... >"$work/packages" ||
    ! jq -se 'all(.Error == null and ((.DepsErrors // []) | length == 0))' "$work/packages" >/dev/null; then
    printf 'UNKNOWN: cannot load complete packages for module %s\n' "$directory" >&2
    exit 1
  fi
  if [ ! -s "$work/packages" ]; then
    # A tool dependency module, such as the Helm CLI pin, owns no first-party
    # package. Report that distinction; never run a fake successful lint or
    # silently accept an empty application module.
    if ! go -C "$module_root" mod edit -json >"$work/module" ||
      ! jq -e '(.Tool // []) | length > 0' "$work/module" >/dev/null; then
      printf 'UNKNOWN: source module %s has no loadable packages\n' "$directory" >&2
      exit 1
    fi
    printf 'NO FIRST-PARTY PACKAGES: tools-only module %s\n' "$directory"
    tooling=$((tooling + 1))
    continue
  fi
  if ! (cd -- "$module_root" && golangci-lint run --no-config --modules-download-mode=readonly \
    --timeout=5m --max-issues-per-linter=0 --max-same-issues=0 ./...); then
    printf 'FAIL: Go lint refused module %s\n' "$directory" >&2
    exit 1
  fi
  checked=$((checked + 1))
done <"$work/validated"
if [ "$checked" -eq 0 ]; then
  echo 'UNKNOWN: no Go source modules were linted' >&2
  exit 1
fi
printf 'CHECKED %s Go source modules; %s tools-only modules have no first-party packages\n' "$checked" "$tooling"
