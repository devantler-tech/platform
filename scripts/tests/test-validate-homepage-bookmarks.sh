#!/usr/bin/env bash
# Compatibility entry point; all acceptance cases live in the native Go harness.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
cd "${repo_root}"
exec go test -count=1 ./scripts/tests/homepage-bookmarks
