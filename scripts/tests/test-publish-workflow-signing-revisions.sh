#!/usr/bin/env bash
# RED/GREEN coverage for `report-publish-workflow-signing-revisions.sh` (#3048).
#
# WHAT IS ACTUALLY BEING PROVED
# The script answers one question per consumer: "is the revision that SIGNED what is
# deployed the same revision this consumer pins TODAY?" An allow-list built for #2818 has
# to accept both, so a DIFFERENCE must be surfaced and a MATCH must not.
#
# The script's only harmful failure mode is reporting a clean, in-sync portfolio while
# having checked nothing or the wrong thing — so most cases below are aimed at that, not
# at the happy path.
#
# WHY THERE IS A RESOLVER SEAM
# Resolving a revision needs the network. A test that depended on it would prove nothing
# repeatable: red when an unrelated repository cut a release, green when the network was
# simply unavailable. The script takes its resolver from PUBLISH_REVISION_RESOLVER and
# these fixtures supply a stub. Discovery stays REAL, against this repository's own
# manifests, because that is the half a stub could hide a regression in.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly SCRIPT="$REPO_ROOT/scripts/report-publish-workflow-signing-revisions.sh"

readonly SHA_A='1111111111111111111111111111111111111111'
readonly SHA_B='2222222222222222222222222222222222222222'
readonly SHA_C='3333333333333333333333333333333333333333'

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# A stub resolver, handed <repo> <workflow> <version>, printing "<signing>\t<current>".
# Each fixture writes its answers into a table the stub reads.
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

consumers="$("$SCRIPT" --list-consumers 2>/dev/null || true)"
if [ -z "$consumers" ]; then
  fail '--list-consumers produced nothing; every case below would pass vacuously'
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi

write_table() { # <path> <special-repo|""> <special-signing> <special-current>
  local table="$1" special="$2" s_sign="$3" s_cur="$4" repo workflow
  : >"$table"
  while IFS=$'\t' read -r repo workflow _version; do
    [ -n "$repo" ] || continue
    if [ -n "$special" ] && [ "$repo" = "$special" ]; then
      printf '%s\t%s\t%s\t%s\n' "$repo" "$workflow" "$s_sign" "$s_cur" >>"$table"
    else
      printf '%s\t%s\t%s\t%s\n' "$repo" "$workflow" "$SHA_A" "$SHA_A" >>"$table"
    fi
  done <<<"$consumers"
}

first_repo="$(printf '%s\n' "$consumers" | head -1 | cut -f1)"

# ---------------------------------------------------------------------------
# 1. AGREEING: identical revisions must NOT be reported as diverged, and must be
#    positively reported — a silent no-op would otherwise look the same.
# ---------------------------------------------------------------------------
agree_table="$WORK/agree.tsv"
write_table "$agree_table" '' '' ''
agree_out="$WORK/agree.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" "$SCRIPT" >"$agree_out" 2>&1; then
  grep -q 'DIVERGED' "$agree_out" &&
    fail 'identical revisions were reported as DIVERGED — the check cannot distinguish the two states'
  grep -q 'IN-SYNC' "$agree_out" ||
    fail 'the agreeing case produced no IN-SYNC line, so a silent no-op would look identical'
  grep -q '0 unresolved' "$agree_out" ||
    fail 'the agreeing case reports unresolved consumers'
  [ "$failures" -eq 0 ] && pass 'matching revisions report IN-SYNC and never DIVERGED'
else
  fail "the script exited non-zero on the all-agreeing fixture: $(cat "$agree_out")"
fi

# ---------------------------------------------------------------------------
# 2. DIFFERING: the measured two-behind case (#3048) in miniature. Both revisions
#    must be named, because the allow-list has to accept both.
# ---------------------------------------------------------------------------
diff_table="$WORK/diff.tsv"
write_table "$diff_table" "$first_repo" "$SHA_B" "$SHA_C"
diff_out="$WORK/diff.out"
before=$failures
if PUBLISH_REVISION_RESOLVER="$(make_stub "$diff_table" diff)" "$SCRIPT" >"$diff_out" 2>&1; then
  grep -q 'DIVERGED' "$diff_out" ||
    fail 'a consumer whose signing revision differs from its current pin was NOT reported as DIVERGED'
  grep -q -- "$first_repo" "$diff_out" || fail "the diverged consumer ($first_repo) is not named"
  grep -q "$SHA_B" "$diff_out" ||
    fail 'the signing revision is missing, so an allow-list cannot be built from it'
  grep -q "$SHA_C" "$diff_out" ||
    fail 'the current pin is missing, so an allow-list cannot be built from it'
  [ "$failures" -eq "$before" ] && pass 'a diverged consumer is reported with both revisions named'
else
  fail "the script exited non-zero on the diverging fixture: $(cat "$diff_out")"
fi

# ---------------------------------------------------------------------------
# 3. DISCOVERY over a real tree that matches NOTHING. An empty result from a filtered
#    read is a claim about the FILTER. This deliberately uses a populated directory of
#    non-matching YAML rather than a missing one, so it exercises the "manifests moved,
#    were renamed, or changed spelling" path and not merely a `[ -d ]` guard.
# ---------------------------------------------------------------------------
nomatch_root="$WORK/nomatch"
mkdir -p "$nomatch_root"
cat >"$nomatch_root/unrelated.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: nothing-to-see
data:
  note: "no cosign subject here"
YAML
nomatch_out="$WORK/nomatch.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
PUBLISH_CONSUMER_ROOT="$nomatch_root" "$SCRIPT" >"$nomatch_out" 2>&1; then
  fail 'discovery over a tree matching nothing exited 0 — a moved manifest would report as clean'
