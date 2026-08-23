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
  # The set comparison runs in BOTH directions, and the unregistered side fires first here: the
  # impostor is not in EXPECTED_CONSUMERS, which is itself the stronger objection — a consumer this
  # script does not know about is one whose later disappearance it could not notice. Either half
  # naming its cause is a pass; silence is not.
  grep -qE -- "impostor-repo|$first_repo" "$swap_out" ||
    fail "the discovery failure names neither the unregistered consumer nor the missing one"
  grep -qE -- 'not registered|not discovered' "$swap_out" ||
    fail "the discovery failure does not say WHICH direction of the set comparison failed"
  pass 'a same-size discovery set with an unregistered member fails closed and names the cause'
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
# exit 0 ON PURPOSE. A FAILING resolver is discarded by the caller (`answer=$(...) || answer=""`),
# so only a SUCCEEDING resolver reaches the tab-free-answer parsing path this case exists to test.
# With `exit 1` the case proved only that an EMPTY answer is UNRESOLVED — a different, weaker claim.
printf '%s\n' '{"message":"Not Found","status":"404"}'
exit 0
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

# ---------------------------------------------------------------------------
# 8. AN EXTRA, UNREGISTERED CONSUMER. This isolates the OTHER direction of the set
#    comparison: every expected consumer is present, so the "missing" check cannot fire
#    and only the unregistered check can. Without this the swap fixture above passes on
#    either direction and the unregistered half is never actually exercised — which is
#    exactly what an ablation showed before this case existed.
#
#    It matters because an unregistered consumer is one whose later disappearance this
#    script could not notice: it would drop out and every registered name would still be
#    present, so the run would exit clean.
# ---------------------------------------------------------------------------
extra_root="$WORK/extra"
mkdir -p "$extra_root"
j=0
while IFS=$'\t' read -r repo workflow _version; do
  [ -n "$repo" ] || continue
  j=$((j + 1))
  # `.github` is the artifact name `github-config` on the wire; emit the OCI name so the
  # script's own mapping reproduces the real repository, exactly as in production.
  oci="$repo"
  [ "$repo" = '.github' ] && oci='github-config'
  cat >"$extra_root/real-$j.yaml" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: r$j
spec:
  url: oci://ghcr.io/devantler-tech/$oci/manifests
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\\.actions\\.githubusercontent\\.com\$'
        subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/$workflow\\.yaml@[0-9a-f]{40}\$'
YAML
done <<<"$consumers"
cat >"$extra_root/extra.yaml" <<'YAML'
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: newcomer
spec:
  url: oci://ghcr.io/devantler-tech/newcomer/manifests
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\.actions\.githubusercontent\.com$'
        subject: '^https://github\.com/devantler-tech/actions/\.github/workflows/publish-app\.yaml@[0-9a-f]{40}$'
YAML
extra_out="$WORK/extra.out"
if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
PUBLISH_CONSUMER_ROOT="$extra_root" "$SCRIPT" >"$extra_out" 2>&1; then
  fail 'an unregistered sixth consumer was accepted — its later disappearance could not be noticed'
else
  grep -q 'not registered' "$extra_out" ||
    fail 'the failure does not identify the unregistered direction of the set comparison'
  grep -q 'newcomer' "$extra_out" ||
    fail 'the unregistered consumer is not named, so registering it needs guesswork'
  grep -q 'not discovered' "$extra_out" &&
    fail 'the run reported a MISSING consumer; this fixture has all five, so the case is not isolating'
  pass 'an extra unregistered consumer fails closed and is named'
fi

