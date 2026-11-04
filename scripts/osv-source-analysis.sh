#!/usr/bin/env bash
# Use the complete native OSV analyzer built with the repository compiler.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
GOTOOLCHAIN=go1.27.3 GOWORK=off go -C "$root" build -mod=readonly -o "$work/guard-osv-source" ./scripts/guard-osv-source
status=0
"$work/guard-osv-source" "$@" || status=$?
exit "$status"
