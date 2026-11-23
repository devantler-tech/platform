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
readonly SHA_A='1111111111111111111111111111111111111111' # applied signer
readonly SHA_B='2222222222222222222222222222222222222222' # default-branch pin
readonly SHA_E='5555555555555555555555555555555555555555' # release candidate
readonly SHA_K='6666666666666666666666666666666666666666' # reviewed canonical commit
readonly SHA_X='7777777777777777777777777777777777777777' # a commit nobody approved
readonly DIGEST='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
readonly CANONICAL_REPOSITORY='devantler-tech/.github'
readonly LEGACY_SET="($SHA_A|$SHA_B|$SHA_E)"
readonly GENERICS=(
  'k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml'
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
# Keep legacy compatibility controls independent of canonical-only admission.
# shellcheck source=scripts/tests/lib/legacy-consumer-fixture.sh
source "$REPO_ROOT/scripts/tests/lib/legacy-consumer-fixture.sh"
build_legacy_consumer_fixture "$REPO_ROOT" "$WORK/legacy-source"
export PUBLISH_CONSUMER_ROOT="$WORK/legacy-source"
# Keep canonical fixture approvals independent of production bootstrap evidence.
export PUBLISH_BOOTSTRAP_FILE="$WORK/bootstrap-none.json"
printf '{"schemaVersion":1,"publications":[]}\n' >"$PUBLISH_BOOTSTRAP_FILE"

consumers="$("$REPORT" --list-consumers 2>/dev/null | cut -f1,2 || true)"
[ -n "$consumers" ] || {
  fail '--list-consumers produced nothing; every case below would pass vacuously'
  exit 1
}
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
# The consumer's own release workflow alternative (#6265), written independently of the
# library under test: its current repository with dots escaped, on a strict semver tag.
# The `.github` consumer, whose repository hosts the shared publishers, gets only its
# cd.yaml on a release tag (devantler-tech/.github#926), never any workflow file there.
readonly OWN_TAIL='/\.github/workflows/[^/]+\.ya?ml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
readonly GITHUB_OWN='\.github/\.github/workflows/cd\.yaml@refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.((0|[1-9][0-9]*)|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
own_release_of() { # <consumer>
  local repository="$1"
  case "$1" in
    .github)
      printf '|%s' "$GITHUB_OWN"
      return 0
      ;;
    data-product-controller) repository=khoist ;;
  esac
  printf '|%s%s' "${repository//./\\.}" "$OWN_TAIL"
}
family_subject() { # <workflow> <legacy-ref> <canonical-ref> [consumer]
  printf '^https://github\\.com/devantler-tech/(actions/\\.github/workflows/%s\\.yaml@%s|\\.github/\\.github/workflows/%s\\.yaml@%s%s)$' \
    "$1" "$2" "$1" "$3" "$(if [ -n "${4:-}" ]; then own_release_of "$4"; fi)"
}
canonical_subject() { # <workflow> <canonical-ref> — the generic verifiers' form (#6264)
  printf '^https://github\\.com/devantler-tech/\\.github/\\.github/workflows/%s\\.yaml@%s$' "$1" "$2"
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

# build_tree <name> — every consumer on its legacy generated set, the generics on the
# canonical-only form (#6264), the approved set, and a canonical record that approves nothing.
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
    printf 'rules:\n  - subjectRegex: %s\n' "$(canonical_subject publish-app "$SHA_K")" >"$root/$generic"
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
  if out="$(run_in "$2" "$GUARD")"; then pass "$1"; else
    fail "$1 — expected exit 0:"
    printf '%s\n' "$out" >&2
  fi
}
expect_refusal() { # <case> <root> <must-mention>...
  local case_name="$1" root="$2" out needle
  shift 2
  if out="$(run_in "$root" "$GUARD")"; then
    fail "$case_name — expected a refusal, guard exited 0:"
    printf '%s\n' "$out" >&2
    return
  fi
  for needle in "$@"; do
    case "$out" in
      *"$needle"*) ;;
      *)
        fail "$case_name — refusal does not name '$needle':"
        printf '%s\n' "$out" >&2
        return
        ;;
    esac
  done
  pass "$case_name"
}

