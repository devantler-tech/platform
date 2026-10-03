#!/usr/bin/env bash
# Root test scripts are entry points even when CI invokes them through Bash.
# Nested fixtures and sourced libraries are outside this convention. Read the
# index: a local chmod cannot repair the mode that a fresh CI clone receives.
set -euo pipefail
repo_root="${1:-.}"
inventory="$(mktemp)"
trap 'rm -f "$inventory"' EXIT
git -C "$repo_root" ls-files --stage -z -- 'scripts/tests/*.sh' > "$inventory" || {
  echo '::error::cannot read tracked test entries' >&2; exit 2
}
count=0
status=0
while IFS= read -r -d '' record; do
  path="${record#*$'\t'}"
  entry="${path#scripts/tests/}"
  [[ "$entry" != */* ]] || continue
  count=$((count + 1))
  metadata="${record%%$'\t'*}"
  if [[ ! "$metadata" =~ ^100755[[:space:]][0-9a-f]+[[:space:]]0$ ]]; then
    echo "::error::$path must be tracked 100755 at stage zero" >&2
    status=1
  fi
done < "$inventory"
if [[ "$count" == 0 ]]; then
  echo '::error::found no tracked root test entries; cannot verify execute bits' >&2
  exit 2
fi
[[ "$status" == 0 ]] || exit "$status"
printf 'Verified tracked execute bits on %s test entries.\n' "$count"
