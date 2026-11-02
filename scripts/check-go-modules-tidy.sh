#!/usr/bin/env bash
# Refuse drift in every tracked module without modifying go.mod or go.sum.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

# Materialize and check discovery before consuming it: partial output from a
# failed Git command must never turn into an incomplete successful check.
if ! git -C "$root" ls-files -z -- go.mod ':(glob)**/go.mod' >"$work/modules"; then
  echo 'UNKNOWN: cannot discover tracked Go modules' >&2
  exit 1
fi
if [ ! -s "$work/modules" ]; then
  echo 'UNKNOWN: no tracked Go modules' >&2
  exit 1
fi

# Check every boundary before Go can reinterpret a missing nested manifest as
# packages in its parent module.
: >"$work/validated"
while IFS= read -r -d '' modfile; do
  directory="${modfile%/go.mod}"
  if [ "$modfile" = go.mod ]; then
    directory=.
  fi
  module_root="$root"
  if [ "$directory" != . ]; then
    module_root="$root/$directory"
  fi
  if [ ! -f "$module_root/go.mod" ] || [ -L "$module_root/go.mod" ] ||
    ! physical_root="$(cd -- "$module_root" && pwd -P)" || [ "$physical_root" != "$module_root" ]; then
    printf 'UNKNOWN: missing or linked manifest/directory for module %s\n' "$directory" >&2
    exit 1
  fi
  printf '%s\0%s\0' "$directory" "$module_root" >>"$work/validated"
done <"$work/modules"

checked=0
while IFS= read -r -d '' directory && IFS= read -r -d '' module_root; do
  printf 'Checking Go module %s\n' "$directory"
  # -diff produces the proposed changes and fails on drift, without writing
  # either file. Ignore ambient workspace/modfile settings; use the CI toolchain
  # already installed from go.mod rather than downloading another toolchain.
  if ! GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local \
    go -C "$module_root" mod tidy -diff; then
    printf 'FAIL: cannot verify tidy files for module %s.\n' "$directory" >&2
    printf 'Run (cd %q && go mod tidy), review the changes, and retry.\n' "$directory" >&2
    exit 1
  fi
  checked=$((checked + 1))
done <"$work/validated"
if [ "$checked" -eq 0 ]; then
  echo 'UNKNOWN: no tracked Go modules were checked' >&2
  exit 1
fi
printf 'CHECKED %s tidy Go modules\n' "$checked"
