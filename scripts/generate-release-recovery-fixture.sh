#!/usr/bin/env bash
# Regenerate only the marked credential-free CI fixture jobs from the production offline job.
set -euo pipefail
[[ $# -le 2 ]] || {
  echo "Usage: generate-release-recovery-fixture.sh [source [ci]]" >&2
  exit 2
}
root="$(cd "$(dirname "$0")/.." && pwd)"
source="${1:-$root/.github/workflows/publish-release-recovery.yaml}"
destination="${2:-$root/.github/workflows/ci.yaml}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
yq '{"jobs": {"test-publish-release-recovery": (.jobs.offline-test | del(.if) | .name="🩹 Validate the release recovery build")}}' "$source" >"$work/jobs.yaml"
sed '1d' "$work/jobs.yaml" >"$work/fragment"
awk -v fragment="$work/fragment" '
  /^  # BEGIN GENERATED RELEASE RECOVERY FIXTURES$/ { print; while ((getline line < fragment) > 0) print line; close(fragment); inside=1; next }
  /^  # END GENERATED RELEASE RECOVERY FIXTURES$/ { inside=0 }
  !inside { print }
' "$destination" >"$work/ci.yaml"
[[ "$(grep -c "^  # BEGIN GENERATED RELEASE RECOVERY FIXTURES$" "$work/ci.yaml")" == 1 && "$(grep -c "^  # END GENERATED RELEASE RECOVERY FIXTURES$" "$work/ci.yaml")" == 1 ]] || {
  echo "release recovery fixture markers must be unique" >&2
  exit 1
}
cp "$work/ci.yaml" "$destination"
