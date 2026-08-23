#!/usr/bin/env bash
# Regression tests for scripts/guard-shared-publish-workflow-pin.sh.
#
# WHY EACH NEGATIVE CASE ASSERTS ITS OWN MESSAGE. The guard has several ways to exit
# non-zero — most easily the EXPECTED_MIN_SUBJECTS floor, which fires whenever a
# fixture is escaped wrongly and the scan matches nothing. A case that only checked
# "exit != 0" would pass on a fixture the guard never actually read, and would keep
# passing after the property it names had regressed. So every case names the reason it
# expects to see, and a positive control runs first to prove the fixture is wired.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT

fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/scripts" "$fixture/config"
cp "$REPO_ROOT/scripts/guard-shared-publish-workflow-pin.sh" "$fixture/scripts/"

readonly PINNED='^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@([0-9a-f]{40}|refs/tags/v.+)$'

failures=0

# Write a subjects file whose FIRST line is the case under test and whose remaining
# seven are legitimate, so the floor is always satisfied and never the reason for a
# failure.
write_subjects() {
  local first="$1" i
  printf '%s\n' "$first" >"$fixture/config/subjects.yaml"
  for ((i = 0; i < 7; i++)); do
    printf "subject: '%s'\n" "$PINNED" >>"$fixture/config/subjects.yaml"
  done
}

run_guard() {
  ( cd "$fixture" && ./scripts/guard-shared-publish-workflow-pin.sh ) \
    >"$fixture/stdout" 2>"$fixture/stderr"
}

expect_accepted() {
  local name="$1"
  if run_guard; then
    printf 'ok   %s\n' "$name"
  else
    printf 'FAIL %s: guard rejected a legitimate configuration\n' "$name" >&2
    sed 's/^/     /' "$fixture/stderr" >&2
    failures=$((failures + 1))
  fi
}

expect_rejected() {
  local name="$1" reason="$2"
  if run_guard; then
    printf 'FAIL %s: guard ACCEPTED it\n' "$name" >&2
    failures=$((failures + 1))
  elif ! grep -q -- "$reason" "$fixture/stderr"; then
    printf 'FAIL %s: rejected, but not for the expected reason (%s)\n' "$name" "$reason" >&2
    sed 's/^/     /' "$fixture/stderr" >&2
    failures=$((failures + 1))
  else
    printf 'ok   %s\n' "$name"
  fi
}

# POSITIVE CONTROL. Everything below depends on the fixture being one the guard reads
# and validates; if this fails, no negative result underneath it means anything.
write_subjects "$(printf "subject: '%s'" "$PINNED")"
expect_accepted 'eight legitimate subjects are accepted'

# A plain (unquoted) scalar: here a whitespace-# genuinely does open a comment, so the
# comment's `@refs/tags/v.+` must not be mistaken for the value.
write_subjects 'subject: ^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@refs/heads/main$ # @refs/tags/v.+'
expect_rejected 'inline comment on a plain scalar cannot launder a branch ref' 'refs/heads/main'

# THE QUOTED-COMMENT BYPASS. Inside quotes a `#` is literal scalar content, so YAML
# hands cosign the whole string — including a second alternative permitting any
# branch. Truncating at the ` #` validated only the tag half and reported it pinned.
write_subjects "subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@refs/tags/v.+ # x|^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@refs/heads/.+\$'"
expect_rejected 'a # inside a quoted scalar does not truncate the value' 'refs/heads/.+'

# A legitimate quoted subject followed by a REAL comment must still be accepted — the
# comment is outside the quotes, so removing it is correct there.
write_subjects "$(printf "subject: '%s' # pinned by #2816" "$PINNED")"
expect_accepted 'a real trailing comment after a quoted scalar is still removed'

# `''` is an escaped quote rather than the end of the scalar, and no real subject can
# carry one. It is refused rather than parsed on a guess.
write_subjects "subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@refs/tags/v.+''|^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@refs/heads/.+$'"
expect_rejected "an escaped '' is refused rather than parsed on a guess" 'could not read the YAML scalar'

# Shapes the parser will not guess at. Both must fail CLOSED and say so, rather than
# being validated on a truncation.
write_subjects 'subject: "^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@refs/tags/v.+$"'
expect_rejected 'a double-quoted scalar is refused rather than parsed on a guess' 'could not read the YAML scalar'

write_subjects "subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@refs/tags/v.+\$"
expect_rejected 'an unterminated quoted scalar is refused' 'could not read the YAML scalar'

if [ "$failures" -ne 0 ]; then
  printf '\n%d test(s) failed\n' "$failures" >&2
  exit 1
fi
printf '\nall tests passed\n'
