#!/usr/bin/env bash
# Keep native Trivy's complete scan; correct only the audited quota rule.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
native="${TRIVY_NATIVE_BIN:-$(command -v trivy)}"
case "$native" in /*) ;; *) echo 'An absolute native Trivy executable is required.' >&2; exit 2 ;; esac
cd "$root"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
go build -o "$work/guard-trivy-policy" ./scripts/guard-trivy-policy
status=0
"$work/guard-trivy-policy" "$native" "$@" || status=$?
exit "$status"