# ── controls ──────────────────────────────────────────────────────────────────────────────
root="$(build_tree control)"
expect_pass 'control: a legacy-only tree with a record that approves nothing passes' "$root"

# A typed source has no literal subject for the old grep to find. A Tenant is admitted
# only beside the exact own-release graph (ADR 0019); beside any other graph its
# controller-made source must not disappear from attribution.
root="$(build_tree unapproved-typed-source)"
tenant_graph=k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml
mkdir -p "$root/$(dirname "$tenant_graph")" "$root/k8s/providers/hetzner/apps/business-site"
cp "$REPO_ROOT/$tenant_graph" "$root/$tenant_graph"
yq -i '(.spec.resources[] | select(.id == "ociRepository").template.spec.ref.semverFilter) = ".*"' "$root/$tenant_graph"
cp "$REPO_ROOT/k8s/providers/hetzner/apps/business-site/tenant.yaml" "$root/k8s/providers/hetzner/apps/business-site/tenant.yaml"
expect_refusal 'a typed source beside an unreviewed Tenant graph cannot disappear from attribution' "$root" 'inventory is UNKNOWN'

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

# A consumer may hold two rows while its publisher moves from one reviewed commit to the next
# (#4713): never a third, and never the same commit twice.
root="$(build_tree record-third)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")" "$(record_row "$manifest_consumer" "$SHA_A")"
expect_refusal 'a third canonical commit for one consumer is refused' "$root" "$manifest_consumer" 'at most two canonical commits'

root="$(build_tree record-repeated)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_K")"
expect_refusal 'the same canonical commit recorded twice is refused' "$root" "$manifest_consumer" "$SHA_K" 'twice'

root="$(build_tree record-second-unrendered)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
expect_refusal 'a second recorded commit the matcher does not carry is refused, naming both' "$root" "$manifest_consumer" "($SHA_K|$SHA_X)"

root="$(build_tree record-app-consumer)"
write_record "$root" "$(record_row "$app_consumer" "$SHA_K")"
expect_refusal 'an application consumer cannot approve the manifest workflow' "$root" "$app_consumer" 'publish-app'

root="$(build_tree record-app-workflow)"
write_record "$root" "$(printf '%s\tpublish-app\t%s\t%s' "$manifest_consumer" "$CANONICAL_REPOSITORY" "$SHA_K")"
expect_refusal 'a manifest consumer cannot approve the application workflow' "$root" "$manifest_consumer" 'publishes with publish-manifests'

root="$(build_tree record-unknown-workflow)"
write_record "$root" "$(printf '%s\tpublish-other\t%s\t%s' "$app_consumer" "$CANONICAL_REPOSITORY" "$SHA_K")"
expect_refusal 'an unknown canonical workflow is refused' "$root" 'not a supported canonical workflow'

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
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
expect_pass 'a two-family matcher naming the recorded commit and the legacy set passes' "$root"

# Application approvals use the same strict record and one anchored identity;
# supporting the family does not approve any application until a record names it.
app_row="$(printf '%s\tpublish-app\t%s\t%s' "$app_consumer" "$CANONICAL_REPOSITORY" "$SHA_K")"
root="$(build_tree app-family-agrees)"
write_record "$root" "$app_row"
write_consumer "$root" "$app_consumer" "$(family_subject publish-app "$LEGACY_SET" "$SHA_K" "$app_consumer")"
expect_pass 'an application accepts only its matching recorded publisher' "$root"
write_consumer "$root" "$app_consumer" "$(family_subject publish-app "$LEGACY_SET" "$SHA_X" "$app_consumer")"
expect_refusal 'an application refuses an unapproved canonical commit' "$root" "$app_consumer" "$SHA_X" "$SHA_K"
write_consumer "$root" "$app_consumer" "$(family_subject publish-app "$LEGACY_SET" "$PATTERN" "$app_consumer")"
expect_refusal 'an application refuses a wildcard canonical revision' "$root" 'not one 40-hex commit'

