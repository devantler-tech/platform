#!/usr/bin/env bash
# RED/GREEN coverage for `report-publish-workflow-signing-revisions.sh` (#3048).
#
# WHAT IS ACTUALLY BEING PROVED
# The reported script answers one question per consumer: "is the revision that SIGNED
# what is deployed the same revision this consumer pins TODAY?" An allow-list built for
# #2818 has to accept both, so the whole value of the script is that a DIFFERENCE is
# surfaced and a MATCH is not. Those are the two directions this file pins.
#
# WHY THERE IS A RESOLVER SEAM
# Resolving a revision needs the network (release tag -> tag commit -> the pin in that
# commit's cd.yaml). A test that depended on it would prove nothing repeatable: it would
# go red when an unrelated repository cut a release, and green when the network was
# simply unavailable. The script therefore takes its resolver from
# PUBLISH_REVISION_RESOLVER, and these fixtures supply a stub. Discovery — which
# consumers exist and whether any went missing — stays REAL, against this repository's
# own manifests, because that is the half a stub could hide a regression in.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly SCRIPT="$REPO_ROOT/scripts/report-publish-workflow-signing-revisions.sh"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A stub resolver. It is handed <repo> <workflow> and prints "<signing>\t<current>".
# Each fixture writes its own answers into a table file the stub reads, so one stub
# covers every case without the fixtures having to generate shell code.
make_stub() {
  local table="$1" path="$WORK/resolver-$2.sh"
  cat >"$path" <<STUB
#!/usr/bin/env bash
set -euo pipefail
while IFS=\$'\t' read -r repo workflow signing current; do
  [ -n "\$repo" ] || continue
  if [ "\$repo" = "\$1" ] && [ "\$workflow" = "\$2" ]; then
    printf '%s\t%s\n' "\$signing" "\$current"
    exit 0
  fi
done <"$table"
exit 7
STUB
  chmod +x "$path"
  printf '%s\n' "$path"
}

# Every consumer this repository actually carries, resolved from the real manifests, so
# a fixture cannot silently drift out of step with discovery.
consumers="$("$SCRIPT" --list-consumers 2>/dev/null || true)"
if [ -z "$consumers" ]; then
  fail '--list-consumers produced nothing; every case below would pass vacuously'
  printf '\n%d failure(s)\n' "$((failures + 1))" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 1. AGREEING: signing revision == current pin for every consumer.
#    The script must NOT report a divergence.
# ---------------------------------------------------------------------------
agree_table="$WORK/agree.tsv"
: >"$agree_table"
while IFS=$'\t' read -r repo workflow _version; do
  [ -n "$repo" ] || continue
  printf '%s\t%s\t%s\t%s\n' "$repo" "$workflow" \
    "1111111111111111111111111111111111111111" \
    "1111111111111111111111111111111111111111" >>"$agree_table"
done <<<"$consumers"

agree_out="$WORK/agree.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
  "$SCRIPT" >"$agree_out" 2>&1; then
  if grep -q 'DIVERGED' "$agree_out"; then
    fail 'identical revisions were reported as DIVERGED — the check cannot distinguish the two states'
  else
    pass 'matching signing revision and current pin are not reported as diverged'
  fi
  if ! grep -q 'IN-SYNC' "$agree_out"; then
    fail 'the agreeing case produced no IN-SYNC line, so a silent no-op would look identical'
  else
    pass 'the agreeing case is positively reported, not merely silent'
  fi
else
  fail "the script exited non-zero on the all-agreeing fixture: $(cat "$agree_out")"
fi

# ---------------------------------------------------------------------------
# 2. DIFFERING: one consumer's deployed artifact was signed by a revision it no
#    longer pins. This is the measured two-behind case (#3048) in miniature, and
#    it is the whole reason the script exists.
# ---------------------------------------------------------------------------
first_repo="$(printf '%s\n' "$consumers" | head -1 | cut -f1)"
diff_table="$WORK/diff.tsv"
: >"$diff_table"
while IFS=$'\t' read -r repo workflow _version; do
  [ -n "$repo" ] || continue
  if [ "$repo" = "$first_repo" ]; then
    printf '%s\t%s\t%s\t%s\n' "$repo" "$workflow" \
      "2222222222222222222222222222222222222222" \
      "3333333333333333333333333333333333333333" >>"$diff_table"
  else
    printf '%s\t%s\t%s\t%s\n' "$repo" "$workflow" \
      "1111111111111111111111111111111111111111" \
      "1111111111111111111111111111111111111111" >>"$diff_table"
  fi
