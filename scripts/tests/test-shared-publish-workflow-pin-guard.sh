#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/guard-shared-publish-workflow-pin.sh.
#
# The guard is the only thing standing between the shared-publish-workflow cosign
# subjects and a silent widening: a widened matcher still verifies, so no schema,
# kubeconform pass or deploy notices. That makes the guard's own failure modes the
# interesting ones — it fails by passing, never by crashing.
#
# The behaviour half runs a COPY of the guard over a synthetic tree. The guard
# derives its scan root from its own location (`dirname $BASH_SOURCE/..`) and greps
# `.`, so placing the copy at <tmp>/scripts/ makes <tmp> the repository it sees.
# That is the seam; without it every case would scan the real repository and the
# ablations could not vary anything.
#
# The wiring half asserts the guard is actually reached. A guard no workflow calls
# protects nothing, and this one is deliberately unconditional — not behind a paths
# filter — because the subjects it covers live in four different trees.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly guard="${root_dir}/scripts/guard-shared-publish-workflow-pin.sh"

# These subjects are cosign identity REGEXES, so TWO ref forms pin a revision and the
# guard accepts both (#3308): the pattern text `[0-9a-f]{40}`, which admits any 40-hex
# commit, and a CONCRETE 40-hex commit naming exactly one. The second is what a
# generated approved-revision allow-list emits, and it is strictly narrower.
#
# The two forms are DISJOINT — neither string can match the other's check — so the
# cases below can attribute an acceptance to one form or the other rather than to
# "something matched".
readonly SHA_PATTERN='[0-9a-f]{40}'

# A real 40-hex commit: the revision that signed the deployed github-config artifact,
# as reported by report-publish-workflow-signing-revisions.sh. Using a revision the
# report actually names keeps this test anchored to the shape the generator emits
# rather than to an invented string.
readonly CONCRETE_SHA='b3783a5148b844b3f1f4011cdc84871ad66348c1'
# A second, different real revision — the one every consumer pins today. A diverged
# consumer's generated set is the alternation of these two, which is the shape that
# must be accepted for a currently deployed artifact to keep verifying.
readonly CONCRETE_SHA_2='3ee6cb9bbd616c2a38779be41a5e41f3e77282d0'

# The opening of a legacy application matcher, and a single quote, for the cases that write
# a whole subject line.
readonly SUBJECT_ID='^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@'
readonly Q="'"

pass_count=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

ok() {
  pass_count=$((pass_count + 1))
  printf 'ok — %s\n' "$1"
}

[ -f "${guard}" ] || fail "guard not found at ${guard}"

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

# write_matcher_list <dir> <n>... — the floor the guard reads (#4558): one matcher expected in
# each named k8s/subject-<n>.yaml.
write_matcher_list() {
  local dir="$1" i
  shift
  : >"${dir}/scripts/shared-publish-workflow-matchers.tsv"
  for i in "$@"; do
    printf '1\tk8s/subject-%s.yaml\n' "${i}" >>"${dir}/scripts/shared-publish-workflow-matchers.tsv"
  done
}

# build_tree <dir> <ref-for-subject-1> — writes eight subjects, the first carrying
# the supplied ref and the remaining seven a plain 40-hex pin. One varying subject
# is what isolates the ref check from the floor check.
build_tree() {
  local dir="$1" first_ref="$2" i
  rm -rf "${dir}"
  mkdir -p "${dir}/scripts" "${dir}/k8s"
  cp "${guard}" "${dir}/scripts/guard-shared-publish-workflow-pin.sh"
  write_matcher_list "${dir}" 1 2 3 4 5 6 7 8

  printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n        subject: '\''^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@%s$'\''\n' \
    "${first_ref}" >"${dir}/k8s/subject-1.yaml"

  for i in 2 3 4 5 6 7 8; do
    printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n        subject: '\''^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@%s$'\''\n' \
      "${SHA_PATTERN}" >"${dir}/k8s/subject-${i}.yaml"
  done
}

# run_tree <dir> — exit status of the guard as that tree's repository root.
run_tree() {
  local dir="$1" status=0
  (cd "${dir}" && bash "${dir}/scripts/guard-shared-publish-workflow-pin.sh") \
    >"${dir}/stdout" 2>"${dir}/stderr" || status=$?
  return "${status}"
}

# The controller patch contains OpenAPI schema keys named subject. Those keys
# are not signer settings, while other subjects in the same file remain live.
profile_tree="$work_dir/helm-schema"
build_tree "$profile_tree" "$SHA_PATTERN"
profile_file='k8s/providers/docker/infrastructure/controllers/flux-instance/flux-instance.yaml'
mkdir -p "$profile_tree/$(dirname "$profile_file")"
cp "$root_dir/$profile_file" "$profile_tree/$profile_file"
cp "$root_dir/scripts/guard-controller-helm-profile.sh" "$profile_tree/scripts/"
run_tree "$profile_tree" || {
  cat "$profile_tree/stderr" >&2
  fail 'audited HelmRelease schema should not be treated as a signer'
}
ok 'the audited schema subject is classified as a schema property'
cat >>"$profile_tree/$profile_file" <<'YAML'
      - target:
          kind: OCIRepository
          name: flux-system
        patch: |
          apiVersion: source.toolkit.fluxcd.io/v1
          kind: OCIRepository
          metadata:
            name: flux-system
          spec:
            verify:
              matchOIDCIdentity:
                - subject:
YAML
outside_line="$(wc -l <"$profile_tree/$profile_file" | tr -d ' ')"
if run_tree "$profile_tree"; then
  fail 'a subject outside the audited patch was skipped'
fi
grep -Fq "$profile_file:$outside_line: subject does not open with the anchored literal" "$profile_tree/stderr" ||
  fail 'the added subject did not reach the identity check'
ok 'a subject outside the audited patch still reaches the identity check'
cp "$root_dir/$profile_file" "$profile_tree/$profile_file"
yq -i '(.spec.kustomize.patches[] | select(.target.kind == "CustomResourceDefinition").patch) |= sub("OIDCIdentityMatch", "unaudited schema")' "$profile_tree/$profile_file"
if run_tree "$profile_tree"; then
  fail 'an unaudited schema patch was skipped'
fi
ok 'a changed schema patch must be audited again'

# --- GREEN: the narrowed shape is what the guard accepts ---------------------

tree="${work_dir}/green"
build_tree "${tree}" "[0-9a-f]{40}"
if run_tree "${tree}"; then
  ok "eight SHA-only subjects pass"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "eight SHA-only subjects should pass, guard rejected them"
fi

# --- GREEN: the generated approved-revision shapes are accepted (#3308) ------
#
# A concrete commit is what an approved-revision allow-list emits, and it is strictly
# NARROWER than the pattern form: one revision instead of the whole 40-hex space. It
# was rejected before #3308, so correct generated output read as a guard failure.
# These cases are what stop that widening being reverted silently.

tree="${work_dir}/green-concrete"
build_tree "${tree}" "${CONCRETE_SHA}"
if run_tree "${tree}"; then
  ok "a concrete 40-hex commit passes"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "a concrete 40-hex commit should pass, guard rejected it"
fi

# The shape a DIVERGED consumer needs: the revision that signed the artifact currently
# deployed AND the revision pinned today. The #3048 report names both, and a set
# carrying only one of them stops verifying an artifact that is running right now.
tree="${work_dir}/green-concrete-pair"
build_tree "${tree}" "(${CONCRETE_SHA}|${CONCRETE_SHA_2})"
if run_tree "${tree}"; then
  ok "an alternation of two concrete commits passes"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "a two-revision approved set should pass, guard rejected it"
