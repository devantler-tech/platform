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

if ! installed="$(GOENV=off GOTOOLCHAIN=local go env GOVERSION)" || [[ ! "$installed" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
  echo 'UNKNOWN: cannot read the installed Go version' >&2
  exit 1
fi
installed="${installed#go}"

checked=0
while IFS= read -r -d '' directory && IFS= read -r -d '' module_root; do
  printf 'Checking Go module %s\n' "$directory"
  # A module may declare a newer Go than the CI toolchain installed from the
  # root go.mod (the KRO compiler module does). Check that module with exactly
  # the release it declares, fetched through Go's own verified toolchain path;
  # every other module uses the installed toolchain and never upgrades.
  declared="$(awk '$1 == "go" && NF == 2 { print $2; exit }' "$module_root/go.mod")"
  if [[ ! "$declared" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
    printf 'UNKNOWN: cannot read the go directive of module %s\n' "$directory" >&2
    exit 1
  fi
  toolchain=local
  if [ "$declared" != "$installed" ] &&
    [ "$(printf '%s\n%s\n' "$installed" "$declared" | sort -V | tail -n 1)" = "$declared" ]; then
    # A two-part directive names that line's first release. Go runs a matching
    # binary from PATH before it downloads one, so CI runners must not ship one.
    release="$declared"
    [[ "$release" =~ ^[0-9]+\.[0-9]+$ ]] && release="$release.0"
    toolchain="go$release"
    printf 'Module %s declares Go %s, newer than the installed %s; checking it with %s\n' \
      "$directory" "$declared" "$installed" "$toolchain"
  fi
  # -diff produces the proposed changes and fails on drift, without writing
  # either file. Ignore ambient workspace/modfile settings.
  if ! GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN="$toolchain" \
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
