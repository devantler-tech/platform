#!/usr/bin/env bash
# RED/GREEN coverage for the canonical-publisher family (#4502): the fixed approvals record,
# the two-family matcher the guard accepts, and the two-family matcher the writer renders.
#
# WHAT IS ACTUALLY BEING PROVED
# A matcher that accepts a second repository still verifies SOMETHING, so nothing downstream
# notices one that accepts too much. Each refusal case isolates one way the canonical family
# could be wider, vaguer or differently placed than its reviewed record, and asserts the
# refusal names the cause rather than merely exiting non-zero. The controls prove the other
# direction: a legacy-only tree with an empty record still passes, and the writer still
# renders the legacy subject byte for byte.
#
# THE SEAMS
# APPROVED_REVISIONS_FILE, CANONICAL_APPROVALS_FILE and PUBLISH_CONSUMER_ROOT point at a
# synthetic tree built here. Discovery, attribution and the registered consumer list are REAL.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GUARD="$REPO_ROOT/scripts/guard-publish-workflow-approved-revisions.sh"
readonly WRITER="$REPO_ROOT/scripts/write-publish-workflow-matchers.sh"
readonly REPORT="$REPO_ROOT/scripts/report-publish-workflow-signing-revisions.sh"

readonly PATTERN='[0-9a-f]{40}'
readonly SHA_A='1111111111111111111111111111111111111111'  # applied signer
readonly SHA_B='2222222222222222222222222222222222222222'  # default-branch pin
readonly SHA_E='5555555555555555555555555555555555555555'  # release candidate
readonly SHA_K='6666666666666666666666666666666666666666'  # reviewed canonical commit
readonly SHA_X='7777777777777777777777777777777777777777'  # a commit nobody approved
readonly DIGEST='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
readonly CANONICAL_REPOSITORY='devantler-tech/.github'
readonly LEGACY_SET="($SHA_A|$SHA_B|$SHA_E)"
readonly GENERICS=(
  'k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml'
  'k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml'
  'talos/cluster/verify-first-party-images.yaml'
)

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

consumers="$("$REPORT" --list-consumers 2>/dev/null | cut -f1,2 || true)"
[ -n "$consumers" ] || { fail '--list-consumers produced nothing; every case below would pass vacuously'; exit 1; }
# Two manifest consumers and one application consumer, taken from the real registry.
manifest_consumer="$(printf '%s\n' "$consumers" | awk -F'\t' '$2 == "publish-manifests" && $1 !~ /^\./ {print $1; exit}')"
other_manifest_consumer="$(printf '%s\n' "$consumers" | awk -F'\t' -v skip="$manifest_consumer" '$2 == "publish-manifests" && $1 != skip {print $1; exit}')"
app_consumer="$(printf '%s\n' "$consumers" | awk -F'\t' '$2 == "publish-app" {print $1; exit}')"
if [ -z "$manifest_consumer" ] || [ -z "$other_manifest_consumer" ] || [ -z "$app_consumer" ]; then
  fail 'need two publish-manifests consumers and one publish-app consumer in the registry'
  exit 1
fi

package_for() {
  case "$1" in
    .github) printf '%s\n' 'github-config' ;;
    *) printf '%s\n' "$1" ;;
  esac
}
manifest_path() { printf 'k8s/bases/apps/%s/oci-repository.yaml\n' "$(package_for "$1")"; }

legacy_subject() { # <workflow> <ref>
  printf '^https://github\\.com/devantler-tech/actions/\\.github/workflows/%s\\.yaml@%s$' "$1" "$2"
}
family_subject() { # <workflow> <legacy-ref> <canonical-ref>
  printf '^https://github\\.com/devantler-tech/(actions/\\.github/workflows/%s\\.yaml@%s|\\.github/\\.github/workflows/%s\\.yaml@%s)$' \
    "$1" "$2" "$1" "$3"
}

