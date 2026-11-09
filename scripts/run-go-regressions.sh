#!/usr/bin/env bash
# Both fixed CI jobs together retain the complete root Go suite.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
die() {
  printf 'go-regressions: %s\n' "$*" >&2
  exit 2
}
[[ $# -le 1 ]] || die 'usage: run-go-regressions.sh [all|bridge|helpers]'
suite="${1:-all}"
case "$suite" in
  all) exec go test -parallel 8 -timeout 20m ./... ;;
  bridge | helpers) ;;
  *) die 'unknown partition' ;;
esac

module=github.com/devantler-tech/platform
bridge="$module/scripts/tests/refresh-flux-ghcr-auth"
packages="$(go list ./...)" || die 'complete package discovery failed'
helpers=() bridge_count=0 package_seen='|'
while IFS= read -r package; do
  [[ "$package" == "$module" || "$package" == "$module/"* ]] || die 'unexpected package identity'
  [[ "$package" =~ ^[a-zA-Z0-9._/-]+$ ]] || die 'invalid package identity'
  [[ "$package_seen" != *"|$package|"* ]] || die 'duplicate package identity'
  package_seen+="$package|"
  if [[ "$package" == "$bridge" ]]; then
    bridge_count=$((bridge_count + 1))
  else
    helpers+=("$package")
  fi
done <<<"$packages"
[[ "$bridge_count" == 1 && ${#helpers[@]} -gt 0 ]] || die 'incomplete root package inventory'
printf 'GO_REGRESSIONS suite=%s bridge=%s helpers=%s\n' "$suite" "$bridge_count" "${#helpers[@]}"

# Select whole packages. Go retains every test, example and fuzz seed.
if [[ "$suite" == bridge ]]; then
  exec go test -parallel 8 -timeout 20m "$bridge"
fi
exec go test -parallel 8 -timeout 20m "${helpers[@]}"