root="$(build_tree app-family-moving)"
write_record "$root" "$app_row" "$(printf '%s\tpublish-app\t%s\t%s' "$app_consumer" "$CANONICAL_REPOSITORY" "$SHA_X")"
write_consumer "$root" "$app_consumer" "$(family_subject publish-app "$LEGACY_SET" "($SHA_K|$SHA_X)" "$app_consumer")"
expect_pass 'an application may overlap exactly two reviewed canonical commits' "$root"
write_record "$root" "$app_row" "$(printf '%s\tpublish-app\t%s\t%s' "$app_consumer" "$CANONICAL_REPOSITORY" "$SHA_X")" "$(printf '%s\tpublish-app\t%s\t%s' "$app_consumer" "$CANONICAL_REPOSITORY" "$SHA_A")"
expect_refusal 'an application may not approve a third canonical commit' "$root" 'at most two canonical commits'

root="$(build_tree family-both)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$other_manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "($SHA_E|$SHA_B|$SHA_A)" "$SHA_X" "$other_manifest_consumer")"
expect_pass 'each manifest consumer carries its own recorded commit, legacy set in any order' "$root"

# While a publisher moves, the matcher carries both recorded commits, in the record's order.
root="$(build_tree family-moving)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "($SHA_K|$SHA_X)" "$manifest_consumer")"
expect_pass 'a two-family matcher naming both recorded commits in record order passes' "$root"

root="$(build_tree family-moving-other-consumer)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$other_manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "($SHA_K|$SHA_X)" "$manifest_consumer")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$other_manifest_consumer")"
expect_pass "one consumer's second commit leaves another consumer on its single commit" "$root"

root="$(build_tree family-moving-reordered)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "($SHA_X|$SHA_K)" "$manifest_consumer")"
expect_refusal 'the two recorded commits in another order are not the rendered spelling' "$root" "$manifest_consumer" "($SHA_X|$SHA_K)" "($SHA_K|$SHA_X)"

root="$(build_tree family-moving-unrecorded-second)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "($SHA_K|$SHA_X)" "$manifest_consumer")"
expect_refusal 'a second canonical commit the record does not hold is refused, naming both' "$root" "$manifest_consumer" "($SHA_K|$SHA_X)" "@$SHA_K"

root="$(build_tree family-moving-foreign-second)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "($SHA_K|$SHA_A)" "$manifest_consumer")"
expect_refusal 'a second canonical commit other than the recorded one is refused' "$root" "$manifest_consumer" "($SHA_K|$SHA_A)"

root="$(build_tree family-unrecorded)"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
expect_refusal 'a canonical family the record does not approve is refused' "$root" "$manifest_consumer" "$SHA_K" 'record none'

root="$(build_tree family-other-commit)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_X" "$manifest_consumer")"
expect_refusal 'a canonical commit other than the recorded one is refused, naming both' "$root" "$manifest_consumer" "$SHA_X" "$SHA_K"

root="$(build_tree family-wrong-consumer)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$other_manifest_consumer")"
expect_refusal "one consumer's approval does not cover another" "$root" "$other_manifest_consumer" 'record none'

root="$(build_tree family-legacy-foreign)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "($SHA_A|$SHA_B|$SHA_X)" "$SHA_K" "$manifest_consumer")"
expect_refusal 'the legacy set is still judged inside a two-family matcher' "$root" "$manifest_consumer" "$SHA_X" 'not the generated set'

root="$(build_tree family-legacy-dropped)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "($SHA_A|$SHA_B)" "$SHA_K" "$manifest_consumer")"
expect_refusal 'adding the canonical family may not drop a legacy approval' "$root" "$manifest_consumer" 'not the generated set'

root="$(build_tree family-legacy-pattern)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K" "$manifest_consumer")"
expect_refusal 'the pattern form stays refused under enforcement inside a two-family matcher' "$root" "$manifest_consumer" 'pattern form'

for floating in "$PATTERN" '.+' 'refs/heads/main' "$SHA_K|$SHA_X" "($SHA_K)" "($SHA_K|$SHA_K)" "($SHA_K|$SHA_X|$SHA_A)" "($SHA_K|$PATTERN)" "($SHA_K|.*)" "($SHA_K|$SHA_X).*"; do
  root="$(build_tree family-floating)"
  write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
  write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$floating" "$manifest_consumer")"
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
subject="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
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
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests '' "$SHA_K" "$manifest_consumer")"
expect_refusal 'an empty legacy ref is refused by name' "$root" 'names no revision'

