#!/usr/bin/env bash
# Validate one complete JSON WebApp without exposing its candidate values.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "$#" -gt 1 ]; then
  echo 'usage: validate-webapp-inputs.sh [json-file|-]' >&2
  exit 2
fi
if ! jq --exit-status --slurp --from-file "$root/scripts/webapp-inputs.jq" "${1:--}" >/dev/null 2>/dev/null; then
  echo 'FAIL: WebApp source is incomplete, unknown or outside the confined input contract' >&2
  exit 1
fi
