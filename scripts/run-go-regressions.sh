#!/usr/bin/env bash
# Both fixed CI partitions together retain the complete root Go suite.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
die() {
  printf 'go-regressions: %s\n' "$*" >&2
  exit 2
}
[[ $# -le 1 ]] || die 'usage: run-go-regressions.sh [all|bridge-0|bridge-1]'
case "${1:-all}" in
all) exec go test -parallel 8 -timeout 20m ./... ;;
bridge-0) partition=0 ;;
bridge-1) partition=1 ;;
*) die 'unknown partition' ;;
esac
module=github.com/devantler-tech/platform
bridge="$module/scripts/tests/refresh-flux-ghcr-auth"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
go list ./... >"$work/packages" || die 'complete package discovery failed'
helpers=() bridge_count=0 package_seen='|'
while IFS= read -r package || [[ -n "$package" ]]; do
  [[ "$package" == "$module" || "$package" == "$module/"* ]] || die 'unexpected package identity'
  [[ "$package" =~ ^[a-zA-Z0-9._/-]+$ ]] || die 'invalid package identity'
  [[ "$package_seen" != *"|$package|"* ]] || die 'duplicate package identity'
  package_seen+="$package|"
  if [[ "$package" == "$bridge" ]]; then
    bridge_count=$((bridge_count + 1))
  else
    helpers+=("$package")
  fi
done <"$work/packages"
[[ "$bridge_count" == 1 && ${#helpers[@]} -gt 0 ]] || die 'incomplete root package inventory'

# Go's compiled inventory includes top-level tests, examples and fuzz seeds.
# An exact top-level -run selector retains all of each selected test's subtests.
go test -list '^(Test|Example|Fuzz)' "$bridge" >"$work/tests" || die 'compiled test discovery failed'
seen='|' total=0 selected=0 summary=0 selector='^('
while IFS= read -r entry || [[ -n "$entry" ]]; do
  if [[ "$entry" =~ ^(Test|Example|Fuzz)[a-zA-Z0-9_]*$ ]]; then
    [[ "$summary" == 0 && "$seen" != *"|$entry|"* ]] || die 'duplicate or misplaced test identity'
    seen+="$entry|"
    if ((total % 2 == partition)); then
      [[ "$selected" == 0 ]] || selector+='|'
      selector+="$entry"
      selected=$((selected + 1))
    fi
    total=$((total + 1))
  else
    read -r status package duration extra <<<"$entry"
    [[ "$status" == ok && "$package" == "$bridge" && "$duration" =~ ^[0-9]+([.][0-9]+)?s$ && -z "$extra" && "$summary" == 0 ]] ||
      die 'unexpected compiled test discovery output'
    summary=1
  fi
done <"$work/tests"
[[ "$summary" == 1 && "$total" -ge 2 && "$selected" -gt 0 ]] || die 'incomplete compiled test inventory'
selector+=')$'
printf 'GO_REGRESSIONS partition=%s tests=%s total=%s helpers=%s\n' "$partition" "$selected" "$total" "${#helpers[@]}"

# Preserve all work even if one command fails; the other CI partition also runs
# with fail-fast disabled. The job remains unsuccessful on either child failure.
result=0
go test -parallel 8 -timeout 20m -run "$selector" "$bridge" || result=$?
if [[ "$partition" == 0 ]]; then
  go test -parallel 8 -timeout 20m "${helpers[@]}" || result=$?
fi
exit "$result"
