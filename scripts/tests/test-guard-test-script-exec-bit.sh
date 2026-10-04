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
cp "$root/.mega-linter.yml" "$work/.mega-linter.yml"
for library in cosign-failure-lib.sh ghcr-auth-lib.sh publish-workflow-approved-revisions.lib.sh refresh-flux-ghcr-auth-safety.sh; do
  printf 'library_function() { :; }\n' > "$work/scripts/$library"
  git -C "$work" add -- "scripts/$library"
done
git -C "$work" add -- scripts/tests/test-entry.sh scripts/library.sh scripts/tests/fixtures/fake.sh
fail() { echo "::error::$*" >&2; exit 1; }
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted a tracked non-executable test entry'; fi
grep -Fq 'scripts/tests/test-entry.sh' "$work/out" || fail 'did not identify the offending entry'
chmod +x "$work/scripts/tests/test-entry.sh"
git -C "$work" update-index --chmod=+x scripts/tests/test-entry.sh
bash "$guard" "$work"
cp "$work/.mega-linter.yml" "$work/valid-linter.yml"
yq -i '.BASH_EXEC_FILTER_REGEX_EXCLUDE = ".*"' "$work/.mega-linter.yml"
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted a broadened bash-exec library exclusion'; fi
grep -Fq 'exclude only the four reviewed sourced libraries' "$work/out" || fail 'did not identify the broadened boundary'
cp "$work/valid-linter.yml" "$work/.mega-linter.yml"
printf '\n---\nmalformed: [\n' >> "$work/.mega-linter.yml"
parser_status=0
bash "$guard" "$work" > "$work/out" 2>&1 || parser_status=$?
[[ "$parser_status" == 2 ]] || fail 'accepted a partial YAML read or did not fail closed'
grep -Fq 'cannot read the bash-exec library exclusion' "$work/out" || fail 'did not identify the partial YAML read'
cp "$work/valid-linter.yml" "$work/.mega-linter.yml"
git -C "$work" update-index --chmod=+x scripts/ghcr-auth-lib.sh
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted an executable sourced library'; fi
grep -Fq 'scripts/ghcr-auth-lib.sh' "$work/out" || fail 'did not identify the library mode regression'
git -C "$work" update-index --chmod=-x scripts/ghcr-auth-lib.sh
git -C "$work" update-index --chmod=-x scripts/tests/test-entry.sh
[[ -x "$work/scripts/tests/test-entry.sh" ]] || fail 'negative control lost its on-disk execute bit'
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'accepted a dropped index execute bit hidden by the filesystem'; fi
git -C "$work" update-index --force-remove -- scripts/tests/test-entry.sh
if bash "$guard" "$work" > "$work/out" 2>&1; then fail 'reported success without any test entries'; fi
printf 'Test entry execute-bit regression passed.\n'