else
  grep -q 'not discovered' "$nomatch_out" ||
    fail 'the discovery failure does not say which consumers went missing'
  pass 'a tree matching nothing fails closed instead of reporting a clean portfolio'
fi

# ---------------------------------------------------------------------------
# 4. RIGHT SIZE, WRONG MEMBERSHIP. This is why the floor is an identity check and not a
#    count: one real consumer dropping out while any other file drops in leaves the total
#    unchanged, so a count-based floor passes and the vanished consumer is never checked.
# ---------------------------------------------------------------------------
swap_root="$WORK/swap"
mkdir -p "$swap_root"
i=0
while IFS=$'\t' read -r repo workflow _version; do
  [ -n "$repo" ] || continue
  i=$((i + 1))
  # Substitute an impostor for one real consumer, keeping the total identical.
  name="$repo"
  [ "$repo" = "$first_repo" ] && name='impostor-repo'
  cat >"$swap_root/consumer-$i.yaml" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: c$i
spec:
  url: oci://ghcr.io/devantler-tech/$name/manifests
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\\.actions\\.githubusercontent\\.com$'
        subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/$workflow\\.yaml@[0-9a-f]{40}\$'
YAML
done <<<"$consumers"
swap_out="$WORK/swap.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
PUBLISH_CONSUMER_ROOT="$swap_root" "$SCRIPT" >"$swap_out" 2>&1; then
  fail 'a discovery set of the right SIZE but wrong MEMBERSHIP passed — a consumer can vanish silently'
else
  grep -q -- "$first_repo" "$swap_out" ||
    fail "the missing consumer ($first_repo) is not named in the discovery failure"
  pass 'a same-size discovery set missing a real consumer fails closed and names it'
fi

# ---------------------------------------------------------------------------
# 5. A RESOLVER ANSWER THAT IS NOT TWO SHAs. `cut -f2` without `-s` echoes the WHOLE line
#    when the delimiter is absent, so a resolver emitting one tab-free line — exactly the
#    shape of a `gh api` error body — made signing == current and reported IN-SYNC for
#    every consumer with exit 0. That is the worst failure this script can have.
# ---------------------------------------------------------------------------
junk_resolver="$WORK/resolver-junk.sh"
cat >"$junk_resolver" <<'JUNK'
#!/usr/bin/env bash
printf '%s\n' '{"message":"Not Found","status":"404"}'
exit 1
JUNK
chmod +x "$junk_resolver"
junk_out="$WORK/junk.out"
if PUBLISH_REVISION_RESOLVER="$junk_resolver" "$SCRIPT" >"$junk_out" 2>&1; then
  fail 'a resolver emitting a tab-free error body exited 0 — every consumer read as IN-SYNC'
else
  grep -q 'IN-SYNC' "$junk_out" &&
    fail 'an unparseable resolver answer was reported as IN-SYNC rather than UNRESOLVED'
  grep -q 'UNRESOLVED' "$junk_out" ||
    fail 'an unparseable resolver answer produced no UNRESOLVED line'
  grep -q 'FAILED' "$junk_out" ||
    fail 'the failure explanation is absent from stdout, so the CI step summary would end on a clean-looking tally'
  pass 'a resolver answer that is not two SHAs is UNRESOLVED, fails the run, and explains itself on stdout'
fi

# ---------------------------------------------------------------------------
# 6. ONE UNRESOLVABLE CONSUMER MUST NOT HIDE THE OTHERS. The script collects and fails at
#    the end precisely so a single lookup failure does not suppress the consumers that did
#    resolve; nothing proved that until now.
# ---------------------------------------------------------------------------
partial_table="$WORK/partial.tsv"
awk -F'\t' -v r="$first_repo" '$1 != r' "$agree_table" >"$partial_table"
expected_insync=$(($(printf '%s\n' "$consumers" | grep -c .) - 1))
partial_out="$WORK/partial.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$partial_table" partial)" "$SCRIPT" >"$partial_out" 2>&1; then
  fail 'a consumer the resolver could not answer for did not fail the run'
else
  grep -q -- "$first_repo" "$partial_out" ||
    fail 'the unresolvable consumer is not named, so the cause is not diagnosable'
  got_insync="$(grep -c '^IN-SYNC' "$partial_out" || true)"
  [ "$got_insync" -eq "$expected_insync" ] ||
    fail "one unresolvable consumer suppressed the others: expected $expected_insync IN-SYNC, got $got_insync"
  pass 'an unresolvable consumer fails the run, is named, and does not hide the consumers that resolved'
fi

# ---------------------------------------------------------------------------
# 7. ARTIFACT NAME vs SOURCE REPOSITORY. `.github` cannot be an OCI path component, so its
#    artifact ships as `github-config`. Deriving the repository from the URL alone names a
#    repository that does not exist, so every lookup for a healthy consumer fails.
# ---------------------------------------------------------------------------
if printf '%s\n' "$consumers" | cut -f1 | grep -qxF -- '.github'; then
  pass 'the github-config artifact is attributed to its real source repository (.github)'
else
  fail 'github-config is not mapped to .github — its revisions cannot be resolved at all'
fi
if printf '%s\n' "$consumers" | cut -f1 | grep -qxF -- 'github-config'; then
  fail 'the OCI artifact name github-config leaked through as a repository name; no such repository exists'
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
printf '\nPASS: publish-workflow signing-revision report (7 cases)\n'