fi

# --- RED: every widening must fire -------------------------------------------
#
# `([0-9a-f]{40}|refs/tags/v.+)` is the shape this change removed (#3022). It is
# listed FIRST because it is the regression this test exists for: it is the exact
# prior contents of all eight subjects, so a guard that still accepts it would let
# the whole change revert silently while reporting a clean repository.

assert_rejected() {
  local label="$1" ref="$2" dir="${work_dir}/red-$3"
  build_tree "${dir}" "${ref}"
  if run_tree "${dir}"; then
    fail "guard ACCEPTED ${label} (ref: ${ref}) — the widening is not enforced"
  fi
  grep -q 'does not pin\|not a fully grouped' "${dir}/stderr" ||
    fail "guard rejected ${label} but not for the ref reason: $(head -1 "${dir}/stderr")"
  ok "rejects ${label}"
}

assert_rejected "the pre-#3022 SHA-or-tag alternation" '([0-9a-f]{40}|refs/tags/v.+)' alternation
assert_rejected "a bare tag ref" 'refs/tags/v.+' tagonly
assert_rejected "a tag ref with a literal version" 'refs/tags/v1.2.3' tagliteral
assert_rejected "a wildcard ref" '.+' wildcard
assert_rejected "a branch ref" 'refs/heads/main' branchref
assert_rejected "a bare moving ref" 'main' bareref
assert_rejected "a short commit" '0123456' shortsha
assert_rejected "a partially grouped alternation" '([0-9a-f]{40})?refs/heads/.+' partialgroup

# --- RED: the #3308 widening must not have opened a hole ---------------------
#
# The concrete form is accepted only as a WHOLE 40-hex alternative. Each case below is
# one character, one case-fold or one alternative away from that, so any of them
# passing would mean the new check matches by prefix, ignores case, or is applied
# per-substring rather than per-alternative.
assert_rejected "a 39-hex near-commit" "${CONCRETE_SHA%?}" sha39
assert_rejected "a 41-hex near-commit" "${CONCRETE_SHA}a" sha41
assert_rejected "an uppercase commit" "$(printf '%s' "${CONCRETE_SHA}" | tr 'a-f' 'A-F')" shaupper
assert_rejected "a concrete commit with a wildcard appended" "${CONCRETE_SHA}.+" shasuffix
assert_rejected "an approved set widened back with a branch" "(${CONCRETE_SHA}|refs/heads/main)" shabranch

# --- RED: the floor names what must be found (#4558) --------------------------
#
# An empty or reduced result from a filtered read is a claim about the FILTER. The floor
# used to be a count, which another matcher could make up; it is now the list of files
# that hold a matcher. Every refusal below asserts its own message, because a fixture
# escaped wrongly fails for some other reason and would otherwise pass as this one.

# assert_tree_rejected <label> <dir> <expected-stderr-pattern>
assert_tree_rejected() {
  local label="$1" dir="$2" expected="$3"
  if run_tree "${dir}"; then
    fail "guard ACCEPTED ${label}"
  fi
  grep -q -- "${expected}" "${dir}/stderr" ||
    fail "guard rejected ${label} but not for the expected reason: $(head -1 "${dir}/stderr")"
  ok "rejects ${label}"
}

readonly OTHER_FILE_HEAD='spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n'

tree="${work_dir}/red-floor"
build_tree "${tree}" "[0-9a-f]{40}"
rm -f "${tree}/k8s/subject-8.yaml"
assert_tree_rejected "a named file that no longer holds its matcher" "${tree}" \
  'k8s/subject-8.yaml holds 0 .* expected 1'

# One matcher disappears while another appears: the count is unchanged, which is what
# satisfied the old floor.
tree="${work_dir}/red-floor-swap"
build_tree "${tree}" "[0-9a-f]{40}"
mv "${tree}/k8s/subject-8.yaml" "${tree}/k8s/subject-9.yaml"
assert_tree_rejected "one matcher vanishing while another appears" "${tree}" \
  'k8s/subject-9.yaml holds a shared-publish-workflow matcher but is not named'
grep -q 'k8s/subject-8.yaml holds 0' "${tree}/stderr" ||
  fail "the swap was refused without naming the file that lost its matcher"

tree="${work_dir}/red-floor-extra"
build_tree "${tree}" "[0-9a-f]{40}"
printf "${OTHER_FILE_HEAD}        subject: '^https://github\\\\.com/devantler-tech/actions/\\\\.github/workflows/publish-app\\\\.yaml@%s\$'\n" \
  "${SHA_PATTERN}" >>"${tree}/k8s/subject-8.yaml"
assert_tree_rejected "a named file holding more matchers than the list says" "${tree}" \
  'k8s/subject-8.yaml holds 2 .* expected 1'

tree="${work_dir}/red-floor-missing-list"
build_tree "${tree}" "[0-9a-f]{40}"
rm -f "${tree}/scripts/shared-publish-workflow-matchers.tsv"
assert_tree_rejected "a missing list of matcher files" "${tree}" 'is missing or unreadable'

tree="${work_dir}/red-floor-empty-list"
build_tree "${tree}" "[0-9a-f]{40}"
printf '# nothing\n' >"${tree}/scripts/shared-publish-workflow-matchers.tsv"
assert_tree_rejected "a list of matcher files that names none" "${tree}" 'names no file'

tree="${work_dir}/red-floor-malformed-list"
build_tree "${tree}" "[0-9a-f]{40}"
printf 'k8s/subject-1.yaml\n' >"${tree}/scripts/shared-publish-workflow-matchers.tsv"
assert_tree_rejected "a list line without a count" "${tree}" 'is not <count><TAB>'

tree="${work_dir}/red-floor-zero-count"
build_tree "${tree}" "[0-9a-f]{40}"
printf '0\tk8s/subject-1.yaml\n' >"${tree}/scripts/shared-publish-workflow-matchers.tsv"
assert_tree_rejected "a list line whose count is zero" "${tree}" 'not a positive number'

tree="${work_dir}/red-floor-duplicate"
build_tree "${tree}" "[0-9a-f]{40}"
printf '1\tk8s/subject-1.yaml\n' >>"${tree}/scripts/shared-publish-workflow-matchers.tsv"
assert_tree_rejected "a list naming one file twice" "${tree}" 'more than once'

# --- RED: a subject the patterns do not select is still accounted for (#4558) ---
#
# Both patterns recognise the workflow identity by its text. A regex that means the same
# identity in another spelling is selected by neither, so it used to be left unjudged
# while the remaining matchers satisfied the count. Every subject is now either judged or
# proved to name a different repository.

# assert_extra_subject_rejected <label> <slug> <subject-line> <expected-stderr-pattern> —
# eight pinned matchers, all named, plus one further file holding the supplied line.
assert_extra_subject_rejected() {
  local label="$1" dir="${work_dir}/red-extra-$2" line="$3" expected="$4"
  build_tree "${dir}" "[0-9a-f]{40}"
  printf "${OTHER_FILE_HEAD}%s\n" "${line}" >"${dir}/k8s/other.yaml"
  assert_tree_rejected "${label}" "${dir}" "${expected}"
}

# assert_extra_subject_accepted <label> <slug> <subject-line>
assert_extra_subject_accepted() {
  local label="$1" dir="${work_dir}/green-extra-$2" line="$3"
  build_tree "${dir}" "[0-9a-f]{40}"
  printf "${OTHER_FILE_HEAD}%s\n" "${line}" >"${dir}/k8s/other.yaml"
  if run_tree "${dir}"; then
    ok "accepts ${label}"
  else
    printf '%s\n' "$(cat "${dir}/stderr")" >&2
    fail "guard rejected ${label}"
  fi
}

