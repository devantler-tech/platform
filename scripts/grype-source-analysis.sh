#!/usr/bin/env bash
# Preserve native source SCA and bind every VEX occurrence to the same snapshot.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
engine="$(command -v grype)"
case "$engine" in /*) ;; *)
  echo 'an absolute native Grype engine is required' >&2
  exit 2
  ;;
esac
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
GOTOOLCHAIN=go1.27.3 GOWORK=off go -C "$root" build -mod=readonly -o "$work/guard-grype-source" ./scripts/guard-grype-source
status=0
"$work/guard-grype-source" "$engine" "$@" || status=$?
exit "$status"