# write_consumer <root> <repo> <subject> — one OCIRepository carrying exactly that subject.
write_consumer() {
  local root="$1" repo="$2" subject="$3" dir
  dir="$root/k8s/bases/apps/$(package_for "$repo")"
  mkdir -p "$dir"
  cat >"$dir/oci-repository.yaml" <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: $(package_for "$repo")
spec:
  url: oci://ghcr.io/devantler-tech/$(package_for "$repo")/manifests
  ref:
    semver: '>=1.0.0'
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\\.actions\\.githubusercontent\\.com\$'
        subject: '$subject'
EOF
}

# build_tree <name> — every consumer on its legacy generated set, the generics on the pattern
# form, the approved set, and a canonical record that approves nothing.
build_tree() {
  local root="$WORK/$1" repo workflow generic
  rm -rf "$root"
  mkdir -p "$root/scripts"
  printf 'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\trelease_candidate_sha\tobserved_on\n' >"$root/scripts/approved.tsv"
  while IFS=$'\t' read -r repo workflow; do
    [ -n "$repo" ] || continue
    write_consumer "$root" "$repo" "$(legacy_subject "$workflow" "$LEGACY_SET")"
    printf '%s\t%s\t1.0.0\t%s\t%s\t%s\t%s\t2026-10-05\n' "$repo" "$workflow" "$DIGEST" "$SHA_A" "$SHA_B" "$SHA_E" >>"$root/scripts/approved.tsv"
  done <<<"$consumers"
  for generic in "${GENERICS[@]}"; do
    mkdir -p "$root/$(dirname "$generic")"
    printf 'rules:\n  - subjectRegex: %s\n' "$(legacy_subject publish-app "$PATTERN")" >"$root/$generic"
  done
  write_record "$root"
  printf '%s\n' "$root"
}

# write_record <root> [<row>...] — the canonical approvals, one row per argument.
write_record() {
  local root="$1" row
  shift
  printf 'consumer\tworkflow\trepository\tcommit\n' >"$root/scripts/canonical.tsv"
  for row in "$@"; do printf '%s\n' "$row" >>"$root/scripts/canonical.tsv"; done
}
record_row() { printf '%s\tpublish-manifests\t%s\t%s' "$1" "$CANONICAL_REPOSITORY" "$2"; }

ENFORCE=1  # the cases below run enforcing unless a block says otherwise
run_in() { # <root> <script> — against the fixture, in the current switch state
  APPROVED_REVISIONS_FILE="$1/scripts/approved.tsv" CANONICAL_APPROVALS_FILE="$1/scripts/canonical.tsv" \
    PUBLISH_CONSUMER_ROOT="$1" APPROVED_REVISIONS_ENFORCE="$ENFORCE" bash "$2" 2>&1
}

expect_pass() { # <case> <root>
  local out
  if out="$(run_in "$2" "$GUARD")"; then pass "$1"; else fail "$1 — expected exit 0:"; printf '%s\n' "$out" >&2; fi
}
expect_refusal() { # <case> <root> <must-mention>...
  local case_name="$1" root="$2" out needle
  shift 2
  if out="$(run_in "$root" "$GUARD")"; then
    fail "$case_name — expected a refusal, guard exited 0:"; printf '%s\n' "$out" >&2
    return
  fi
  for needle in "$@"; do
    case "$out" in
      *"$needle"*) ;;
      *) fail "$case_name — refusal does not name '$needle':"; printf '%s\n' "$out" >&2; return ;;
    esac
  done
  pass "$case_name"
}

# ── controls ──────────────────────────────────────────────────────────────────────────────
root="$(build_tree control)"
expect_pass 'control: a legacy-only tree with a record that approves nothing passes' "$root"

# ── the record, read strictly ─────────────────────────────────────────────────────────────
root="$(build_tree record-missing)"
rm "$root/scripts/canonical.tsv"
expect_refusal 'a missing record is refused, never read as no approvals' "$root" 'canonical approvals not found'

root="$(build_tree record-empty)"
: >"$root/scripts/canonical.tsv"
expect_refusal 'an empty record is refused' "$root" 'canonical approvals'

root="$(build_tree record-header)"
printf 'consumer\tworkflow\tcommit\n' >"$root/scripts/canonical.tsv"
expect_refusal 'a record with another header is refused' "$root" 'canonical approvals header'