readonly ORG_ID='^https://github\.com/devantler-tech/'

assert_extra_subject_rejected "a matcher whose dots are written as classes" class-dots \
  "        subject: ${Q}${ORG_ID}actions/[.]github/workflows/publish-app[.]yaml@.+\$${Q}" \
  'can name the repository that hosts the shared publish workflows'
assert_extra_subject_rejected "a matcher whose repository is partly a wildcard" repo-wildcard \
  "        subject: ${Q}${ORG_ID}act.*@.+\$${Q}" \
  'does not open its repository with a literal name'
assert_extra_subject_rejected "a matcher whose repository ends in a quantifier" repo-quantifier \
  "        subject: ${Q}${ORG_ID}xa?ctions/.+\$${Q}" \
  'does not open its repository with a literal name'
assert_extra_subject_rejected "a matcher for the canonical repository alone, in another spelling" canonical-class \
  "        subject: ${Q}${ORG_ID}[.]github/[.]github/workflows/publish-manifests[.]yaml@.+\$${Q}" \
  'does not open its repository with a literal name'
assert_extra_subject_rejected "a subject that accepts every identity" wildcard-subject \
  "        subject: ${Q}.*${Q}" \
  'does not open with the anchored literal'
assert_extra_subject_rejected "an unanchored subject" unanchored \
  "        subject: ${Q}https://github\\.com/devantler-tech/platform/x${Q}" \
  'does not open with the anchored literal'
assert_extra_subject_rejected "a first-party subject with an ungrouped alternative" top-alternation \
  "        subject: ${Q}${ORG_ID}platform/\\.github/workflows/cd\\.yaml@refs/heads/main\$|.*${Q}" \
  'alternation outside a group'
assert_extra_subject_rejected "an alternative hidden behind a class holding a bracket" class-bracket \
  "        subject: ${Q}${ORG_ID}platform/[](]x|.*${Q}" \
  'character class opening with'
assert_extra_subject_rejected "a subject whose owner is not literal" owner-wildcard \
  "        subject: ${Q}^https://github\\.com/.+/actions/.+\$${Q}" \
  'does not name one literal owner'
assert_extra_subject_rejected "a matcher under a quoted key" quoted-key \
  "        ${Q}subject${Q}: ${Q}${ORG_ID}actions/[.]github/workflows/publish-app[.]yaml@.+\$${Q}" \
  'can name the repository that hosts the shared publish workflows'
assert_extra_subject_rejected "a subject the guard cannot read as a scalar" double-quoted-other \
  "        subject: \"${ORG_ID}platform/x\"" \
  'could not read the YAML scalar of this subject'
assert_extra_subject_rejected "a subject inside a flow mapping" flow-mapping \
  "      - {issuer: x, subject: ${Q}.*${Q}}" \
  'inside a YAML flow mapping'

assert_extra_subject_accepted "a first-party subject with a grouped alternation" grouped \
  "        subject: ${Q}${ORG_ID}platform/\\.github/workflows/(ci\\.yaml@refs/heads/gh-readonly-queue/main/.+|(cd|dr-rebuild)\\.yaml@refs/heads/main)\$${Q}"
assert_extra_subject_accepted "a subject whose repository name continues as a class" repo-class \
  "        subjectRegex: ${ORG_ID}provider-upjet-[a-z0-9-]+/\\.github/workflows/publish-provider-package\\.yml@refs/tags/v.+\$"
assert_extra_subject_accepted "a repository that only begins like the hosting one" repo-longer \
  "        subject: ${Q}${ORG_ID}actions-runner/\\.github/workflows/cd\\.yaml@refs/tags/v.+\$${Q}"
assert_extra_subject_accepted "a subject naming another owner" other-owner \
  "        subject: ${Q}^https://github\\.com/fluxcd/flux2/\\.github/workflows/release\\.yaml@refs/tags/v.+\$${Q}"

# --- RED: a reference discovered but not validated is reported ---------------
#
# A ninth consumer written in a form the strict pattern misses (a multiline
# `subject: >-`, or a fourth key name) is invisible to validation while the eight
# known subjects still satisfy the floor.

tree="${work_dir}/red-unvalidated"
build_tree "${tree}" "[0-9a-f]{40}"
printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n        someOtherKey: '\''^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@.+$'\''\n' \
  >"${tree}/k8s/subject-9.yaml"
if run_tree "${tree}"; then
  fail "guard passed with an unvalidated shared-publish-workflow reference"
fi
grep -q 'did not validate' "${tree}/stderr" || fail "unvalidated-reference check fired with the wrong message"
ok "reports a reference it discovered but could not validate"

# --- The guard stays scoped: first-party tag signers are NOT its business ----
#
# ksail's own cd.yaml signs by release tag, legitimately and permanently. The
# narrowed allow-list must not reach it — a guard that fires on the correct,
# deployed configuration is one that gets switched off.

tree="${work_dir}/scope"
build_tree "${tree}" "[0-9a-f]{40}"
printf 'spec:\n  attestors:\n    - cosign:\n        keyless:\n          identities:\n            - subjectRegExp: ^https://github\\.com/devantler-tech/ksail/\\.github/workflows/cd\\.yaml@refs/tags/v.+$\n' \
  >"${tree}/k8s/ksail-signer.yaml"
if run_tree "${tree}"; then
  ok "ignores first-party tag signers outside the shared publish workflows"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "guard fired on ksail's legitimate tag signer — it is out of scope"
fi

# --- RED/GREEN: the scalar is read the way YAML reads it ---------------------
#
# A `#` opens a comment only OUTSIDE a quoted scalar. The guard used to strip
# everything from the first whitespace-`#` unconditionally, so a single-quoted
# subject whose value legitimately contains one was truncated and only the
# surviving half was judged. Reproduced before the fix: the first case below was
# ACCEPTED, and the guard reported all eight subjects pinned, while the value
# YAML hands cosign carried a second alternative permitting any branch.
#
# These cases vary the QUOTING rather than the ref, which build_tree cannot do —
# it always emits a well-formed single-quoted scalar.

# build_tree_line <dir> <verbatim-subject-line> — as build_tree, but the first
# subject's whole line is supplied by the caller.
build_tree_line() {
  local dir="$1" line="$2" i
  rm -rf "${dir}"
  mkdir -p "${dir}/scripts" "${dir}/k8s"
  cp "${guard}" "${dir}/scripts/guard-shared-publish-workflow-pin.sh"
  write_matcher_list "${dir}" 1 2 3 4 5 6 7 8
  printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n%s\n' \
    "${line}" >"${dir}/k8s/subject-1.yaml"
  for i in 2 3 4 5 6 7 8; do
    printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n        subject: '\''^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@%s$'\''\n' \
      "${SHA_PATTERN}" >"${dir}/k8s/subject-${i}.yaml"
  done
}

# assert_line_rejected <label> <subject-line> <slug> <expected-stderr-pattern>
#
# The expected pattern is required because the guard's easiest failure is the
# floor, which fires whenever a fixture is escaped wrongly. A bare exit-status
# assertion would pass on a tree the guard never actually read.
assert_line_rejected() {
  local label="$1" line="$2" dir="${work_dir}/red-$3" pattern="$4"
  build_tree_line "${dir}" "${line}"
  if run_tree "${dir}"; then
    fail "guard ACCEPTED ${label} — the value YAML passes to cosign was not what was judged"
  fi
  grep -q "${pattern}" "${dir}/stderr" ||
    fail "guard rejected ${label} but not for the expected reason: $(head -1 "${dir}/stderr")"
  ok "rejects ${label}"
}