# ---------------------------------------------------------------------------
# 9. A BOUNDED SEMVER CONSTRAINT MUST REFUSE, NOT GUESS. An unbounded `>=1.0.0` and
#    "whatever is newest" happen to agree, which is why discarding the constraint looked
#    harmless. A bounded selector (`~1.4`, `<2.0.0`) does not agree: the newest published
#    tag is one Flux would never serve, so its workflow revision would be attributed to
#    the deployed artifact and the real signer omitted from the allow-list.
#
#    Resolving such a range needs Flux-compatible semver selection this script does not
#    implement, so the honest outcome is a named refusal.
# ---------------------------------------------------------------------------
# Each selector below is bounded and must refuse BY NAME. `~1.4` is the simple form;
# `>=1.0.0 <2.0.0` is the compound one, which matters because a lower-bound-prefix
# test accepts it — the upper bound is simply not looked at — and the script would
# then treat a bounded range as unbounded and resolve it to the newest tag.
bounded_case() {
  bc_label=$1
  bc_selector=$2
  bc_root="$WORK/bounded-$bc_label"
  mkdir -p "$bc_root"
  k=0
  while IFS=$'\t' read -r repo workflow _version; do
    [ -n "$repo" ] || continue
    k=$((k + 1))
    oci="$repo"
    [ "$repo" = '.github' ] && oci='github-config'
    # One consumer gets the BOUNDED range; the rest keep an unbounded one, so the
    # refusal cannot be confused with a wholesale failure of the fixture.
    if [ "$k" -eq 1 ]; then ref="semver: \"$bc_selector\""; else ref='semver: ">=1.0.0"'; fi
    cat >"$bc_root/c-$k.yaml" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: b$k
spec:
  ref:
    $ref
  url: oci://ghcr.io/devantler-tech/$oci/manifests
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: '^https://token\\.actions\\.githubusercontent\\.com\$'
        subject: '^https://github\\.com/devantler-tech/actions/\\.github/workflows/$workflow\\.yaml@[0-9a-f]{40}\$'
YAML
  done <<<"$consumers"
  bc_out="$WORK/bounded-$bc_label.out"
  # The stub is safe here BECAUSE the refusal is decided from the manifest, before any
  # resolver is consulted. When this check lived inside the resolver, the only way to
  # reach it was to let the real one run — which made this case hit the network and fail
  # in CI for an unrelated reason. A hermetic case that reaches the branch is strictly
  # better than a live one that does.
  if PUBLISH_REVISION_RESOLVER="$(make_stub "$agree_table" agree)" \
  PUBLISH_CONSUMER_ROOT="$bc_root" "$SCRIPT" >"$bc_out" 2>&1; then
    fail "a bounded semver constraint ($bc_selector) was silently resolved to the newest tag instead of refusing"
  else
    grep -q 'bounded semver constraint' "$bc_out" ||
      fail "the run failed but not because of the bounded constraint ($bc_selector) — the case is not testing what it claims"
    grep -qF "$bc_selector" "$bc_out" ||
      fail "the refusal does not name the constraint ($bc_selector), so the cause is not diagnosable"
    pass "a bounded semver constraint refuses by name instead of guessing the newest tag: $bc_selector"
  fi
}

bounded_case simple '~1.4'
# A COMPOUND range beginning with a lower bound. The whole point: it *starts* like the
# unbounded `>=1.0.0` that is legitimately allowed, so any test looking only at the
# prefix accepts it and discards the `<2.0.0` — attributing a 2.x tag's workflow
# revision to an artifact Flux would never deploy, and omitting the real signer.
bounded_case compound '>=1.0.0 <2.0.0'

# ---------------------------------------------------------------------------
# 10. AN EMPTY MIDDLE FIELD MUST NOT SHIFT `origin` INTO `current`. (#3305 review)
#     Tab is IFS WHITESPACE, so `IFS=$'\t' read -r signing current origin` COLLAPSES
#     adjacent tabs: an answer of `SHA_A<TAB><TAB>SHA_A` assigned SHA_A to BOTH signing
#     and current and reported IN-SYNC — a confident wrong answer produced by the parser
#     rather than the network, which is the one outcome this script must never have.
#     Written without arrays or `read -d`, so it holds on the Bash 3.2 that ships on macOS.
# ---------------------------------------------------------------------------
collapse_resolver="$WORK/resolver-collapse.sh"
cat >"$collapse_resolver" <<COLLAPSE
#!/usr/bin/env bash
# exit 0 on purpose: a FAILING resolver is discarded by the caller, so only a SUCCEEDING
# one reaches the field-splitting path this case exists to test.
printf '%s\t\t%s\n' '$SHA_A' '$SHA_A'
exit 0
COLLAPSE
chmod +x "$collapse_resolver"
collapse_out="$WORK/collapse.out"
if PUBLISH_REVISION_RESOLVER="$collapse_resolver" "$SCRIPT" >"$collapse_out" 2>&1; then
  fail 'an answer with an EMPTY MIDDLE FIELD exited 0 — the collapsed field was compared as the current revision'