root="$(build_tree record-family)"
write_record "$root" "$(printf '%s\tpublish-manifests\tdevantler-tech/elsewhere\t%s' "$manifest_consumer" "$SHA_K")"
expect_refusal 'an unknown publisher family is refused by name' "$root" 'devantler-tech/elsewhere' 'not a known publisher family'

root="$(build_tree record-legacy-family)"
write_record "$root" "$(printf '%s\tpublish-manifests\tdevantler-tech/actions\t%s' "$manifest_consumer" "$SHA_K")"
expect_refusal 'the legacy repository is not a canonical family' "$root" 'not a known publisher family'

for floating in main v7.1.2 refs/heads/main '[0-9a-f]{40}' "${SHA_K%?}" "${SHA_K}0" 'ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD' 'abcdefabcdefabcdefabcdefabcdefabcdefabcG'; do
  root="$(build_tree record-floating)"
  write_record "$root" "$(record_row "$manifest_consumer" "$floating")"
  expect_refusal "a commit of '$floating' is refused as not a 40-hex commit" "$root" "$manifest_consumer" 'is not a 40-hex commit'
done

root="$(build_tree record-duplicate)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
expect_refusal 'a second canonical commit for one consumer is refused' "$root" "$manifest_consumer" 'at most one canonical commit'

root="$(build_tree record-app-consumer)"
write_record "$root" "$(record_row "$app_consumer" "$SHA_K")"
expect_refusal 'an application-image consumer cannot accept the canonical publisher' "$root" "$app_consumer" 'publish-app'

root="$(build_tree record-app-workflow)"
write_record "$root" "$(printf '%s\tpublish-app\t%s\t%s' "$manifest_consumer" "$CANONICAL_REPOSITORY" "$SHA_K")"
expect_refusal 'the canonical application workflow is not approved' "$root" 'is not the canonical manifest workflow'

root="$(build_tree record-unregistered)"
write_record "$root" "$(record_row nobody "$SHA_K")"
expect_refusal 'an unregistered consumer is refused' "$root" 'nobody' 'not a registered consumer'

root="$(build_tree record-extra-field)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"$'\t'"$SHA_X"
expect_refusal 'a fifth field is refused' "$root" 'exactly four fields'

root="$(build_tree record-short)"
write_record "$root" "$(printf '%s\tpublish-manifests\t%s' "$manifest_consumer" "$SHA_K")"
expect_refusal 'a missing field is refused' "$root" 'exactly four fields'

root="$(build_tree record-empty-field)"
write_record "$root" "$(printf '%s\tpublish-manifests\t\t%s' "$manifest_consumer" "$SHA_K")"
expect_refusal 'an empty field is refused' "$root" 'empty field'

root="$(build_tree record-blank-line)"
write_record "$root" '' "$(record_row "$manifest_consumer" "$SHA_K")"
expect_refusal 'a blank line is refused, never skipped' "$root" 'exactly four fields'

# A final row with no trailing newline must still be read: dropping it would turn a present
# approval into an absent one, and this tree would then pass.
root="$(build_tree record-no-newline)"
printf 'consumer\tworkflow\trepository\tcommit\n%s' "$(record_row "$manifest_consumer" "$SHA_K")" >"$root/scripts/canonical.tsv"
expect_refusal 'a final row without a trailing newline is still read' "$root" "$manifest_consumer" "$SHA_K" 'does not accept the canonical publisher'

# ── the guard, both families ──────────────────────────────────────────────────────────────
root="$(build_tree family-agrees)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
expect_pass 'a two-family matcher naming the recorded commit and the legacy set passes' "$root"

root="$(build_tree family-both)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$other_manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "($SHA_E|$SHA_B|$SHA_A)" "$SHA_X")"
expect_pass 'each manifest consumer carries its own recorded commit, legacy set in any order' "$root"

root="$(build_tree family-unrecorded)"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
expect_refusal 'a canonical family the record does not approve is refused' "$root" "$manifest_consumer" "$SHA_K" 'record none'

root="$(build_tree family-other-commit)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_X")"
expect_refusal 'a canonical commit other than the recorded one is refused, naming both' "$root" "$manifest_consumer" "$SHA_X" "$SHA_K"