assert_line_rejected "a # inside a quoted scalar used to hide a branch alternative" \
  "        subject: ${Q}${SUBJECT_ID}${SHA_PATTERN} # x|${SUBJECT_ID}refs/heads/.+\$${Q}" \
  quotedcomment 'does not pin'

# THE SAME HIDING TRICK WITH THE ALTERNATIVES REVERSED. The case above puts the
# fixed-SHA alternative first and the branch alternative last, so the last-@ read
# lands on `refs/heads/.+` and rejects. Swap them and the last @ lands on a DECOY
# SHA instead: the guard validates that decoy, reports the subject pinned, and the
# FIRST alternative -- the one cosign also honours -- still permits any branch.
# Ordering, not shape, is what the last-@ read is sensitive to, so both orders have
# to be pinned or the fix only covers the spelling that was reported.
assert_line_rejected "a reversed alternation whose LAST @ is a decoy SHA" \
  "        subject: ${Q}${SUBJECT_ID}refs/heads/.+\$| #${SUBJECT_ID}${SHA_PATTERN}\$${Q}" \
  reversedalt 'does not pin'

assert_line_rejected "a double-quoted scalar, whose escapes this guard does not resolve" \
  "        subject: \"${SUBJECT_ID}${SHA_PATTERN}\$\"" \
  doublequoted 'could not read the YAML scalar'

assert_line_rejected "an unterminated quoted scalar" \
  "        subject: ${Q}${SUBJECT_ID}${SHA_PATTERN}\$" \
  unterminated 'could not read the YAML scalar'

assert_line_rejected "an escaped '' inside a single-quoted scalar" \
  "        subject: ${Q}${SUBJECT_ID}${SHA_PATTERN}${Q}${Q}|${SUBJECT_ID}refs/heads/.+\$${Q}" \
  escapedquote 'could not read the YAML scalar'

# GREEN counterpart: a comment OUTSIDE the quotes is a real comment, and removing
# it is correct. Without this the fix could pass every case above by refusing
# every line that contains a `#` at all.
tree="${work_dir}/green-realcomment"
build_tree_line "${tree}" "        subject: ${Q}${SUBJECT_ID}${SHA_PATTERN}\$${Q} # pinned by #2816"
if run_tree "${tree}"; then
  ok "still removes a real comment that follows a quoted scalar"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "guard rejected a legitimate subject carrying a real trailing comment"
fi

# A PLAIN (unquoted) scalar. YAML excludes trailing whitespace from the scalar; a `${v%%[[:space:]]#*}`
# strip does not — with no comment it keeps every trailing space, and before a `#` it removes only the
# one space adjacent to the marker. The leftover whitespace then stops the trailing `$` being stripped,
# so the fixed-SHA alternative fails the whole-line allow-list and the guard blocks a VALID subject.
# Fail-closed, but wrong: these two must be ACCEPTED.

assert_line_accepted() {
  local label="$1" line="$2" dir="${work_dir}/green-$3"
  build_tree_line "${dir}" "${line}"
  if run_tree "${dir}"; then
    ok "accepts ${label}"
  else
    printf '%s\n' "$(cat "${dir}/stderr")" >&2
    fail "guard REJECTED ${label} — a valid pinned subject must not be blocked"
  fi
}

assert_line_accepted "a plain scalar with trailing whitespace" \
  "        subject: ${SUBJECT_ID}${SHA_PATTERN}\$   " \
  plaintrailing

assert_line_accepted "a plain scalar with two spaces before a real comment" \
  "        subject: ${SUBJECT_ID}${SHA_PATTERN}\$  # pinned by #2816" \
  plaintwospace

# A BLOCK SCALAR carries one value across several lines, and this guard reads LINES.
# YAML folds the block into a SINGLE value, so an indented content line that merely
# LOOKS like `subject: <pinned>` is not a key at all -- but the line-oriented grep sees
# it, validates it, and reports the subject pinned. The value cosign actually receives
# is `.*| subject: ...@[0-9a-f]{40}$`, whose FIRST alternative accepts EVERY identity.
#
# MEASURED with gopkg.in/yaml.v3: the fixture below resolves to exactly that one scalar.
# Note the shape is deliberately chosen to carry only ONE workflow URL, so the
# coverage rule's counts still line up -- a decoy repeating the URL in both alternatives
# is already caught there, and pinning only that shape would leave this one open.
#
# The folded value carries two `@`, so the one-identity rule would have caught it; that
# rule never runs, because the guard never assembles the folded value.
#
# Both block styles are pinned: `>-` folds newlines to spaces and `|-` keeps them, and a
# matcher-looking line is equally invisible in either.
assert_line_rejected "a folded block scalar hiding a matcher-looking content line" \
  "        subject: >-
          .*|
          subject: ${SUBJECT_ID}${SHA_PATTERN}\$" \
  foldedblock 'BLOCK SCALAR'

assert_line_rejected "a literal block scalar hiding a matcher-looking content line" \
  "        subject: |-
          .*|
          subject: ${SUBJECT_ID}${SHA_PATTERN}\$" \
  literalblock 'BLOCK SCALAR'

# A block-scalar header may carry its indentation digit and chomping sign IN EITHER
# ORDER. `>-2` and `|+2` are as valid as `>2-` and `|2+` -- all four were measured with
# gopkg.in/yaml.v3 folding an indented decoy line into the value -- so a check keyed on
# digits-then-sign missed the sign-first spellings and the bypass came straight back.
assert_line_rejected "a block scalar whose chomping sign precedes its indentation digit" \
  "        subject: >-2
           .*|
           subject: ${SUBJECT_ID}${SHA_PATTERN}\$" \
  chompfirst 'BLOCK SCALAR'

assert_line_rejected "a literal block scalar with keep-chomping before its indentation digit" \
  "        subject: |+2
           .*|
           subject: ${SUBJECT_ID}${SHA_PATTERN}\$" \
  keepchompfirst 'BLOCK SCALAR'

assert_line_rejected "a block scalar with its indentation digit first" \
  "        subject: >2-
           .*|
           subject: ${SUBJECT_ID}${SHA_PATTERN}\$" \
  indentfirst 'BLOCK SCALAR'

# A comment MAY open immediately after a closing quote, with no whitespace before
# its `#`. YAML requires whitespace before a comment that follows a PLAIN scalar,
# and this guard applied that rule to quoted ones too — but a quoted scalar has
# already ended at its closing quote, so `gopkg.in/yaml.v3` reads `'...'# pinned`
# as the subject plus a comment. Measured against yaml.v3 directly: `'abc'# c`
# parses to `abc`, while `'abc'x` is a parse error. So refusing this shape blocks
# a subject the platform's own parser accepts — fail-closed, and still a defect,
# exactly as the plain-scalar cases above.
assert_line_accepted "a comment opening immediately after the closing quote" \
  "        subject: ${Q}${SUBJECT_ID}${SHA_PATTERN}\$${Q}# pinned by #2816" \
  quotednospacecomment

# --- RED/GREEN: the two-publisher-family subject (#4502) ----------------------
#
# A manifest matcher may accept the canonical manifest workflow beside the legacy one,
# as ONE subject holding one group. That subject carries two `@` by design, which is
# exactly what the single-identity rule above refuses, so the guard reads it with its own
# strict parse. The GREEN cases prove the reviewed arrangement passes; each RED case
# isolates one way a second family could let a moving ref, a second identity or an
# unreviewed arrangement through, and asserts the refusal names that cause — the floor
# and the single-identity rule both fire on a mis-escaped fixture, so an exit status
# alone would prove nothing.

