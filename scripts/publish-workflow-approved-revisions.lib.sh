# shellcheck shell=bash
# Shared, read-only validation and consumer attribution for the matcher guard and writer.
# Sourcing validates the complete approved set and source tree before either caller acts.

set -euo pipefail

GUARD_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly GUARD_DIR
readonly REPORT="$GUARD_DIR/report-publish-workflow-signing-revisions.sh"
# shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
source "$REPORT"

readonly APPROVED_SET="${APPROVED_REVISIONS_FILE:-$REPO_ROOT/scripts/publish-workflow-approved-revisions.tsv}"
readonly SCAN_ROOT="${PUBLISH_CONSUMER_ROOT:-$REPO_ROOT}"
readonly ENFORCE="${APPROVED_REVISIONS_ENFORCE:-0}"
readonly HEADER=$'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\trelease_candidate_sha\tobserved_on'

# Relative to the scan root. Every entry must exist: a generic subject that moves or is
# renamed would otherwise become an unattributed subject with no home, and this list is
# where its absence should be noticed and the boundary redrawn on purpose.
readonly GENERIC_SUBJECT_FILES=(
  'k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml'
  'k8s/bases/infrastructure/resource-graph-definitions/tenant/resource-graph-definition.yaml'
  'talos/cluster/verify-first-party-images.yaml'
)

# Every registered consumer, application tenants included, is narrowed to its approved revision
# set: the revision that signed the running artifact, the revision its default branch pins, and,
# the latest released devantler-tech/actions revision (#3960, #4416). A tenant
# cannot move its pin ahead of that set: each app tenant runs a required check that refuses a pin
# the committed approved set does not contain (#3961), so a new shared-workflow revision holds the
# tenant's dependency PR rather than failing its release.

readonly SUBJECT_PREFIX='^https://github\.com/devantler-tech/actions/\.github/workflows/publish-'
# Used by both callers after the shared discovery completes.
# shellcheck disable=SC2034
readonly PATTERN_REF='[0-9a-f]{40}'

# ── Publisher families (#4502) ──────────────────────────────────────────────────────────
# A manifest artifact may additionally accept ONE reviewed commit of the canonical manifest
# workflow, which lives in a second repository. That approval is a fixed, hand-reviewed record
# rather than a column of the generated set, so the daily regeneration can never move it.
# The matcher stays a single identity entry: one anchored subject whose only top-level group
# holds the legacy family first and the canonical family second. A second identity entry
# would be an OR beside the pair, which the attribution below already refuses.
readonly CANONICAL_SET="${CANONICAL_APPROVALS_FILE:-$REPO_ROOT/scripts/publish-workflow-canonical-approvals.tsv}"
readonly CANONICAL_HEADER=$'consumer\tworkflow\trepository\tcommit'
readonly CANONICAL_REPOSITORY='devantler-tech/.github'
readonly CANONICAL_WORKFLOW='publish-manifests'
readonly FAMILY_PREFIX='^https://github\.com/devantler-tech/('
readonly LEGACY_FAMILY='actions/\.github/workflows/publish-'
readonly CANONICAL_FAMILY='\.github/\.github/workflows/publish-'
# SUBJECT_PATTERN with an optional group opener before the legacy repository, so the scan
# finds a two-family subject as well as a legacy-only one. Every legacy-only match is unchanged.
readonly FAMILY_SUBJECT_PATTERN='(subject|subjectRegex|subjectRegExp):[[:space:]]*.?\^?https://github\\?\.com/devantler-tech/[(]?actions/\\?\.github/workflows/publish-(app|manifests)\\?\.yaml@'

refuse() {
  printf 'guard-publish-workflow-approved-revisions: %s\n' "$*" >&2
  exit 1
}

case "$ENFORCE" in
  0 | 1) ;;
  *) refuse "APPROVED_REVISIONS_ENFORCE must be 0 or 1, got '$ENFORCE'" ;;
esac

# ── 1. The approved set, read strictly ──────────────────────────────────────────────────
[ -f "$APPROVED_SET" ] || refuse "approved set not found at $APPROVED_SET; run scripts/generate-publish-workflow-approved-revisions.sh"
[ "$(head -n1 "$APPROVED_SET")" = "$HEADER" ] || refuse "approved set header is not the generator's; refusing to read it"