root="$(build_tree family-wrong-consumer)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
expect_refusal "one consumer's approval does not cover another" "$root" "$other_manifest_consumer" 'record none'

root="$(build_tree family-legacy-foreign)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "($SHA_A|$SHA_B|$SHA_X)" "$SHA_K")"
expect_refusal 'the legacy set is still judged inside a two-family matcher' "$root" "$manifest_consumer" "$SHA_X" 'not the generated set'

root="$(build_tree family-legacy-dropped)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "($SHA_A|$SHA_B)" "$SHA_K")"
expect_refusal 'adding the canonical family may not drop a legacy approval' "$root" "$manifest_consumer" 'not the generated set'

root="$(build_tree family-legacy-pattern)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K")"
expect_refusal 'the pattern form stays refused under enforcement inside a two-family matcher' "$root" "$manifest_consumer" 'pattern form'

for floating in "$PATTERN" '.+' 'refs/heads/main' "($SHA_K|$SHA_X)" "$SHA_K|$SHA_X"; do
  root="$(build_tree family-floating)"
  write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
  write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$floating")"
  expect_refusal "a canonical ref of '$floating' is refused as not one commit" "$root" 'not one 40-hex commit'
done

root="$(build_tree family-twice)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-manifests\\.yaml@$LEGACY_SET|\\.github/\\.github/workflows/publish-manifests\\.yaml@$SHA_K|\\.github/\\.github/workflows/publish-manifests\\.yaml@$SHA_X)\$"
expect_refusal 'a second canonical alternative is refused' "$root" 'more than once'

root="$(build_tree family-legacy-twice)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-manifests\\.yaml@$LEGACY_SET|\\.github/\\.github/workflows/publish-manifests\\.yaml@$SHA_K|actions/\\.github/workflows/publish-manifests\\.yaml@$SHA_X)\$"
expect_refusal 'a second legacy alternative after the canonical one is refused' "$root" 'more than once'

root="$(build_tree family-third-repository)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-manifests\\.yaml@$LEGACY_SET|elsewhere/\\.github/workflows/publish-manifests\\.yaml@$SHA_K)\$"
expect_refusal 'a second family that is not the canonical repository is refused' "$root" 'legacy family followed by the canonical family'

root="$(build_tree family-other-workflow)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/(actions/\\.github/workflows/publish-manifests\\.yaml@$LEGACY_SET|\\.github/\\.github/workflows/publish-app\\.yaml@$SHA_K)\$"
expect_refusal 'the two families must name the same workflow' "$root" 'different workflows'

root="$(build_tree family-reversed)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/(\\.github/\\.github/workflows/publish-manifests\\.yaml@$SHA_K|actions/\\.github/workflows/publish-manifests\\.yaml@$LEGACY_SET)\$"
expect_refusal 'canonical-first is not the rendered spelling and is refused' "$root" 'legacy family followed by the canonical family'

root="$(build_tree family-canonical-only)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-manifests\\.yaml@$SHA_K\$"
expect_refusal 'a canonical-only matcher drops every legacy approval and is refused' "$root" 'does not start with the shared-workflow identity prefix'

root="$(build_tree family-unanchored)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
subject="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "${subject%\$}"
expect_refusal 'an unanchored two-family matcher is refused' "$root" "does not end with ')\$'"

root="$(build_tree family-trailing)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "${subject%\$}.*\$"
expect_refusal 'a suffix after the group is refused' "$root" "does not end with ')\$'"

root="$(build_tree family-second-identity)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$subject"
printf "      - issuer: '.*'\n        subject: '.*'\n" >>"$root/$(manifest_path "$manifest_consumer")"
expect_refusal 'a second identity entry beside a two-family matcher is refused' "$root" 'exactly one is allowed'

# A subject naming ONLY the canonical repository must be found to be judged. Beside a correct
# matcher it would otherwise sit unseen in a second file or a second document.
canonical_only="^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/publish-manifests\\.yaml@.*\$"
root="$(build_tree stray-second-file)"
mkdir -p "$root/k8s/extra"
sed "s|subject: .*|subject: '$canonical_only'|" "$root/$(manifest_path "$manifest_consumer")" >"$root/k8s/extra/oci-repository.yaml"
expect_refusal 'a canonical-only subject in a second file for the same artifact is refused' "$root" 'k8s/extra/oci-repository.yaml'

