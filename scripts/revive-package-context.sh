#!/usr/bin/env bash
# Keep the pinned native rules while matching the package resolver compiler.
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
if [ "$(revive -version)" != 'version 1.16.0' ]; then
  echo 'the native MegaLinter Revive pin changed; no scan was accepted' >&2
  exit 2
fi
# A version-suffixed install otherwise ignores the repository's Go selection.
toolchain="$(go env GOVERSION)"
case "$toolchain" in
  go1.*) ;;
  *) echo 'unknown Go compiler; no scan was accepted' >&2; exit 2 ;;
esac
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
GOBIN="$work/bin" GOTOOLCHAIN="$toolchain" GOWORK=off go install github.com/mgechev/revive@v1.16.0
# A list-of-files batch selects the scan, not its coverage: every invocation
# checks the complete tracked inventory so batching cannot omit a package.
PATH="$work/bin:$PATH" GOTOOLCHAIN="$toolchain" go run ./scripts/check-go-package-style --from-megalinter "$@"
