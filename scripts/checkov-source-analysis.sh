#!/usr/bin/env bash
# Preserve complete native Checkov scanning and rebind its resource decisions.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
engine="$(command -v checkov)"
case "$engine" in /*) ;; *)
  echo 'an absolute native Checkov engine is required' >&2
  exit 2
  ;;
esac
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
GOTOOLCHAIN=go1.27.3 GOWORK=off go -C "$root" build -mod=readonly -o "$work/guard-checkov-source" ./scripts/guard-checkov-source
status=0
"$work/guard-checkov-source" "$engine" "$@" || status=$?
exit "$status"