root="$(build_tree stray-second-document)"
printf -- '---\napiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata:\n  name: stray\nspec:\n  rules:\n    - subjectRegExp: %s\n' "$canonical_only" >>"$root/$(manifest_path "$manifest_consumer")"
expect_refusal 'a canonical-only subject in a second document is refused' "$root" 'subject outside the OCIRepository document is unjudged'

root="$(build_tree family-empty-legacy)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests '' "$SHA_K")"
expect_refusal 'an empty legacy ref is refused by name' "$root" 'names no revision'

# ── switch OFF: the canonical family has no pattern form to fall back to ──────────────────
ENFORCE=0
root="$(build_tree off-recorded)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K")"
expect_pass 'switch off: the legacy pattern form beside the recorded commit passes' "$root"

root="$(build_tree off-unrecorded)"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K")"
expect_refusal 'switch off: an unrecorded canonical family is still refused' "$root" "$SHA_K" 'record none'

root="$(build_tree off-other-commit)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_X")"
expect_refusal 'switch off: another canonical commit is still refused' "$root" "$SHA_X" "$SHA_K"

root="$(build_tree off-missing)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(legacy_subject publish-manifests "$PATTERN")"
expect_refusal 'switch off: a recorded approval the matcher does not carry is still refused' "$root" 'does not accept the canonical publisher'
ENFORCE=1

# ── the writer ────────────────────────────────────────────────────────────────────────────
subject_in() { yq eval -r 'select(.kind == "OCIRepository") | .spec.verify.matchOIDCIdentity[0].subject' "$1"; }

root="$(build_tree writer-legacy)"
cp -R "$root" "$WORK/writer-legacy.before"
if out="$(run_in "$root" "$WRITER")" && diff -r "$root" "$WORK/writer-legacy.before" >/dev/null; then
  pass 'writer: with no approval, a legacy tree is left byte for byte'
else
  fail 'writer: a record that approves nothing changed a legacy tree'; printf '%s\n' "$out" >&2
fi

root="$(build_tree writer-adds)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
cp "$root/$(manifest_path "$other_manifest_consumer")" "$WORK/writer-adds.other"
want="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$want" ]; then
  pass 'writer: renders the legacy set and the recorded commit as one two-family subject'
else
  fail 'writer: did not render the expected two-family subject'; printf '%s\n' "$out" >&2
fi
if cmp -s "$root/$(manifest_path "$other_manifest_consumer")" "$WORK/writer-adds.other"; then
  pass 'writer: a consumer with no approval is not touched'
else
  fail 'writer: rewrote a consumer the record does not name'
fi
expect_pass 'writer: its output passes the enforcing guard' "$root"
if out="$(run_in "$root" "$WRITER")" && case "$out" in *' 0 consumer matcher(s) changed'*) true ;; *) false ;; esac; then
  pass 'writer: a second run changes nothing'
else
  fail 'writer: is not idempotent on a two-family tree'; printf '%s\n' "$out" >&2
fi

# Withdrawing the approval must render the legacy subject back, not leave the family behind.
write_record "$root"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$(legacy_subject publish-manifests "$LEGACY_SET")" ]; then
  pass 'writer: a withdrawn approval renders the legacy subject back'
else
  fail 'writer: a withdrawn approval left the canonical family in the matcher'; printf '%s\n' "$out" >&2
fi

root="$(build_tree writer-refuses)"
write_record "$root" "$(record_row "$manifest_consumer" main)"
cp -R "$root" "$WORK/writer-refuses.before"
if out="$(run_in "$root" "$WRITER")"; then
  fail 'writer: accepted a floating canonical approval'; printf '%s\n' "$out" >&2
elif diff -r "$root" "$WORK/writer-refuses.before" >/dev/null; then
  pass 'writer: an invalid record changes no manifest'
else
  fail 'writer: refused an invalid record but still changed the tree'
fi

printf '\n%d failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