# ── the consumer's own release workflow (#6265) ───────────────────────────────────────────
root="$(build_tree own-release-missing)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
expect_refusal 'a two-family matcher without its own release workflow is refused' "$root" 'does not accept its own release workflow'

root="$(build_tree own-release-other-repository)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$other_manifest_consumer-x")"
expect_refusal "a matcher accepting another repository's release workflow is refused" "$root" 'not exactly'

root="$(build_tree own-release-any-repository)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "[a-z-]+")"
expect_refusal 'a matcher accepting any repository'"'"'s release workflow is refused' "$root" 'not exactly'

root="$(build_tree own-release-twice)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" \
  "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K$(own_release_of "$manifest_consumer")" "$manifest_consumer")"
expect_refusal 'a matcher naming its own release workflow twice is refused' "$root" 'not exactly'

root="$(build_tree own-release-publisher-host)"
write_record "$root" "$(record_row "$other_manifest_consumer" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" \
  "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
expect_refusal "the .github consumer accepts no other repository's release workflow" "$root" 'not exactly'

root="$(build_tree own-release-publisher-host-cd)"
write_record "$root" "$(record_row "$other_manifest_consumer" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$other_manifest_consumer")"
expect_pass "the .github consumer accepts exactly its cd.yaml on a release tag" "$root"

root="$(build_tree own-release-publisher-host-none)"
write_record "$root" "$(record_row "$other_manifest_consumer" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" "$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
expect_refusal 'the .github consumer without its cd.yaml release workflow is refused' "$root" 'does not accept its own release workflow'

root="$(build_tree own-release-publisher-host-any)"
write_record "$root" "$(record_row "$other_manifest_consumer" "$SHA_K")"
any_github="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K")"
write_consumer "$root" "$other_manifest_consumer" "${any_github%')$'}|\\.github$OWN_TAIL)\$"
expect_refusal 'the .github consumer accepting any of its workflows is refused' "$root" 'not exactly'

# ── switch OFF: the canonical family has no pattern form to fall back to ──────────────────
ENFORCE=0
root="$(build_tree off-recorded)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K" "$manifest_consumer")"
expect_pass 'switch off: the legacy pattern form beside the recorded commit passes' "$root"

root="$(build_tree off-unrecorded)"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_K" "$manifest_consumer")"
expect_refusal 'switch off: an unrecorded canonical family is still refused' "$root" "$SHA_K" 'record none'

root="$(build_tree off-other-commit)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(family_subject publish-manifests "$PATTERN" "$SHA_X" "$manifest_consumer")"
expect_refusal 'switch off: another canonical commit is still refused' "$root" "$SHA_X" "$SHA_K"

root="$(build_tree off-missing)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
write_consumer "$root" "$manifest_consumer" "$(legacy_subject publish-manifests "$PATTERN")"
expect_refusal 'switch off: a recorded approval the matcher does not carry is still refused' "$root" 'does not accept the canonical publisher'
ENFORCE=1

# ── the writer ────────────────────────────────────────────────────────────────────────────
subject_in() { yq eval -r 'select(.kind == "OCIRepository") | .spec.verify.matchOIDCIdentity[0].subject' "$1"; }

root="$(build_tree writer-app)"
write_record "$root" "$app_row"
cp "$root/$(manifest_path "$manifest_consumer")" "$WORK/writer-app.other"
want="$(family_subject publish-app "$LEGACY_SET" "$SHA_K" "$app_consumer")"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$app_consumer")")" = "$want" ]; then
  pass 'writer: renders the approved application family without widening revisions'
else
  fail 'writer: did not render the approved application family'
  printf '%s\n' "$out" >&2
fi
expect_pass 'writer: its application output passes the enforcing guard' "$root"
# The independent repository-wide shape guard must accept the actual writer
# output too, not only handcrafted family examples in its own suite.
cp "$REPO_ROOT/scripts/guard-shared-publish-workflow-pin.sh" "$root/scripts/pin-guard.sh"
# This fixture deliberately places every consumer under its synthetic apps
# tree. Bind the floor to those intended fixture paths, not production paths.
{
  while IFS=$'\t' read -r fixture_consumer _; do
    [ -n "$fixture_consumer" ] || continue
    printf '1\t%s\n' "$(manifest_path "$fixture_consumer")"
  done <<<"$consumers"
  for fixture_generic in "${GENERICS[@]}"; do printf '1\t%s\n' "$fixture_generic"; done
} >"$root/scripts/shared-publish-workflow-matchers.tsv"
if out="$(bash "$root/scripts/pin-guard.sh" 2>&1)"; then
  pass 'writer: its application output passes the independent publisher pin guard'
