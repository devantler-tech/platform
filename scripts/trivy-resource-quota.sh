#!/usr/bin/env bash
# Keep native Trivy's complete scan; correct only the audited quota rule.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
native="${TRIVY_NATIVE_BIN:-$(command -v trivy)}"
case "$native" in /*) ;; *)
  echo 'An absolute native Trivy executable is required.' >&2
  exit 2
  ;;
esac
cd "$root"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
GOTOOLCHAIN=go1.27.3 GOWORK=off go build -mod=readonly -o "$work/guard-trivy-policy" ./scripts/guard-trivy-policy
status=0
case "$*" in
  --version | --help | 'fs --help')
    "$work/guard-trivy-policy" "$native" "$@" || status=$?
    ;;
  *)
    bash "$root/scripts/with-runtime-disposition-tools.sh" --kubectl-only -- \
      "$work/guard-trivy-policy" "$native" "$@" || status=$?
    ;;
esac
exit "$status"
