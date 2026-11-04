#!/usr/bin/env bash
# Re-audit an operational workflow before carrying its build-input disposition.
set -euo pipefail
root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}"
[ "$#" -le 1 ] || exit 2
while IFS=' ' read -r expected path; do
  file="$root/$path"
  [ -f "$file" ] && [ ! -L "$file" ] || {
    echo 'operational input guard: missing or linked workflow' >&2
    exit 2
  }
  actual="$(shasum -a 256 "$file")"
  [ "${actual%% *}" = "$expected" ] || {
    echo "operational input guard: re-audit changed workflow $path before carrying CKV_GHA_7" >&2
    exit 2
  }
done <<'PROFILES'
85fc0c606350e016eeeb7f389bf126175d026a085e13f7dd1d0d71865879fb2f .github/workflows/prove-cilium-default-deny.yaml
d4c2ed403823edc4c535655af05f06ad707609f372b725d78b8c8c9f08a59ad8 .github/workflows/verify-alert-publisher-app.yaml
0e62201ccf7525eb743d3c6af13dda390eb489be8945f778954a00fd4882c3e0 .github/workflows/stale-issues.yaml
PROFILES
echo 'CHECKED the three reviewed operational input boundaries'
