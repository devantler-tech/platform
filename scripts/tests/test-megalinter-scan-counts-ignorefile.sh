#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

mkdir -p "$scratch/bin"
cat >"$scratch/bin/trivy" <<'FAKE_TRIVY'
#!/usr/bin/env bash
set -euo pipefail

if [ "${1-}" = "--version" ]; then
  printf 'Version: 0.71.2\n'
  exit 0
fi

printf '%s\n' "$*" >"${TRIVY_ARGS_LOG:?}"
printf 'fixture.yaml\n============\nTests: 1 (SUCCESSES: 1, FAILURES: 0)\n'
FAKE_TRIVY
chmod +x "$scratch/bin/trivy"

PATH="$scratch/bin:$PATH" \
  TRIVY_ARGS_LOG="$scratch/trivy-args" \
  "$repo_root/scripts/megalinter-scan-counts.sh" --trivy-only >/dev/null

trivy_args="$(<"$scratch/trivy-args")"
case " $trivy_args " in
  *" --ignorefile .trivyignore.yaml "*) ;;
  *)
    printf 'megalinter scan omitted the path-scoped Trivy ignore file: %s\n' "$trivy_args" >&2
    exit 1
    ;;
esac

printf 'megalinter Trivy ignore-file contract OK\n'