readonly FAMILY_OPEN='^https://github\.com/devantler-tech/('
readonly LEGACY_ID='actions/\.github/workflows/publish-manifests\.yaml@'
readonly CANONICAL_ID='\.github/\.github/workflows/publish-manifests\.yaml@'

# family_line <legacy-ref> <canonical-ref> — a single-quoted two-family subject line.
family_line() {
  printf '        subject: %s%s%s%s|%s%s)$%s' \
    "${Q}" "${FAMILY_OPEN}" "${LEGACY_ID}" "$1" "${CANONICAL_ID}" "$2" "${Q}"
}

assert_line_accepted "a two-family subject with one concrete legacy commit" \
  "$(family_line "${CONCRETE_SHA}" "${CONCRETE_SHA_2}")" \
  family-single

assert_line_accepted "a two-family subject whose legacy ref is an approved set" \
  "$(family_line "(${CONCRETE_SHA}|${CONCRETE_SHA_2})" "${CONCRETE_SHA_2}")" \
  family-set

assert_line_accepted "a two-family subject whose legacy ref is the pattern form" \
  "$(family_line "${SHA_PATTERN}" "${CONCRETE_SHA}")" \
  family-pattern

assert_line_accepted "a two-family subject written as a plain scalar" \
  "        subject: ${FAMILY_OPEN}${LEGACY_ID}${CONCRETE_SHA}|${CANONICAL_ID}${CONCRETE_SHA_2})\$" \
  family-plain

# The canonical family is approved one reviewed commit at a time, so the pattern form —
# accepted for the legacy family — is refused here: it would admit every commit of the
# canonical repository.
assert_line_rejected "a canonical ref in the pattern form" \
  "$(family_line "${CONCRETE_SHA}" "${SHA_PATTERN}")" \
  family-canonical-pattern 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical ref naming a branch" \
  "$(family_line "${CONCRETE_SHA}" 'refs/heads/main')" \
  family-canonical-branch 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical ref naming a bare moving ref" \
  "$(family_line "${CONCRETE_SHA}" 'main')" \
  family-canonical-bare 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical ref that is a wildcard" \
  "$(family_line "${CONCRETE_SHA}" '.+')" \
  family-canonical-wildcard 'does not pin one concrete 40-hex commit'

# Two distinct commits written as one group are accepted (#4713): the commit a canonical
# publisher moves from and the one it moves to. Nothing wider is.
assert_line_accepted "a canonical ref that is a group of two distinct commits" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA}|${CONCRETE_SHA_2})")" \
  family-canonical-set

assert_line_rejected "a canonical group naming the same commit twice" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA_2}|${CONCRETE_SHA_2})")" \
  family-canonical-set-repeated 'names the same commit twice'

assert_line_rejected "a canonical group of three commits" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA}|${CONCRETE_SHA_2}|${CONCRETE_SHA})")" \
  family-canonical-set-three 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical group of one commit" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA_2})")" \
  family-canonical-set-one 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical group holding the pattern form" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA_2}|${SHA_PATTERN})")" \
  family-canonical-set-pattern 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical group holding a wildcard" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA_2}|.*)")" \
  family-canonical-set-wildcard 'does not pin one concrete 40-hex commit'

assert_line_rejected "two canonical commits written without a group" \
  "$(family_line "${CONCRETE_SHA}" "${CONCRETE_SHA}|${CONCRETE_SHA_2}")" \
  family-canonical-set-ungrouped 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical group with a wildcard appended" \
  "$(family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA}|${CONCRETE_SHA_2}).*")" \
  family-canonical-set-suffix 'does not pin one concrete 40-hex commit'

assert_line_rejected "a short canonical commit" \
  "$(family_line "${CONCRETE_SHA}" '0123456')" \
  family-canonical-short 'does not pin one concrete 40-hex commit'

assert_line_rejected "an uppercase canonical commit" \
  "$(family_line "${CONCRETE_SHA}" "$(printf '%s' "${CONCRETE_SHA}" | tr 'a-f' 'A-F')")" \
  family-canonical-upper 'does not pin one concrete 40-hex commit'

assert_line_rejected "a canonical commit with a wildcard appended" \
  "$(family_line "${CONCRETE_SHA}" "${CONCRETE_SHA_2}.*")" \
  family-canonical-suffix 'does not pin one concrete 40-hex commit'

# A third alternative appended INSIDE the group keeps the `)$` ending and both `@`, so
# only reading the canonical ref whole catches it.
assert_line_rejected "a third alternative appended inside the group" \
  "$(family_line "${CONCRETE_SHA}" "${CONCRETE_SHA_2})|(.*")" \
  family-third-alternative 'does not pin one concrete 40-hex commit'

# The legacy family keeps the allow-list it has outside a group.
assert_line_rejected "a two-family subject whose legacy ref is a branch" \
  "$(family_line 'refs/heads/main' "${CONCRETE_SHA}")" \
  family-legacy-branch 'does not pin a fixed revision'

assert_line_rejected "a two-family subject whose legacy ref is a wildcard" \
  "$(family_line '.+' "${CONCRETE_SHA}")" \
  family-legacy-wildcard 'does not pin a fixed revision'

assert_line_rejected "a two-family subject whose legacy set carries a branch" \
  "$(family_line "(${CONCRETE_SHA}|refs/heads/main)" "${CONCRETE_SHA_2}")" \
  family-legacy-setbranch 'does not pin a fixed revision'

assert_line_rejected "a two-family subject whose legacy ref is partially grouped" \
  "$(family_line "(${CONCRETE_SHA})?.+" "${CONCRETE_SHA_2}")" \
  family-legacy-partial 'not a fully grouped'

# Arrangements nobody reviewed. Each still names only pinned commits, which is the point:
# the refusal is about the arrangement, not about a ref the allow-list would catch anyway.
assert_line_rejected "the canonical family written first" \
  "        subject: ${Q}${FAMILY_OPEN}${CANONICAL_ID}${CONCRETE_SHA}|${LEGACY_ID}${CONCRETE_SHA_2})\$${Q}" \
  family-reversed 'not the legacy family followed by the canonical family'

assert_line_rejected "a group holding the legacy family twice" \
  "        subject: ${Q}${FAMILY_OPEN}${LEGACY_ID}${CONCRETE_SHA}|${LEGACY_ID}${CONCRETE_SHA_2})\$${Q}" \
  family-legacy-twice 'not the legacy family followed by the canonical family'

assert_line_rejected "a group naming the canonical family twice" \
  "$(family_line "${CONCRETE_SHA}" "${CONCRETE_SHA_2}|${CANONICAL_ID}${CONCRETE_SHA}")" \
  family-canonical-twice 'where exactly two are expected'

assert_line_rejected "a canonical family repeated without a second ref" \
  "        subject: ${Q}${FAMILY_OPEN}${LEGACY_ID}${CONCRETE_SHA}|\\.github/\\.github/workflows/publish-${CANONICAL_ID}${CONCRETE_SHA_2})\$${Q}" \
  family-canonical-nested 'names a publisher family more than once'

assert_line_rejected "a subject naming only the canonical publisher" \
  "        subject: ${Q}^https://github\\.com/devantler-tech/${CANONICAL_ID}${CONCRETE_SHA}\$${Q}" \
  family-canonical-only 'names only the canonical publisher'

assert_line_rejected "a group that is not closed and anchored" \
  "        subject: ${Q}${FAMILY_OPEN}${LEGACY_ID}${CONCRETE_SHA}|${CANONICAL_ID}${CONCRETE_SHA_2})${Q}" \
  family-unanchored-end 'opens a publisher group but is not exactly'

assert_line_rejected "a group followed by a further alternative" \
  "        subject: ${Q}${FAMILY_OPEN}${LEGACY_ID}${CONCRETE_SHA}|${CANONICAL_ID}${CONCRETE_SHA_2})\$|.*${Q}" \
  family-trailing-alternative 'opens a publisher group but is not exactly'

assert_line_rejected "a group that is not anchored at its start" \
  "        subject: ${Q}https://github\\.com/devantler-tech/(${LEGACY_ID}${CONCRETE_SHA}|${CANONICAL_ID}${CONCRETE_SHA_2})\$${Q}" \
  family-unanchored-start 'opens a publisher group but is not exactly'

assert_line_rejected "a two-family subject with unescaped dots" \
  "        subject: ${Q}^https://github.com/devantler-tech/(actions/.github/workflows/publish-manifests.yaml@${CONCRETE_SHA}|.github/.github/workflows/publish-manifests.yaml@${CONCRETE_SHA_2})\$${Q}" \
  family-bare-dots 'opens a publisher group but is not exactly'

# Both registered publisher workflows use the same strict two-family grammar.
assert_line_accepted "a two-family subject for the application workflow" \
  "        subject: ${Q}${FAMILY_OPEN}actions/\\.github/workflows/publish-app\\.yaml@${CONCRETE_SHA}|\\.github/\\.github/workflows/publish-app\\.yaml@${CONCRETE_SHA_2})\$${Q}" \
  family-app

app_family_line() {
  family_line "$1" "$2" | sed 's/publish-manifests/publish-app/g'
}
assert_line_accepted "application publisher with two reviewed canonical commits" \
  "$(app_family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA}|${CONCRETE_SHA_2})")" family-app-pair
