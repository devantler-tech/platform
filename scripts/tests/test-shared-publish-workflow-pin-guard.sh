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

# These subjects are cosign identity REGEXES, so the accepted ref is the pattern
# `[0-9a-f]{40}` as literal text — not a concrete commit. A hand-written literal
# SHA is a different thing (an approved-revision allow-list, #2818's option 2) and
# the guard does not recognise it today.
readonly SHA_PATTERN='[0-9a-f]{40}'

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

# build_tree <dir> <ref-for-subject-1> — writes eight subjects, the first carrying
# the supplied ref and the remaining seven a plain 40-hex pin. One varying subject
# is what isolates the ref check from the floor check.
build_tree() {
  local dir="$1" first_ref="$2" i
  rm -rf "${dir}"
  mkdir -p "${dir}/scripts" "${dir}/k8s"
  cp "${guard}" "${dir}/scripts/guard-shared-publish-workflow-pin.sh"

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

# --- GREEN: the narrowed shape is what the guard accepts ---------------------

tree="${work_dir}/green"
build_tree "${tree}" "[0-9a-f]{40}"
if run_tree "${tree}"; then
  ok "eight SHA-only subjects pass"
else
  printf '%s\n' "$(cat "${tree}/stderr")" >&2
  fail "eight SHA-only subjects should pass, guard rejected them"
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

# --- RED: the floor fails closed on a shrunken match set ---------------------
#
# An empty or reduced result from a filtered read is a claim about the FILTER. If
# the subjects move to a key spelling the pattern does not know, the grep returns
# less and the guard would otherwise report a clean repository having checked
# nothing.

tree="${work_dir}/red-floor"
build_tree "${tree}" "[0-9a-f]{40}"
rm "${tree}/k8s/subject-8.yaml"
if run_tree "${tree}"; then
  fail "guard passed with only seven subjects — the floor did not fail closed"
fi
grep -q 'expected at least' "${tree}/stderr" || fail "floor fired but with the wrong message"
ok "fails closed when fewer subjects are found than expected"

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

readonly SUBJECT_ID='^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@'
readonly Q="'"

# build_tree_line <dir> <verbatim-subject-line> — as build_tree, but the first
# subject's whole line is supplied by the caller.
build_tree_line() {
  local dir="$1" line="$2" i
  rm -rf "${dir}"
  mkdir -p "${dir}/scripts" "${dir}/k8s"
  cp "${guard}" "${dir}/scripts/guard-shared-publish-workflow-pin.sh"
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

# --- Integration: the real repository satisfies the narrowed guard -----------

if (cd "${root_dir}" && bash "${guard}" >/dev/null 2>"${work_dir}/real.stderr"); then
  ok "the real repository passes the narrowed guard"
else
  printf '%s\n' "$(cat "${work_dir}/real.stderr")" >&2
  fail "the real repository does not satisfy the narrowed guard"
fi

# Belt and braces: assert directly that no shared-publish subject still carries a
# tag alternative, independently of the guard's own parsing. If the guard's
# pattern ever stops matching these lines, the check above passes vacuously while
# this one still sees the file contents.
if grep -rnE 'workflows/publish-(app|manifests)\\?\.yaml@[^'\''"]*refs/tags' \
  --include='*.yaml' "${root_dir}" >"${work_dir}/leftover" 2>/dev/null; then
  printf '%s\n' "$(cat "${work_dir}/leftover")" >&2
  fail "a shared-publish-workflow subject still accepts a tag ref"
fi
ok "no shared-publish-workflow subject accepts a tag ref"

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