# Records are newline-delimited "<key><TAB>…" strings rather than associative arrays: the
# maintainer's macOS ships bash 3.2, where `declare -A` is a hard error, and a guard that
# cannot run where the matchers are edited is one that gets skipped there.
# lookup <records> <key> → the record for <key> (fields after the key), or nothing.
lookup() {
  # `$1 "" == k ""` forces a STRING compare: awk compares two numeric-looking strings as
  # numbers, so `100` would match a key of `1e2`.
  printf '%s\n' "$1" | awk -F'\t' -v k="$2" '$1 "" == k "" { sub(/^[^\t]*\t/, ""); print; exit }'
}

# approved_revisions <signer> <pin> <candidate> → each distinct approved revision on its own line,
# in signer, pin, candidate order. A legacy `-` manifests candidate names no revision.
approved_revisions() {
  local seen=" " rev
  for rev in "$@"; do
    [ "$rev" != '-' ] || continue
    case "$seen" in *" $rev "*) continue ;; esac
    seen="$seen$rev "
    printf '%s\n' "$rev"
  done
}

# approved_ref <signer> <pin> <candidate> → the canonical matcher ref: the single revision, or a
# grouped alternation of the distinct revisions in approved_revisions order.
approved_ref() {
  local revs
  revs="$(approved_revisions "$@")"
  if [ "$(printf '%s\n' "$revs" | grep -c .)" -eq 1 ]; then
    printf '%s' "$revs"
  else
    printf '(%s)' "$(printf '%s\n' "$revs" | paste -sd'|' -)"
  fi
}

# ref_matches_approved <ref> <signer> <pin> <candidate> → success when <ref> names exactly the
# approved revisions, each once, in any order. A single revision may be grouped or bare; several
# must be grouped, because an ungrouped `a|b` inside the anchored subject splits the whole regex
# into two alternatives and admits far more than either revision. Anything else fails, including
# a ref that adds, omits or repeats a revision.
ref_matches_approved() {
  local ref="$1" body got want
  shift
  case "$ref" in
    '('*')') body="${ref#\(}"; body="${body%\)}" ;;
    *'|'*) return 1 ;;
    *) body="$ref" ;;
  esac
  got="$(printf '%s\n' "$body" | tr '|' '\n')"
  ! grep -qvxE '[0-9a-f]{40}' <<<"$got" || return 1
  [ "$(printf '%s\n' "$got" | sort | uniq -d)" = "" ] || return 1
  want="$(approved_revisions "$@" | sort)"
  [ "$(printf '%s\n' "$got" | sort)" = "$want" ]
}

# consumer → "workflow<TAB>signer<TAB>pin"
approved=""
line_no=1
while IFS= read -r row; do
  line_no=$((line_no + 1))
  # Tab is IFS whitespace, so a split `read` merges adjacent tabs and drops a trailing one:
  # an empty final field would vanish. Count the separators on the raw row instead.
  tabs="${row//[!$'\t']/}"
  IFS=$'\t' read -r consumer workflow applied_tag applied_digest signer pin candidate observed_on extra <<<"$row"
  [ -n "$consumer" ] || continue
  [ "$line_no" -gt 2 ] || [ "$consumer" != 'consumer' ] || continue  # the header
  { [ "${#tabs}" -le 7 ] && [ -z "${extra:-}" ]; } || refuse "approved set line $line_no has more than eight fields"
  { [ "${#tabs}" -eq 7 ] && [ -n "$observed_on" ]; } || refuse "approved set line $line_no has fewer than eight fields"
  plausible_repo "$consumer" || refuse "approved set line $line_no names an implausible consumer '$consumer'"
  case "$workflow" in
    publish-app | publish-manifests) ;;
    *) refuse "approved set line $line_no: '$workflow' is not a shared publish workflow" ;;
  esac
  is_sha "$signer" || refuse "approved set line $line_no ($consumer): applied_signer_sha '$signer' is not a 40-hex commit"
  is_sha "$pin" || refuse "approved set line $line_no ($consumer): main_pin_sha '$pin' is not a 40-hex commit"
  # Both publishing workflows can preapprove one released commit. Legacy manifests
  # rows remain readable until the normal generator refreshes the reviewed set.
  if [ "$workflow" = publish-app ] || [ "$candidate" != '-' ]; then
    is_sha "$candidate" || refuse "approved set line $line_no ($consumer): release_candidate_sha '$candidate' is not a 40-hex commit"
  fi
  [ -z "$(lookup "$approved" "$consumer")" ] || refuse "approved set names $consumer twice"
  : "$applied_tag" "$applied_digest"
  approved="${approved}${consumer}"$'\t'"${workflow}"$'\t'"${signer}"$'\t'"${pin}"$'\t'"${candidate}"$'\n'