else
  fail 'writer: application output rejected by independent publisher pin guard'
  printf '%s\n' "$out" >&2
fi
if cmp -s "$root/$(manifest_path "$manifest_consumer")" "$WORK/writer-app.other"; then
  pass 'writer: an application approval leaves the unapproved manifest consumer unchanged'
else
  fail 'writer: an application approval changed another consumer'
fi
write_record "$root"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$app_consumer")")" = "$(legacy_subject publish-app "$LEGACY_SET")" ]; then
  pass 'writer: withdrawing an application approval restores its exact legacy identity'
else
  fail 'writer: retained a withdrawn application approval'
  printf '%s\n' "$out" >&2
fi

root="$(build_tree writer-legacy)"
cp -R "$root" "$WORK/writer-legacy.before"
if out="$(run_in "$root" "$WRITER")" && diff -r "$root" "$WORK/writer-legacy.before" >/dev/null; then
  pass 'writer: with no approval, a legacy tree is left byte for byte'
else
  fail 'writer: a record that approves nothing changed a legacy tree'
  printf '%s\n' "$out" >&2
fi

root="$(build_tree writer-adds)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")"
cp "$root/$(manifest_path "$other_manifest_consumer")" "$WORK/writer-adds.other"
want="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_K" "$manifest_consumer")"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$want" ]; then
  pass 'writer: renders the legacy set and the recorded commit as one two-family subject'
else
  fail 'writer: did not render the expected two-family subject'
  printf '%s\n' "$out" >&2
fi
if cmp -s "$root/$(manifest_path "$other_manifest_consumer")" "$WORK/writer-adds.other"; then
  pass 'writer: a consumer with no approval is not touched'
else
  fail 'writer: rewrote a consumer the record does not name'
fi
expect_pass 'writer: its output passes the enforcing guard' "$root"
if out="$(run_in "$root" "$WRITER")" && case "$out" in *' 0 consumer matcher(s) changed'*) true ;; *) false ;; esac then
  pass 'writer: a second run changes nothing'
else
  fail 'writer: is not idempotent on a two-family tree'
  printf '%s\n' "$out" >&2
fi

# Withdrawing the approval must render the legacy subject back, not leave the family behind.
write_record "$root"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$(legacy_subject publish-manifests "$LEGACY_SET")" ]; then
  pass 'writer: a withdrawn approval renders the legacy subject back'
else
  fail 'writer: a withdrawn approval left the canonical family in the matcher'
  printf '%s\n' "$out" >&2
fi

# Two recorded commits render as one grouped pair in the record's order, and dropping the first
# row once the move has landed renders the single commit that remains.
root="$(build_tree writer-moving)"
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_K")" "$(record_row "$manifest_consumer" "$SHA_X")"
want="$(family_subject publish-manifests "$LEGACY_SET" "($SHA_K|$SHA_X)" "$manifest_consumer")"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$want" ]; then
  pass 'writer: renders two recorded commits as one grouped pair in record order'
else
  fail 'writer: did not render the expected two-commit canonical family'
  printf '%s\n' "$out" >&2
fi
expect_pass 'writer: its two-commit output passes the enforcing guard' "$root"
if out="$(run_in "$root" "$WRITER")" && case "$out" in *' 0 consumer matcher(s) changed'*) true ;; *) false ;; esac then
  pass 'writer: a second run changes nothing on a two-commit tree'
else
  fail 'writer: is not idempotent on a two-commit tree'
  printf '%s\n' "$out" >&2
fi
write_record "$root" "$(record_row "$manifest_consumer" "$SHA_X")"
want="$(family_subject publish-manifests "$LEGACY_SET" "$SHA_X" "$manifest_consumer")"
if out="$(run_in "$root" "$WRITER")" && [ "$(subject_in "$root/$(manifest_path "$manifest_consumer")")" = "$want" ]; then
  pass 'writer: dropping the first row renders the remaining commit alone'