assert_line_rejected "application canonical publisher cannot use a pattern" \
  "$(app_family_line "${CONCRETE_SHA}" "${SHA_PATTERN}")" family-app-pattern 'does not pin one concrete 40-hex commit'
assert_line_rejected "application canonical publisher cannot use a branch" \
  "$(app_family_line "${CONCRETE_SHA}" 'refs/heads/main')" family-app-branch 'does not pin one concrete 40-hex commit'
assert_line_rejected "application canonical publisher cannot repeat a commit" \
  "$(app_family_line "${CONCRETE_SHA}" "(${CONCRETE_SHA}|${CONCRETE_SHA})")" family-app-repeat 'names the same commit twice'

assert_line_rejected "two families naming different workflows" \
  "        subject: ${Q}${FAMILY_OPEN}actions/\\.github/workflows/publish-app\\.yaml@${CONCRETE_SHA}|${CANONICAL_ID}${CONCRETE_SHA_2})\$${Q}" \
  family-mixed-workflows 'name different workflows'

# THE GENERIC VERIFIERS ARE CANONICAL-ONLY (#6264). The legacy publisher repository is
# deleted, so in the three generic files the guard accepts only the canonical-only form and
# refuses the legacy family in every spelling it selects: the former pattern rule, an exact
# legacy commit, and the former two-family subject. Outside those files the canonical-only
# form stays refused (family-canonical-only above).
readonly APP_CANONICAL_ONLY="^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-app\\.yaml@"
readonly APP_LEGACY_ONLY="^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@"

# generic_case <accept|reject> <label> <generic-path> <subject-line> <slug> [stderr-pattern]
# The ordinary tree plus ONE generic verifier at its production path, named in the floor.
generic_case() {
  local expect="$1" label="$2" path="$3" line="$4" dir="${work_dir}/generic-$5" pattern="${6:-}"
  build_tree "${dir}" "${SHA_PATTERN}"
  mkdir -p "${dir}/$(dirname "${path}")"
  printf 'rules:\n  - keyless:\n%s\n' "${line}" >"${dir}/${path}"
  printf '1\t%s\n' "${path}" >>"${dir}/scripts/shared-publish-workflow-matchers.tsv"
  if [ "${expect}" = accept ]; then
    run_tree "${dir}" || fail "guard rejected ${label}: $(head -1 "${dir}/stderr")"
    ok "accepts ${label}"
    return
  fi
  if run_tree "${dir}"; then
    fail "guard ACCEPTED ${label} in a generic verifier"
  fi
  grep -q -- "${pattern}" "${dir}/stderr" ||
    fail "guard rejected ${label} but not for the expected reason: $(head -1 "${dir}/stderr")"
  ok "rejects ${label}"
}

readonly GENERIC_TALOS='talos/cluster/verify-first-party-images.yaml'
readonly GENERIC_KYVERNO='k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml'
readonly GENERIC_TENANT='k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml'
readonly NOT_GENERIC='only the maintained publisher'
# The Tenant graph names no shared publisher at all (ADR 0019).
readonly NOT_TENANT='may name no shared publisher'

generic_case accept "a canonical-only node-pull rule naming two reviewed commits" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_ONLY}(${CONCRETE_SHA}|${CONCRETE_SHA_2})\$" talos-pair
generic_case accept "a canonical-only admission rule naming one reviewed commit" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_ONLY}${CONCRETE_SHA}\$" kyverno-single
generic_case reject "the deleted publisher at ANY commit in the node-pull rule" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_LEGACY_ONLY}${SHA_PATTERN}\$" talos-legacy-pattern "${NOT_GENERIC}"
generic_case reject "the deleted publisher at one exact commit in the node-pull rule" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_LEGACY_ONLY}${CONCRETE_SHA}\$" talos-legacy-exact "${NOT_GENERIC}"
generic_case reject "the former two-family admission rule with the deleted publisher at any commit" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-app\\.yaml@${SHA_PATTERN}|\\.github/\\.github/workflows/publish-app\\.yaml@(${CONCRETE_SHA}|${CONCRETE_SHA_2}))\$" \
  kyverno-two-family "${NOT_GENERIC}"
generic_case reject "a plain subject naming the deleted publisher in the tenant graph" "${GENERIC_TENANT}" \
  "        subject: ${Q}${APP_LEGACY_ONLY}${CONCRETE_SHA}\$${Q}" tenant-legacy "${NOT_TENANT}"
generic_case reject "a canonical-only generic rule that admits every maintained commit" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_ONLY}${SHA_PATTERN}\$" talos-canonical-pattern 'does not pin one concrete 40-hex commit'
generic_case reject "a canonical-only generic rule on a branch" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_ONLY}refs/heads/main\$" kyverno-canonical-branch 'does not pin one concrete 40-hex commit'
generic_case reject "a canonical-only generic rule with a second identity appended" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_ONLY}${CONCRETE_SHA}\$|${APP_LEGACY_ONLY}${CONCRETE_SHA_2}\$" talos-appended "\"@\" separators"

# A PRODUCT'S OWN RELEASE WORKFLOW ON A RELEASE TAG (#6265). One further alternative, written
# last in the same group, with a tail compared literally and a repository segment fixed per
# verifier: the first-party segment in the image verifiers and one literal repository in a
# consumer's matcher. The Tenant source names only the tenant's own repository, built in CEL
# (exercised through the real graph by test-tenant-source-constructors.sh).
readonly OWN_TAIL='/\.github/workflows/[^/]+\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
readonly OWN_BRANCH_TAIL='/\.github/workflows/[^/]+\.ya?ml@refs/heads/main'
readonly OWN_ANY_PATH_TAIL='/\.github/workflows/.+\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
readonly FIRST_PARTY='[A-Za-z0-9][A-Za-z0-9_.-]*'
readonly APP_CANONICAL_GROUP='^https://github\.com/devantler-tech/(\.github/\.github/workflows/publish-app\.yaml@'