done <<<"$consumers"

diff_out="$WORK/diff.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$diff_table" diff)" \
  "$SCRIPT" >"$diff_out" 2>&1; then
  grep -q 'DIVERGED' "$diff_out" ||
    fail 'a consumer whose signing revision differs from its current pin was NOT reported as DIVERGED'
  grep -q "$first_repo" "$diff_out" ||
    fail "the diverged consumer ($first_repo) is not named in the output"
  # The allow-list this feeds has to accept BOTH revisions, so both must appear.
  grep -q '2222222222222222222222222222222222222222' "$diff_out" ||
    fail 'the signing revision is missing from the output, so an allow-list cannot be built from it'
  grep -q '3333333333333333333333333333333333333333' "$diff_out" ||
    fail 'the current pin is missing from the output, so an allow-list cannot be built from it'
  [ "$failures" -eq 0 ] && pass 'a diverged consumer is reported with both revisions named'
else
  fail "the script exited non-zero on the diverging fixture: $(cat "$diff_out")"
fi

# ---------------------------------------------------------------------------
# 3. DISCOVERY FLOOR. An empty or shrunken result from a filtered read is a claim
#    about the FILTER, not about the repository. If the manifests move, are
#    renamed, or adopt a spelling discovery does not match, the script must FAIL
#    rather than report a clean, empty portfolio — the same fail-closed property
#    guard-shared-publish-workflow-pin.sh carries for the same reason.
# ---------------------------------------------------------------------------
floor_out="$WORK/floor.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
  PUBLISH_CONSUMER_ROOT="$WORK/empty" "$SCRIPT" >"$floor_out" 2>&1; then
  fail 'discovery over an EMPTY tree exited 0 — an unreadable repository would report as clean'
else
  pass 'discovery over an empty tree fails closed instead of reporting a clean portfolio'
fi

# ---------------------------------------------------------------------------
# 4. RESOLUTION FAILURE. A consumer the resolver cannot answer for must fail the
#    run. Reporting "no divergence" because a lookup errored is the same
#    fail-open as an empty read, one layer down.
# ---------------------------------------------------------------------------
partial_table="$WORK/partial.tsv"
grep -v "^$first_repo	" "$agree_table" >"$partial_table" || true
partial_out="$WORK/partial.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$partial_table" partial)" \
  "$SCRIPT" >"$partial_out" 2>&1; then
  fail 'a consumer the resolver could not answer for did not fail the run'
else
  grep -q "$first_repo" "$partial_out" ||
    fail 'the unresolvable consumer is not named in the failure, so the cause is not diagnosable'
  pass 'an unresolvable consumer fails the run and is named'
fi

# ---------------------------------------------------------------------------
# 5. ARTIFACT NAME vs SOURCE REPOSITORY. `.github` cannot be an OCI path component,
#    so its artifact ships as `github-config`. Deriving the repository from the URL
#    alone therefore names a repository that does not exist, and every lookup for
#    that consumer fails — which the report would show as UNRESOLVED, i.e. a healthy
#    consumer reported as unreadable. Pin the translation so it cannot be dropped.
# ---------------------------------------------------------------------------
if printf '%s\n' "$consumers" | cut -f1 | grep -qxF '.github'; then
  pass 'the github-config artifact is attributed to its real source repository (.github)'
else
  fail 'github-config is not mapped to .github — its revisions cannot be resolved at all'
fi
if printf '%s\n' "$consumers" | cut -f1 | grep -qxF 'github-config'; then
  fail 'the OCI artifact name github-config leaked through as a repository name; no such repository exists'
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
printf '\nPASS: publish-workflow signing-revision report (5 cases)\n'
