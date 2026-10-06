#!/usr/bin/env bash
# Compatibility entry point for the native Go validator. Preserve caller-independent
# defaults and the validator exit codes: 0 valid, 1 violations, 2 unknown.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
[ "$#" -le 2 ] || { echo "::error::usage: validate-homepage-bookmarks.sh [config-map.yaml] [k8s-root]"; exit 2; }
binary="$(mktemp "${TMPDIR:-/tmp}/homepage-bookmarks.XXXXXX")"
trap 'rm -f -- "$binary"' EXIT
go -C "$repo_root" build -mod=readonly -o "$binary" ./scripts/validate-homepage-bookmarks || exit 2
"$binary" "${1:-${repo_root}/k8s/bases/apps/homepage/config-map.yaml}" "${2:-${repo_root}/k8s}"