generic_case accept "an admission rule grouping two reviewed commits with a first-party release" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_GROUP}(${CONCRETE_SHA}|${CONCRETE_SHA_2})|${FIRST_PARTY}${OWN_TAIL})\$" kyverno-own
generic_case accept "a node-pull rule grouping one reviewed commit with a first-party release" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_TAIL})\$" talos-own
generic_case reject "a first-party release segment that admits the .github repository" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|[A-Za-z0-9_.-]+${OWN_TAIL})\$" kyverno-own-dotgithub "${NOT_GENERIC}"
generic_case reject "a first-party release segment that admits a slash" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|[A-Za-z0-9][A-Za-z0-9_./-]*${OWN_TAIL})\$" talos-own-slash "${NOT_GENERIC}"
generic_case reject "a first-party release on a branch" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_BRANCH_TAIL})\$" talos-own-branch "${NOT_GENERIC}"
generic_case reject "a first-party release from any workflow path" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_ANY_PATH_TAIL})\$" kyverno-own-path "${NOT_GENERIC}"
generic_case reject "a first-party release whose tag may carry a suffix" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_TAIL}.*)\$" kyverno-own-suffix "${NOT_GENERIC}"
generic_case reject "the Tenant marker in an image verifier" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|<tenant>${OWN_TAIL})\$" talos-own-marker "${NOT_GENERIC}"
generic_case reject "a first-party release in the Tenant graph's plain subject" "${GENERIC_TENANT}" \
  "        subject: ${Q}${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_TAIL})\$${Q}" tenant-own-plain "${NOT_TENANT}"
# Written first, the shared publisher is no longer where the guard selects it, so the floor
# refuses the file outright.
generic_case reject "a first-party release written before the shared publisher" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ^https://github\\.com/devantler-tech/(${FIRST_PARTY}${OWN_TAIL}|\\.github/\\.github/workflows/publish-app\\.yaml@${CONCRETE_SHA})\$" kyverno-own-first 'holds 0 shared-publish-workflow matcher'
generic_case reject "a grouped generic rule that admits every maintained commit" "${GENERIC_TALOS}" \
  "      subjectRegex: ${APP_CANONICAL_GROUP}${SHA_PATTERN}|${FIRST_PARTY}${OWN_TAIL})\$" talos-own-pattern 'does not pin one concrete 40-hex commit'
generic_case reject "a first-party release alternative written twice" "${GENERIC_KYVERNO}" \
  "      subjectRegExp: ${APP_CANONICAL_GROUP}${CONCRETE_SHA}|${FIRST_PARTY}${OWN_TAIL}|${FIRST_PARTY}${OWN_TAIL})\$" kyverno-own-twice "\"@\" separators"

# own_line <repository-text> [tail] — an application two-family subject followed by that
# consumer's own release alternative.
own_line() {
  printf '        subject: %s%sactions/\\.github/workflows/publish-app\\.yaml@%s|\\.github/\\.github/workflows/publish-app\\.yaml@%s|%s%s)$%s' \
    "${Q}" "${FAMILY_OPEN}" "${CONCRETE_SHA}" "${CONCRETE_SHA_2}" "$1" "${2:-${OWN_TAIL}}" "${Q}"
}
assert_line_accepted "a consumer matcher naming its own repository's release workflow" "$(own_line wedding-app)" family-own
assert_line_accepted "a consumer matcher whose own repository has an escaped dot" "$(own_line 'wedding\.app')" family-own-dot
assert_line_rejected "a consumer own release naming a class of repositories" "$(own_line '[a-z-]+')" \
  family-own-class 'does not name one literal repository'
assert_line_rejected "a consumer own release with an unescaped dot" "$(own_line 'wedding.app')" \
  family-own-bare-dot 'does not name one literal repository'
assert_line_rejected "a consumer own release naming the .github repository" "$(own_line '\.github')" \
  family-own-dotgithub 'does not name one literal repository'
assert_line_rejected "a consumer own release naming the deleted legacy publisher repository" "$(own_line actions)" \
  family-own-actions 'hosted the shared publishers'
assert_line_rejected "a consumer own release naming two repositories" "$(own_line 'wedding-app|ascoachingogvaner')" \
  family-own-two 'does not pin one concrete 40-hex commit'
assert_line_rejected "a consumer own release on a branch" "$(own_line wedding-app "${OWN_BRANCH_TAIL}")" \
  family-own-branch 'where exactly two are expected'

# Discovery is independent of the subject pattern, and it has to see the canonical family
# too: a canonical reference under a key the guard does not validate is reported, not
# skipped, exactly as a legacy one is.
tree="${work_dir}/red-family-undiscovered"
build_tree "${tree}" "${SHA_PATTERN}"
printf 'spec:\n  verify:\n    matchOIDCIdentity:\n      - issuer: x\n        identity: '\''^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-manifests\\.yaml@.+$'\''\n' \
  >"${tree}/k8s/canonical-other-key.yaml"
if run_tree "${tree}"; then
  fail "guard ACCEPTED a canonical-publisher reference it never validated"
fi
grep -q 'did not validate' "${tree}/stderr" ||
  fail "guard rejected an unvalidated canonical reference but not for the discovery reason: $(head -1 "${tree}/stderr")"
ok "reports a canonical-publisher reference under a key it does not validate"

# --- RED: an alternation over refs must be grouped ------------------------------
#
# `@A|B$` is not two refs: it reads as `^…@A` OR `B$`, and the second half is anchored to
# nothing before it. Splitting the ref on `|` found two well-formed alternatives and
# accepted a subject whose second half matches ANY identity ending in a commit.

assert_rejected_grouping() { # <label> <subject-line> <slug>
  assert_line_rejected "$1" "$2" "$3" 'not a fully grouped'
}

assert_rejected_grouping "an ungrouped alternation of a commit and the commit pattern" \
  "        subject: ${Q}${SUBJECT_ID}${CONCRETE_SHA}|${SHA_PATTERN}\$${Q}" \
  ungrouped-pattern

assert_rejected_grouping "an ungrouped alternation of two commits" \
  "        subject: ${Q}${SUBJECT_ID}${CONCRETE_SHA}|${CONCRETE_SHA_2}\$${Q}" \
  ungrouped-commits

assert_rejected_grouping "a two-family subject whose legacy refs are not grouped" \
  "$(family_line "${CONCRETE_SHA}|${CONCRETE_SHA_2}" "${CONCRETE_SHA}")" \
  family-legacy-ungrouped

# --- RED: the scalar judged is the identity the line was selected for -----------
#
# The subject pattern selects a LINE and matches anywhere on it. Each case below is
# selected by text that is not the value's identity — a comment, or the tail of an
# alternation — while the value itself carries one `@` and a well-formed ref, which is all
# the guard used to look at. cosign receives the value, not the comment.

assert_rejected_identity() { # <label> <subject-line> <slug>
  assert_line_rejected "$1" "$2" "$3" 'subject is not exactly'
}

assert_rejected_identity "a wildcard identity selected by a comment naming the workflow" \
  "        subject: ${Q}.*@${SHA_PATTERN}${Q} # subject: ${SUBJECT_ID}" \
  identity-comment