done <"$APPROVED_SET"

# Exactly the registered consumers, both directions: a missing row is a consumer nobody
# narrowed, an extra row is a set nothing consumes.
for expected in "${EXPECTED_CONSUMERS[@]}"; do
  [ -n "$(lookup "$approved" "$expected")" ] || refuse "approved set has no row for registered consumer $expected"
done
while IFS=$'\t' read -r consumer _rest; do
  [ -n "$consumer" ] || continue
  known=0
  for expected in "${EXPECTED_CONSUMERS[@]}"; do
    [ "$expected" = "$consumer" ] && known=1 && break
  done
  [ "$known" -eq 1 ] || refuse "approved set names $consumer, which is not a registered consumer"
done <<<"$approved"

# ── 1b. The canonical approvals, read strictly ──────────────────────────────────────────
# One row approves one reviewed commit of the canonical manifest workflow for one manifest
# consumer. Anything this reader cannot place exactly is refused: an approval it skipped would
# be an identity nobody rendered, and one it guessed at would be an identity nobody reviewed.
[ -f "$CANONICAL_SET" ] || refuse "canonical approvals not found at $CANONICAL_SET; the file must exist even when it approves nothing"
# `|| [ -n … ]` keeps a final row that has no trailing newline: dropping it would read a
# present approval as absent.
canonical_header=""
IFS= read -r canonical_header <"$CANONICAL_SET" || [ -n "$canonical_header" ] || refuse "canonical approvals at $CANONICAL_SET are empty"
[ "$canonical_header" = "$CANONICAL_HEADER" ] || refuse "canonical approvals header is not 'consumer<TAB>workflow<TAB>repository<TAB>commit'; refusing to read it"

# consumer → "commit"
canonical=""
line_no=0
while IFS= read -r row || [ -n "$row" ]; do
  line_no=$((line_no + 1))
  [ "$line_no" -gt 1 ] || continue  # the header, checked above
  tabs="${row//[!$'\t']/}"
  [ "${#tabs}" -eq 3 ] || refuse "canonical approvals line $line_no does not have exactly four fields"
  IFS=$'\t' read -r consumer workflow repository commit <<<"$row"
  { [ -n "$consumer" ] && [ -n "$workflow" ] && [ -n "$repository" ] && [ -n "$commit" ]; } ||
    refuse "canonical approvals line $line_no has an empty field"
  plausible_repo "$consumer" || refuse "canonical approvals line $line_no names an implausible consumer '$consumer'"
  [ "$repository" = "$CANONICAL_REPOSITORY" ] ||
    refuse "canonical approvals line $line_no ($consumer): '$repository' is not a known publisher family; only $CANONICAL_REPOSITORY is"
  [ "$workflow" = "$CANONICAL_WORKFLOW" ] ||
    refuse "canonical approvals line $line_no ($consumer): '$workflow' is not the canonical manifest workflow; only $CANONICAL_WORKFLOW is"
  is_sha "$commit" || refuse "canonical approvals line $line_no ($consumer): commit '$commit' is not a 40-hex commit"
  set_record="$(lookup "$approved" "$consumer")"
  [ -n "$set_record" ] || refuse "canonical approvals line $line_no names $consumer, which is not a registered consumer"
  [ "${set_record%%$'\t'*}" = "$CANONICAL_WORKFLOW" ] ||
    refuse "canonical approvals line $line_no: $consumer publishes with ${set_record%%$'\t'*}, and only $CANONICAL_WORKFLOW consumers may accept the canonical publisher"
  [ -z "$(lookup "$canonical" "$consumer")" ] ||
    refuse "canonical approvals name $consumer twice; a consumer accepts at most one canonical commit"
  canonical="${canonical}${consumer}"$'\t'"${commit}"$'\n'
done <"$CANONICAL_SET"