else
  grep -q 'IN-SYNC' "$collapse_out" &&
    fail 'SHA<TAB><TAB>SHA was reported as IN-SYNC — adjacent tabs collapsed and the origin field became the current one'
  grep -q 'UNRESOLVED' "$collapse_out" ||
    fail 'an answer with an empty middle field produced no UNRESOLVED line'
  pass 'an empty middle field is UNRESOLVED, never a comparison against the shifted field'
fi

# ---------------------------------------------------------------------------
# 11. TRAILING OUTPUT AFTER A VALID LINE MUST NOT BE ACCEPTED UNSEEN. (#3305 review)
#     `read` stops at the first line, so a resolver emitting a good line followed by
#     anything at all was accepted on the strength of the part that parsed.
# ---------------------------------------------------------------------------
extra_resolver="$WORK/resolver-extra.sh"
cat >"$extra_resolver" <<EXTRA
#!/usr/bin/env bash
printf '%s\t%s\nunexpected trailing output\n' '$SHA_A' '$SHA_A'
exit 0
EXTRA
chmod +x "$extra_resolver"
extra_out="$WORK/extra.out"
if PUBLISH_REVISION_RESOLVER="$extra_resolver" "$SCRIPT" >"$extra_out" 2>&1; then
  fail 'a multi-line resolver answer exited 0 — only its first line was ever examined'
else
  grep -q 'IN-SYNC' "$extra_out" &&
    fail 'a valid first line with trailing output was reported as IN-SYNC'
  grep -q 'UNRESOLVED' "$extra_out" ||
    fail 'a multi-line resolver answer produced no UNRESOLVED line'
  pass 'trailing output after a valid line is UNRESOLVED, not accepted on the first line alone'
fi

# ---------------------------------------------------------------------------
# 12. FLUX REFERENCE PRECEDENCE: digest > semver > tag, omitted ref means `latest`.
#     (#3305 review) Reading `tag` first attributed a document carrying both a tag and a
#     digest to a tag Flux never serves; a digest or an omitted ref collapsed to the
#     meaningless `semver:`, which `effective_version` then refused as a bounded
#     constraint, so a resolvable consumer read as UNRESOLVED. Asserted against the
#     expression itself, because the selector choice is a property of the MANIFEST.
# ---------------------------------------------------------------------------
# 🔴 EXTRACTED FROM THE SCRIPT, NEVER RETYPED. A copy of the expression here would assert
# only that SOME expression is correct, and would keep passing while the one the script
# actually runs regressed — the test would pin nothing. The extraction is asserted
# non-empty below, so a rename or reformat fails the case loudly instead of silently
# testing an empty string.
# `|| true` is load-bearing: under `set -e` a no-match grep in a command substitution
# ABORTS the suite at this line, so the guard below never runs and the run dies with exit 1
# and NO diagnostic — a failure nobody can act on. Measured while ablating this case.
ref_expr="$(grep -o '(((\.spec\.ref\.digest.*"latest")' "$SCRIPT" || true)"
[ -n "$ref_expr" ] ||
  fail 'could not extract the reference-precedence expression from the script — the case would pass vacuously'
ref_case() { # <name> <ref-yaml-block> <expected>
  local name="$1" block="$2" expected="$3" doc="$WORK/ref-$1.yaml" got
  {
    printf 'kind: OCIRepository\nspec:\n  url: oci://ghcr.io/devantler-tech/x/manifests\n'
    [ -n "$block" ] && printf '%s\n' "$block"
  } >"$doc"
  got="$(yq eval -r "$ref_expr" "$doc" 2>&1)"
  [ "$got" = "$expected" ] ||
    fail "reference precedence ($name): expected [$expected], got [$got]"
}
ref_case tag '  ref:
    tag: v1.2.3' 'v1.2.3'
ref_case semver '  ref:
    semver: ">=1.0.0"' 'semver:>=1.0.0'
ref_case digest '  ref:
    digest: sha256:abcdef' 'digest:sha256:abcdef'
ref_case tag-and-digest '  ref:
    tag: v1.2.3
    digest: sha256:abcdef' 'digest:sha256:abcdef'
ref_case tag-and-semver '  ref:
    tag: v1.2.3
    semver: ">=1.0.0"' 'semver:>=1.0.0'
ref_case omitted '' 'latest'
pass 'Flux reference precedence is digest > semver > tag, and an omitted ref means latest'

if [ "$failures" -ne 0 ]; then
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
printf '\nPASS: publish-workflow signing-revision report (12 cases)\n'
