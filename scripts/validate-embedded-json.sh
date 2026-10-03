#!/usr/bin/env bash
# Validate the checkout's embedded ConfigMap JSON from any working directory.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec go -C "$repo_root" run -mod=readonly ./scripts/validate-embedded-json "$@"