# approved_subject <workflow> <legacy-ref> <canonical-commit-or-empty> → the whole matcher subject.
# Without a canonical commit this is the legacy subject, byte for byte.
approved_subject() {
  local name="${1#publish-}"
  if [ -z "$3" ]; then
    printf '%s%s\\.yaml@%s$' "$SUBJECT_PREFIX" "$name" "$2"
  else
    printf '%s%s%s\\.yaml@%s|%s%s\\.yaml@%s)$' \
      "$FAMILY_PREFIX" "$LEGACY_FAMILY" "$name" "$2" "$CANONICAL_FAMILY" "$name" "$3"
  fi
}

# ── 2. Every shared-workflow subject in the tree, attributed or excluded ─────────────────
[ -d "$SCAN_ROOT" ] || refuse "scan root $SCAN_ROOT is not a directory"
for generic in "${GENERIC_SUBJECT_FILES[@]}"; do
  [ -f "$SCAN_ROOT/$generic" ] || refuse "generic subject file $generic is missing from the scan root; the scope boundary has moved — redraw GENERIC_SUBJECT_FILES deliberately"
done

# `.claude/` holds nested per-session worktrees on the maintainer's checkout — whole copies of
# this tree — so descending into it reports every generic subject a second time from a path the
# exclusion list does not name, and the guard refuses a correct repository. `.git` for the same
# reason a packed ref or a stray object file must never be read as a manifest.
subject_files="$(grep -rlE "$FAMILY_SUBJECT_PATTERN" --include='*.yaml' --exclude-dir=.git --exclude-dir=.claude "$SCAN_ROOT" 2>/dev/null | sort -u || true)"
[ -n "$subject_files" ] || refuse "no shared-publish-workflow subject found under $SCAN_ROOT; the scan, not the tree, is the likely cause"