assert_rejected_identity "a wildcard alternative ahead of a workflow-shaped tail" \
  "        subject: ${Q}.*|subject: ${SUBJECT_ID}${SHA_PATTERN}\$${Q}" \
  identity-tail

assert_rejected_identity "a canonical-only identity in another spelling, selected by a comment" \
  "        subject: ${Q}^https://github\\.com/devantler-tech/[.]github/[.]github/workflows/publish-manifests[.]yaml@${SHA_PATTERN}\$${Q} # subject: ${SUBJECT_ID}x" \
  identity-canonical-comment

assert_rejected_identity "a subject with no closing anchor" \
  "        subject: ${Q}${SUBJECT_ID}${CONCRETE_SHA}${Q}" \
  identity-no-end-anchor

assert_rejected_identity "a subject with no opening anchor" \
  "        subject: ${Q}${SUBJECT_ID#^}${CONCRETE_SHA}\$${Q}" \
  identity-no-start-anchor

assert_rejected_identity "a subject with unescaped dots" \
  "        subject: ${Q}^https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@${CONCRETE_SHA}\$${Q}" \
  identity-bare-dots

assert_rejected_identity "a subject naming another publish workflow" \
  "        subject: ${Q}^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml.*@${CONCRETE_SHA}\$${Q} # subject: ${SUBJECT_ID}" \
  identity-workflow-suffix

# --- Integration: the real repository satisfies the narrowed guard -----------

if (cd "${root_dir}" && bash "${guard}" >/dev/null 2>"${work_dir}/real.stderr"); then
  ok "the real repository passes the narrowed guard"
else
  printf '%s\n' "$(cat "${work_dir}/real.stderr")" >&2
  fail "the real repository does not satisfy the narrowed guard"
fi

# Belt and braces: assert directly that no shared-publisher alternative still carries a
# tag ref, independently of the guard's own parsing. If the guard's pattern ever stops
# matching these lines, the check above passes vacuously while this one still sees the
# file contents.
#
# A product's own release alternative (#6265) is a tag by design: its tail is the exact
# OWN_TAIL text, which no shared-publisher ref can equal. Only that exact text is removed
# from a line before the same pattern judges what is left. The own-release alternatives
# themselves are probed against the shipped files below.
readonly SHARED_TAG_RE='workflows/publish-(app|manifests)\\?\.yaml@[^'\''"]*refs/tags'
: >"${work_dir}/leftover"
while IFS= read -r hit; do
  if [[ "${hit//"${OWN_TAIL}"/}" =~ ${SHARED_TAG_RE} ]]; then
    printf '%s\n' "${hit}" >>"${work_dir}/leftover"
  fi
done < <(grep -rnE "${SHARED_TAG_RE}" --include='*.yaml' "${root_dir}" 2>/dev/null || true)
if [ -s "${work_dir}/leftover" ]; then
  printf '%s\n' "$(cat "${work_dir}/leftover")" >&2
  fail "a shared-publisher alternative accepts a tag ref"
fi
ok "no shared-publisher alternative accepts a tag ref; tags reach only the fixed own-release alternatives"

# What the shipped image rules accept at a tag (#6265). Their own-release alternative
# accepts any first-party repository's release workflow on a release tag except `.github`,
# which hosts reusable workflows other repositories call. A repository named `actions`
# would be a first-party repository like any other, so its release tag is accepted; the
# deleted publisher's commit-pinned identities stay refused.
while IFS=$'\t' read -r label file selector; do
  shipped="$(yq -er "${selector}" "${root_dir}/${file}")" || fail "${label}: could not read the shipped subject"
  while IFS=$'\t' read -r expected identity; do
    result=reject
    if printf '%s\n' "${identity}" | grep -Eq -- "${shipped}"; then result=accept; fi
    [ "${result}" = "${expected}" ] || fail "shipped ${label} rule: expected ${expected} for ${identity}, got ${result}"
    ok "shipped ${label} rule: ${expected} ${identity}"
  done <<'IDENTITIES'
accept	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@refs/tags/v1.2.3
accept	https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@refs/tags/v5.4.0
accept	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@b3783a5148b844b3f1f4011cdc84871ad66348c1
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v1.2.3
reject	https://github.com/devantler-tech/.github/.github/workflows/release.yaml@refs/tags/v1.2.3
IDENTITIES
done <<'RULES'
admission	k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml	.spec.attestors[] | select(.name == "publishapp").cosign.keyless.identities[0].subjectRegExp
node-pull	talos/cluster/verify-first-party-images.yaml	.rules[] | select(.image == "ghcr.io/devantler-tech/*").keyless.subjectRegex
RULES
# The removal above is exact: a shared-publisher tag written beside a product's own
# release alternative is still found.
probe="\\.github/\\.github/workflows/publish-app\\.yaml@(${CONCRETE_SHA}|refs/tags/v1)|wedding-app${OWN_TAIL})\$"
[[ "${probe//"${OWN_TAIL}"/}" =~ ${SHARED_TAG_RE} ]] ||
  fail "removing the own-release tail hid a shared-publisher tag ref"
ok "a shared-publisher tag ref beside an own-release alternative is still found"

# --- Wiring: the guard is actually reached, in all three workflows -----------
#
# Match the STEP, never the string. `ci.yaml` names this guard twice — once as a
# paths-filter entry and once as the step that runs it — so a substring test is
# satisfied by the filter entry alone. Deleting the actual invocation would then
# leave this file reporting every assertion green while no pull request executes
# the guard at all: the exact vacuous pass this suite exists to prevent, and one
# the filter entry added in this same change introduced.
#
# So ask the workflow's structure what it will RUN, via each job's steps, rather
# than asking the file what text it contains.

# guard_steps <workflow-file> — one line per step that invokes the guard,
# formatted `<job>|<step-if>|<job-if>`. Empty output means nothing runs it.
guard_steps() {
  yq e -o=json '.jobs // {}' "$1" 2>/dev/null | jq -r '
    to_entries[]
    | .key as $job
    | (.value.if // "") as $jobif
    | (.value.steps // [])[]
    | select((.run // "") | test("scripts/guard-shared-publish-workflow-pin\\.sh"))
    | [$job, (.if // ""), $jobif] | join("|")
  '
}

for wf in ci cd validate-main; do
  file="${root_dir}/.github/workflows/${wf}.yaml"
  [ -f "${file}" ] || fail "expected workflow ${file} to exist"
  steps="$(guard_steps "${file}")"
  [ -n "${steps}" ] ||
    fail "${wf}.yaml has no step whose run: invokes the guard — it would protect nothing there"
  ok "${wf}.yaml runs the guard ($(printf '%s\n' "${steps}" | wc -l | tr -d ' ') step(s))"
done

# The guard must NOT sit behind a paths filter: its subjects live in k8s/, talos/
# and the tenant RGD, so a PR touching only one of those trees must still run it.
# Check the step's own `if:` AND its job's — either one gates it.
while IFS='|' read -r job stepif jobif; do
  [ -n "${job}" ] || continue
  case "${stepif}${jobif}" in
    *needs.changes*)
      fail "the guard is gated on a paths filter in ci.yaml (job ${job}: step-if='${stepif}' job-if='${jobif}'); it must run unconditionally"
      ;;
  esac
done <<EOF
$(guard_steps "${root_dir}/.github/workflows/ci.yaml")
EOF
ok "the guard runs unconditionally in ci.yaml"

printf '\n%d assertion(s) passed.\n' "${pass_count}"