else
  fail 'writer: left a withdrawn canonical commit in the matcher'
  printf '%s\n' "$out" >&2
fi

root="$(build_tree writer-refuses)"
write_record "$root" "$(record_row "$manifest_consumer" main)"
cp -R "$root" "$WORK/writer-refuses.before"
if out="$(run_in "$root" "$WRITER")"; then
  fail 'writer: accepted a floating canonical approval'
  printf '%s\n' "$out" >&2
elif diff -r "$root" "$WORK/writer-refuses.before" >/dev/null; then
  pass 'writer: an invalid record changes no manifest'
else
  fail 'writer: refused an invalid record but still changed the tree'
fi

# Exercise the committed verification payloads as identity predicates, not text
# snapshots. Existing releases remain valid, but only the reviewed maintained
# revision may join them; a wildcard or an unanchored alternation fails below.
while IFS=$'\t' read -r profile file selector; do
  subject="$(yq -er "$selector" "$REPO_ROOT/$file")"
  while IFS=$'\t' read -r expected identity; do
    # The product documentation consumer is data-product-controller, which approves
    # v19.0.5 since its publisher moved there (#6227), so it accepts the same rows.
    result=reject
    if printf '%s\n' "$identity" | grep -Eq -- "$subject"; then result=accept; fi
    # The generic rules no longer accept the deleted legacy publisher repository at
    # any commit (#6264). The consumer-specific sources keep their exact generated
    # legacy commit until the approval machinery that writes them is retired.
    if [[ "$identity" == https://github.com/devantler-tech/actions/* && ("$profile" == admission || "$profile" == node-pull) ]]; then expected=reject; fi
    if [ "$result" = "$expected" ]; then
      pass "committed $profile: $expected $identity"
    else
      fail "committed $profile: expected $expected for $identity, got $result"
    fi
  done <<'IDENTITIES'
accept	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328
accept	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@0c7b6c3509b73adbc7537e030935bb01987e625b
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@v19.0.5
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@main
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c5
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812-extra
reject	https://github.com/devantler-tech/xgithub/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/another-org/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df90
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@v13.10.12
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@main
IDENTITIES
done <<'PROFILES'
admission	k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml	.spec.attestors[] | select(.name == "publishapp").cosign.keyless.identities[0].subjectRegExp
node-pull	talos/cluster/verify-first-party-images.yaml	.rules[] | select(.image == "ghcr.io/devantler-tech/*").keyless.subjectRegex
coaching	k8s/bases/apps/ascoachingogvaner/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
wedding	k8s/bases/apps/wedding-app/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
product-docs	k8s/providers/hetzner/apps/khoist-documentation/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
PROFILES

# The generic rules admitted v21.3.4 (#6228) in place of the retired v8.0.0
# publisher. Its only user, business-site, now signs its own releases, so it is
# retired too (#6266). Exercise the committed predicates at both boundaries:
# v21.3.4 and v8.0.0 are refused in every form.
while IFS=$'\t' read -r profile file selector; do
  subject="$(yq -er "$selector" "$REPO_ROOT/$file")"
  while IFS=$'\t' read -r expected identity; do
    result=reject
    if printf '%s\n' "$identity" | grep -Eq -- "$subject"; then result=accept; fi
    if [ "$result" = "$expected" ]; then
      pass "retired v21 $profile: $expected $identity"
    else
      fail "retired v21 $profile: expected $expected for $identity, got $result"
    fi
  done <<'IDENTITIES'
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@v21.3.4
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35-extra
reject	https://github.com/devantler-tech/xgithub/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/another-org/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@467f07582436a26b62cf4aa756423512f85ba0c0
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@v8.0.0
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@467f075
IDENTITIES
done <<'PROFILES'
admission	k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml	.spec.attestors[] | select(.name == "publishapp").cosign.keyless.identities[0].subjectRegExp
node-pull	talos/cluster/verify-first-party-images.yaml	.rules[] | select(.image == "ghcr.io/devantler-tech/*").keyless.subjectRegex
PROFILES

# The legacy publisher repository is deleted (#6264). The generic rules accept only
# the reviewed maintained revision v19.0.5 and refuse that repository at every commit:
# an arbitrary one, the commit its final release was cut from, commits that other
# consumers approved for it, and a maintained commit written under its name.
while IFS=$'\t' read -r profile file selector; do
  subject="$(yq -er "$selector" "$REPO_ROOT/$file")"
  while IFS=$'\t' read -r expected identity; do
    result=reject
    if printf '%s\n' "$identity" | grep -Eq -- "$subject"; then result=accept; fi
    if [ "$result" = "$expected" ]; then
      pass "deleted publisher retired $profile: $expected $identity"
    else
      fail "deleted publisher retired $profile: expected $expected for $identity, got $result"
    fi
  done <<'IDENTITIES'
accept	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@0123456789abcdef0123456789abcdef01234567
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@b089a1b041cb86af22cdc57de58a4d7d258dcc32
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@af042c540ce8db4d64add98c34b5c57691e0e812
reject	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@a76d5df9371cf712a8f60307fde060825a34cf35
IDENTITIES
done <<'PROFILES'
admission	k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml	.spec.attestors[] | select(.name == "publishapp").cosign.keyless.identities[0].subjectRegExp
node-pull	talos/cluster/verify-first-party-images.yaml	.rules[] | select(.image == "ghcr.io/devantler-tech/*").keyless.subjectRegex
PROFILES

# A product's own release workflow on a release tag (#6265, ADR 0017). The image rules accept
# it for any first-party repository; each consumer source accepts only its own repository.
# Column 1 names the profiles that accept the identity (`-` for none). The generic Tenant
# source binds it to the tenant name, which the constructor reader prints as `<tenant>`, so
# no identity matches it here; the compiled graph is exercised in webapp-compiler/tenant_test.go.
#
# The image rules accept any first-party repository except `.github`, which hosts reusable
# workflows other repositories call; a repository named `actions` would be a first-party
# repository like any other, so its release tag passes them. No consumer source accepts it.
# The deleted publisher's commit-pinned identity stays refused by the image rules; only the
# consumers whose generated approvals name that exact legacy commit keep it.
while IFS=$'\t' read -r profile file selector; do
  subject="$(yq -er "$selector" "$REPO_ROOT/$file")"
  while IFS=$'\t' read -r accepting identity; do
    expected=reject
    case ",$accepting," in *",$profile,"*) expected=accept ;; esac
    result=reject
    if printf '%s\n' "$identity" | grep -Eq -- "$subject"; then result=accept; fi
    if [ "$result" = "$expected" ]; then
      pass "own release $profile: $expected $identity"
    else
      fail "own release $profile: expected $expected for $identity, got $result"
    fi
  done <<'IDENTITIES'
admission,node-pull,wedding	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3
admission,node-pull,wedding	https://github.com/devantler-tech/wedding-app/.github/workflows/cd.yml@refs/tags/v1.2.3-rc.1
admission,node-pull,coaching	https://github.com/devantler-tech/ascoachingogvaner/.github/workflows/release.yaml@refs/tags/v0.1.0
admission,node-pull,product-docs	https://github.com/devantler-tech/khoist/.github/workflows/release.yaml@refs/tags/v5.4.0
admission,node-pull,aws	https://github.com/devantler-tech/aws/.github/workflows/cd.yaml@refs/tags/v1.1.4
admission,node-pull	https://github.com/devantler-tech/data-product-controller/.github/workflows/release.yaml@refs/tags/v5.4.0
admission,node-pull	https://github.com/devantler-tech/wedding-app-x/.github/workflows/release.yaml@refs/tags/v1.2.3
-	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v21.3.4
-	https://github.com/devantler-tech/.github/.github/workflows/release.yaml@refs/tags/v21.3.4
-	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@refs/tags/v1.2.3
admission,node-pull	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@refs/tags/v1.2.3
admission,node-pull	https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@refs/tags/v5.4.0
coaching,wedding,product-docs	https://github.com/devantler-tech/actions/.github/workflows/publish-app.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328
-	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/heads/main
-	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2
-	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/latest
-	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3/../evil
-	https://github.com/devantler-tech/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3+build
-	https://github.com/devantler-tech/wedding-app/.github/workflows/nested/release.yaml@refs/tags/v1.2.3
-	https://github.com/devantler-tech/wedding-app/release.yaml@refs/tags/v1.2.3
-	https://github.com/devantler-tech/wedding-app/sub/.github/workflows/release.yaml@refs/tags/v1.2.3
-	https://github.com/another-org/wedding-app/.github/workflows/release.yaml@refs/tags/v1.2.3
IDENTITIES
done <<'PROFILES'
admission	k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml	.spec.attestors[] | select(.name == "publishapp").cosign.keyless.identities[0].subjectRegExp
node-pull	talos/cluster/verify-first-party-images.yaml	.rules[] | select(.image == "ghcr.io/devantler-tech/*").keyless.subjectRegex
coaching	k8s/bases/apps/ascoachingogvaner/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
wedding	k8s/bases/apps/wedding-app/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
product-docs	k8s/providers/hetzner/apps/khoist-documentation/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
aws	k8s/providers/hetzner/apps/aws/oci-repository.yaml	.spec.verify.matchOIDCIdentity[0].subject
github-config	k8s/bases/apps/github-config/oci-repository.yaml	select(.kind == "OCIRepository").spec.verify.matchOIDCIdentity[0].subject
PROFILES

# The github-config v12 transition retains the independently verified v12.0.2
# signer while admitting the new exact publisher. Retired and unrelated
# identities must fail both the committed subject and issuer predicates (#5265).
github_source="$REPO_ROOT/k8s/bases/apps/github-config/oci-repository.yaml"
github_subject="$(yq -er '.spec.verify.matchOIDCIdentity[0].subject' "$github_source")"
github_issuer="$(yq -er '.spec.verify.matchOIDCIdentity[0].issuer' "$github_source")"
while IFS=$'\t' read -r expected issuer identity; do
  result=reject
  if printf '%s\n' "$issuer" | grep -Eq -- "$github_issuer" &&
    printf '%s\n' "$identity" | grep -Eq -- "$github_subject"; then
    result=accept
  fi
  if [ "$result" = "$expected" ]; then
    pass "github-config transition: $expected $issuer $identity"
  else
    fail "github-config transition: expected $expected for $issuer $identity, got $result"
  fi
done <<'IDENTITIES'
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@56ceec6717a1faa0686d865424cd2d1d45f18a0a
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@57310dc609dcc8f9205f53c674ff17c5dc979b73
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@e6db1a4ed5b23ce6f69e755deeddd6649313e9dc
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@1a0eb6f18b88a7816b91cbf692b8ad8ea9a08123
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@933b53600c15fe7a67189756307cb19e23090c99
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@2666baeaf1384d3c2d918e19692b8a6d9fb6fd78
accept	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
accept	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@d70d50665465117f01104313e53057374a96a1a3
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@0966f4b015e85ef14be37411d424a7028a858550
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@bd24a3e1bbbf6aba2d7afd58a2fd8a3d67bebe6b
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@0f390329b590404c02565b27a263ee841fa02bce
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@8d2203af130a8c27b8b98b3d7146f76b5ade7a98
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@6f1dee9557a939cc24ca2c562ea775d05e01564d
accept	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@1d726483facb9340655489913f609082677be8f5
accept	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/actions/.github/workflows/publish-manifests.yaml@ea4df9004d4ab43b6c9af5b0c5f55712f7211328
reject	https://another-issuer.invalid	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
reject	https://tokenXactionsXgithubusercontentXcom	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
reject	https://token.actions.githubusercontent.com	https://github.com/another-org/.github/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/xgithub/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-app.yaml@f33622708f9f25a32acb46cc419007e18a3223b3
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@v12.0.1
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@main
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@f336227
reject	https://token.actions.githubusercontent.com	https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@f33622708f9f25a32acb46cc419007e18a3223b3-extra
IDENTITIES

aws_subject="$(yq -er '.spec.verify.matchOIDCIdentity[0].subject' "$REPO_ROOT/k8s/providers/hetzner/apps/aws/oci-repository.yaml")"
if printf '%s\n' 'https://github.com/devantler-tech/.github/.github/workflows/publish-manifests.yaml@6f1dee9557a939cc24ca2c562ea775d05e01564d' |
  grep -Eq -- "$aws_subject"; then
  fail 'the retired github-config publisher must stay closed for aws'
else
  pass 'the retired github-config publisher stays closed for the unrelated aws consumer'
fi

printf '\n%d failure(s)\n' "$failures"
[ "$failures" -eq 0 ]