# consumer → "file<TAB>workflow<TAB>ref<TAB>canonical-commit-or-dash"
observed=""
while IFS= read -r file; do
  [ -n "$file" ] || continue
  rel="${file#"$SCAN_ROOT"/}"
  case "$rel" in
    scripts/*) continue ;;  # test fixtures under scripts/ carry subjects that verify nothing
  esac
  is_generic=0
  for generic in "${GENERIC_SUBJECT_FILES[@]}"; do
    [ "$rel" = "$generic" ] && is_generic=1 && break
  done
  [ "$is_generic" -eq 0 ] || continue

  # One OCIRepository document per file, with exactly one identity entry, and that entry names a
  # shared workflow — the same attribution the report makes, read the way Flux reads it. Fields:
  # url, the total number of identity entries, the number naming a shared workflow, and the
  # first of those.
  #
  # The TOTAL matters as much as the shared count: `matchOIDCIdentity` is an OR-list, so a second
  # entry beside a correct pair (`subject: '.*'`, or a first-party branch identity) admits any
  # signer while the pair reads as narrowed. Counting only the shared-workflow entries would
  # report `form=set` over exactly that widening.
  # shellcheck disable=SC2016  # `$ids`/`$shared` are yq variables, not shell ones
  docs="$(yq eval -r '
    select(.kind == "OCIRepository") |
    (.spec.verify.matchOIDCIdentity // []) as $ids |
    ($ids | map(.subject // "") | map(select(test("devantler-tech/[(]?actions/.{1,2}github/workflows/publish-(app|manifests)")))) as $shared |
    [(.spec.url // "-"), ($ids | length), ($shared | length), ($shared[0] // "-")] | @tsv
  ' "$file" 2>/dev/null)" || refuse "$rel could not be read as YAML"
  [ -n "$docs" ] || refuse "$rel carries a shared-publish-workflow subject but no OCIRepository document owns it; attribute it to a consumer or add it to GENERIC_SUBJECT_FILES"
  [ "$(printf '%s\n' "$docs" | grep -c .)" -eq 1 ] || refuse "$rel carries more than one OCIRepository document; the attribution is ambiguous"
  IFS=$'\t' read -r url identity_count subject_count subject <<<"$docs"
  case "$url" in
    oci://ghcr.io/devantler-tech/*) ;;
    *) refuse "$rel: OCIRepository url '$url' is not a devantler-tech GHCR artifact" ;;
  esac
  [ "$subject_count" = "1" ] || refuse "$rel: expected exactly one shared-publish-workflow subject on the OCIRepository, found $subject_count"
  [ "$identity_count" = "1" ] || refuse "$rel: the OCIRepository carries $identity_count matchOIDCIdentity entries; a second entry beside the pair admits any signer it names, so exactly one is allowed"
  # The line scan that discovered this file and the document read above must agree: a shared-
  # workflow subject in a SECOND document (a policy appended after `---`) is invisible to the
  # OCIRepository selection, and would otherwise pass unjudged.
  line_count="$(grep -cE "$FAMILY_SUBJECT_PATTERN" "$file" || true)"
  [ "$line_count" = "1" ] || refuse "$rel: $line_count shared-publish-workflow subject lines found but exactly one OCIRepository entry was read; a subject outside the OCIRepository document is unjudged"
  repo="${url#oci://ghcr.io/devantler-tech/}"; repo="${repo%%/*}"
  repo="$(oci_name_to_repo "$repo")"
  plausible_repo "$repo" || refuse "$rel: consumer name '$repo' derived from $url is implausible"
  [ -n "$(lookup "$approved" "$repo")" ] || refuse "$rel is a shared-workflow consumer ($repo) with no row in the approved set and no entry in GENERIC_SUBJECT_FILES"
  prior="$(lookup "$observed" "$repo")"
  [ -z "$prior" ] || refuse "consumer $repo has an OCIRepository in both $rel and ${prior%%$'\t'*}"

  canonical_ref='-'
  case "$subject" in
    "$SUBJECT_PREFIX"*)
      rest="${subject#"$SUBJECT_PREFIX"}"          # <workflow>\.yaml@<ref>$
      case "$rest" in
        *'$') rest="${rest%\$}" ;;
        *) refuse "$rel: subject is not anchored with a trailing \$: $subject" ;;
      esac
      ;;
    "$FAMILY_PREFIX"*)
      # ^…/(<legacy family><workflow>\.yaml@<ref>|<canonical family><workflow>\.yaml@<commit>)$
      # Legacy first, canonical second, each exactly once: any other arrangement is refused
      # rather than interpreted, so there is one spelling for the writer to produce and for
      # the guard to compare.
      inner="${subject#"$FAMILY_PREFIX"}"
      case "$inner" in
        *')$') inner="${inner%')$'}" ;;
        *) refuse "$rel: two-family subject does not end with ')\$': $subject" ;;
      esac
      family_boundary='|'"$CANONICAL_FAMILY"
      case "$inner" in
        "$LEGACY_FAMILY"*"$family_boundary"*) ;;
        *) refuse "$rel: two-family subject is not the legacy family followed by the canonical family: $subject" ;;
      esac
      canonical_part="${inner#*"$family_boundary"}"   # <workflow>\.yaml@<commit>
      legacy_part="${inner%"$family_boundary$canonical_part"}"
      case "$canonical_part" in
        *"$CANONICAL_FAMILY"* | *"$LEGACY_FAMILY"*) refuse "$rel: two-family subject names a publisher family more than once: $subject" ;;
      esac
      case "$canonical_part" in
        *'\.yaml@'*) ;;
        *) refuse "$rel: canonical family has no '\\.yaml@' boundary: $subject" ;;
      esac
      canonical_ref="${canonical_part#*\\.yaml@}"
      is_sha "$canonical_ref" || refuse "$rel: canonical family pins '@$canonical_ref', which is not one 40-hex commit"
      rest="${legacy_part#"$LEGACY_FAMILY"}"          # <workflow>\.yaml@<ref>
      [ "publish-${canonical_part%%\\.yaml@*}" = "publish-${rest%%\\.yaml@*}" ] ||
        refuse "$rel: the two publisher families name different workflows: $subject"
      ;;
    *) refuse "$rel: subject does not start with the shared-workflow identity prefix: $subject" ;;
  esac
  case "$rest" in
    *'\.yaml@'*) ;;
    *) refuse "$rel: subject has no '\\.yaml@' boundary: $subject" ;;
  esac
  workflow="publish-${rest%%\\.yaml@*}"
  ref="${rest#*\\.yaml@}"
  observed="${observed}${repo}"$'\t'"${rel}"$'\t'"${workflow}"$'\t'"${ref}"$'\t'"${canonical_ref}"$'\n'
done <<<"$subject_files"
