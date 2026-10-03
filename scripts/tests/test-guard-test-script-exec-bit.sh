#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$root/scripts/guard-test-script-exec-bit.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git -C "$work" init -q
mkdir -p "$work/scripts/tests/fixtures" "$work/scripts"
printf '#!/usr/bin/env bash\nexit 0\n' > "$work/scripts/tests/test-entry.sh"
printf 'library_function() { :; }\n' > "$work/scripts/library.sh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$work/scripts/tests/fixtures/fake.sh"
git -C "$work" add -- scripts/tests/test-entry.sh scripts/library.sh scripts/tests/fixtures/fake.sh
fail() { echo "::error::$*" >&2; exit 1; }
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted a tracked non-executable test entry'; fi
grep -Fq 'scripts/tests/test-entry.sh' "$work/out" || fail 'did not identify the offending entry'
chmod +x "$work/scripts/tests/test-entry.sh"
git -C "$work" update-index --chmod=+x scripts/tests/test-entry.sh
bash "$guard" "$work"
git -C "$work" update-index --chmod=-x scripts/tests/test-entry.sh
[[ -x "$work/scripts/tests/test-entry.sh" ]] || fail 'negative control lost its on-disk execute bit'
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted a dropped index execute bit hidden by the filesystem'; fi
git -C "$work" update-index --force-remove -- scripts/tests/test-entry.sh
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'reported success without any test entries'; fi
printf 'Test entry execute-bit regression passed.\n'
