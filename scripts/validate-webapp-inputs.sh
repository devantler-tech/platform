#!/usr/bin/env bash
# Validate one complete JSON WebApp without exposing its candidate values.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "$#" -gt 1 ]; then
  echo 'usage: validate-webapp-inputs.sh [json-file|-]' >&2
  exit 2
fi
input="${1:--}"
case "$input" in
  -|/*) ;;
  *) input="$(pwd -P)/$input" ;;
esac
if ! GOWORK=off go -C "$root" run ./scripts/validate-webapp-inputs json "$input" "$root/scripts/webapp-inputs.jq" >/dev/null; then
  echo 'FAIL: WebApp source is incomplete, unknown or outside the confined input contract' >&2
  exit 1
fi
