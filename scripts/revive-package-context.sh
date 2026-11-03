#!/usr/bin/env bash
# Keep MegaLinter's native engine/rules while retaining complete package context.
set -euo pipefail
if [ "$#" -eq 1 ]; then
  case "$1" in
    -version | --version | -h | --help) exec revive "$@" ;;
  esac
fi
if [ "$#" -lt 2 ] || [ "$1" != -set_exit_status ]; then
  echo 'unexpected native Revive invocation; no scan was accepted' >&2
  exit 2
fi
shift
# A list-of-files batch selects the scan, not its coverage: every invocation
# checks the complete tracked inventory so batching cannot omit a package.
exec go run ./scripts/check-go-package-style --from-megalinter "$@"
