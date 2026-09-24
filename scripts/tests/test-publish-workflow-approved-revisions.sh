#!/usr/bin/env bash
# RED/GREEN coverage for `generate-publish-workflow-approved-revisions.sh` (#3550).
#
# WHAT IS ACTUALLY BEING PROVED
# The generator's one harmful failure mode is writing an approved set that LOOKS complete
# while a consumer is missing from it or carries one revision where two are needed —
# whoever narrows the cosign matchers from that file cannot tell. Most cases below are
# therefore about refusal, not the happy path: an unresolved consumer must fail the run and
# leave no file behind, a one-revision answer must be refused by name, and regenerating on
# unchanged inputs must be byte-identical so a guard can diff the committed file.
#
# WHY THERE ARE TWO SEAMS
# The applied revision is a cluster read and the pin is a network read; a test depending on
# either proves nothing repeatable. The generator takes its observer from
# APPROVED_REVISION_OBSERVER and its pin resolver from APPROVED_REVISION_PIN_RESOLVER;
# discovery stays REAL against this repository's manifests, through the report.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly SCRIPT="$REPO_ROOT/scripts/generate-publish-workflow-approved-revisions.sh"
readonly REPORT="$REPO_ROOT/scripts/report-publish-workflow-signing-revisions.sh"

readonly SHA_A='1111111111111111111111111111111111111111'
readonly SHA_B='2222222222222222222222222222222222222222'
readonly SHA_C='3333333333333333333333333333333333333333'
readonly SHA_R='4444444444444444444444444444444444444444'  # release candidate (#3960)
readonly DIGEST_A='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

consumers="$("$REPORT" --list-consumers 2>/dev/null || true)"
if [ -z "$consumers" ]; then
  fail '--list-consumers produced nothing; every case below would pass vacuously'
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
consumer_count="$(printf '%s\n' "$consumers" | grep -c .)"
first_consumer="$(printf '%s\n' "$consumers" | head -1 | cut -f1)"
first_workflow="$(printf '%s\n' "$consumers" | head -1 | cut -f2)"

# A stub observer: "<repo> <workflow>" → the table's "<tag>\t<digest>\t<signer>" for that
# consumer, exit 7 when the table has no row (an unresolvable consumer).
make_observer() { # <table> <name>
  local path="$WORK/observer-$2.sh"
  cat >"$path" <<STUB
#!/usr/bin/env bash
set -euo pipefail
while IFS=\$'\t' read -r repo workflow answer; do
  [ -n "\$repo" ] || continue
  if [ "\$repo" = "\$1" ] && [ "\$workflow" = "\$2" ]; then
    printf '%b\n' "\$answer"
    exit 0
  fi
done <"$1"
exit 7
STUB
  chmod +x "$path"
  printf '%s\n' "$path"
}
# A stub pin resolver: every consumer pins SHA_B, except an optional named repo that fails.
make_pin_resolver() { # <name> <failing-repo|"">
  local path="$WORK/pin-$1.sh"
  cat >"$path" <<STUB
#!/usr/bin/env bash
[ "\$1" = "$2" ] && exit 7
printf '%s\n' "$SHA_B"
STUB
  chmod +x "$path"
  printf '%s\n' "$path"
}
# A stub release resolver: appends one line per call to <name>.calls, then prints <answer>
# (with `\n` escapes expanded) and exits <status>.
make_release_resolver() { # <name> <answer> <status>
  local path="$WORK/release-$1.sh"
  cat >"$path" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$1" >>"$WORK/release-$1.calls"
printf '%b\n' '$2'
exit $3
STUB
  chmod +x "$path"
  printf '%s\n' "$path"
}
release_ok="$(make_release_resolver ok "$SHA_R" 0)"
# Observer table: every consumer answers "<tag> <digest> <signer>", with one optional
# override "<repo>\t<workflow>\t<answer-with-\\t-escapes>" and one optional omitted repo.
write_table() { # <path> <override-repo|""> <override-answer> <omit-repo|"">
  local path="$1" override="$2" answer="$3" omit="$4" repo workflow _version
  : >"$path"
  while IFS=$'\t' read -r repo workflow _version; do
    [ -n "$repo" ] || continue
    [ "$repo" = "$omit" ] && continue
    if [ "$repo" = "$override" ]; then
      printf '%s\t%s\t%s\n' "$repo" "$workflow" "$answer" >>"$path"
    else
      printf '%s\t%s\t%s\n' "$repo" "$workflow" "1.2.3\\t${DIGEST_A}\\t${SHA_A}" >>"$path"
    fi
  done <<<"$consumers"
}

run_gen() { # <observer> <pin-resolver> <output-file> <observed-on> <log> [<release-resolver>]
  APPROVED_REVISION_OBSERVER="$1" APPROVED_REVISION_PIN_RESOLVER="$2" \
    APPROVED_REVISION_RELEASE_RESOLVER="${6:-$release_ok}" \
    APPROVED_REVISIONS_FILE="$3" APPROVED_REVISIONS_OBSERVED_ON="$4" "$SCRIPT" >"$5" 2>&1
}

# --- 1. Every consumer resolves: one row each, both revisions, exit 0 -------------------
full_table="$WORK/full.tsv"; write_table "$full_table" '' '' ''
observer_full="$(make_observer "$full_table" full)"
pin_ok="$(make_pin_resolver ok '')"
out1="$WORK/out1.tsv"; log1="$WORK/log1"
if run_gen "$observer_full" "$pin_ok" "$out1" 2026-01-01 "$log1"; then
  rows="$(tail -n +2 "$out1" | grep -c . || true)"
  if [ "$rows" -eq "$consumer_count" ]; then
    pass "complete run writes one row per consumer ($rows)"
  else
    fail "complete run wrote $rows row(s) for $consumer_count consumer(s)"; cat "$out1"
  fi
  if head -1 "$out1" | grep -qxF -- $'consumer\tworkflow\tapplied_tag\tapplied_digest\tapplied_signer_sha\tmain_pin_sha\trelease_candidate_sha\tobserved_on'; then
    pass 'header names the eight columns'
  else
    fail "unexpected header: $(head -1 "$out1")"
  fi
  if tail -n +2 "$out1" | awk -F'\t' -v s="$SHA_A" -v p="$SHA_B" -v d="$DIGEST_A" \
    -v r="$SHA_R" \
    'NF != 8 || $3 != "1.2.3" || $4 != d || $5 != s || $6 != p || $8 != "2026-01-01" { bad = 1 }
     $2 == "publish-app" && $7 != r { bad = 1 } $2 != "publish-app" && $7 != "-" { bad = 1 } END { exit bad }'; then
    pass 'every row carries tag, digest, signer, pin, candidate and date in the expected columns'
  else
    fail 'a row is malformed'; cat "$out1"
  fi
else
  fail "complete run exited non-zero"; cat "$log1"
fi

# --- 2. Byte-identical regeneration: unchanged tuples keep their date --------------------
cp "$out1" "$WORK/out1.before"
if run_gen "$observer_full" "$pin_ok" "$out1" 2026-02-02 "$WORK/log2" && cmp -s "$out1" "$WORK/out1.before"; then
  pass 'regenerating on unchanged inputs is byte-identical (dates preserved, no re-stamp)'
else
  fail 'regeneration changed the file'; diff "$WORK/out1.before" "$out1" || true
fi

# --- 3. A changed tuple is re-stamped; unchanged rows keep their date -------------------
moved_table="$WORK/moved.tsv"; write_table "$moved_table" "$first_consumer" "1.2.4\\t${DIGEST_A}\\t${SHA_C}" ''
observer_moved="$(make_observer "$moved_table" moved)"
if run_gen "$observer_moved" "$pin_ok" "$out1" 2026-03-03 "$WORK/log3"; then
  moved_date="$(awk -F'\t' -v c="$first_consumer" -v w="$first_workflow" '$1 == c && $2 == w { print $8 }' "$out1")"
  kept="$(awk -F'\t' -v c="$first_consumer" 'NR > 1 && $1 != c && $8 != "2026-01-01" { bad = 1 } END { exit bad }' "$out1" && echo yes || echo no)"
  if [ "$moved_date" = '2026-03-03' ] && [ "$kept" = yes ]; then
    pass 'a moved signer re-stamps only its own row'
  else
    fail "re-stamp wrong: moved row date=$moved_date, other rows kept=$kept"; cat "$out1"
  fi
else
  fail 'moved-tuple run exited non-zero'; cat "$WORK/log3"
fi

# --- 4. AC3: a consumer the observer cannot resolve FAILS the run and writes NOTHING -----
omit_table="$WORK/omit.tsv"; write_table "$omit_table" '' '' "$first_consumer"
observer_omit="$(make_observer "$omit_table" omit)"
out4="$WORK/out4.tsv"
if run_gen "$observer_omit" "$pin_ok" "$out4" 2026-01-01 "$WORK/log4"; then
  fail 'an unresolvable consumer did not fail the run'
else
  if [ ! -e "$out4" ] && grep -qF -- "$first_consumer" "$WORK/log4"; then
    pass 'an unresolvable consumer fails the run, is named, and no file is written'
  else
    fail "unresolvable consumer: file exists=$([ -e "$out4" ] && echo yes || echo no), named=$(grep -cF -- "$first_consumer" "$WORK/log4" || true)"
    cat "$WORK/log4"
  fi
fi
# ...and an EXISTING file is left untouched, not truncated, by a refused run.
cp "$WORK/out1.before" "$out4"
run_gen "$observer_omit" "$pin_ok" "$out4" 2026-01-01 "$WORK/log4b" || true
if cmp -s "$out4" "$WORK/out1.before"; then
  pass 'a refused run leaves the existing file untouched'
else
  fail 'a refused run modified the existing file'
fi

# --- 5. A set of ONE revision is refused by consumer name --------------------------------
one_table="$WORK/one.tsv"; write_table "$one_table" "$first_consumer" "1.2.3\\t${DIGEST_A}\\t" ''
observer_one="$(make_observer "$one_table" one)"
out5="$WORK/out5.tsv"
if run_gen "$observer_one" "$pin_ok" "$out5" 2026-01-01 "$WORK/log5"; then
  fail 'an empty signer (set of one) was accepted'
else
  if grep -qF -- "$first_consumer" "$WORK/log5" && [ ! -e "$out5" ]; then
    pass 'an empty signer is refused naming the consumer, no file written'
  else
    fail 'empty-signer refusal did not name the consumer or wrote a file'; cat "$WORK/log5"
  fi
fi
pin_fail="$(make_pin_resolver fail "$first_consumer")"
out6="$WORK/out6.tsv"
if run_gen "$observer_full" "$pin_fail" "$out6" 2026-01-01 "$WORK/log6"; then
  fail 'a missing pin (set of one) was accepted'
else
  if grep -qF -- "$first_consumer" "$WORK/log6" && [ ! -e "$out6" ]; then
    pass 'a missing pin is refused naming the consumer, no file written'
  else
    fail 'missing-pin refusal did not name the consumer or wrote a file'; cat "$WORK/log6"
  fi
fi

# --- 6. Malformed observer answers are refused, never parsed on their first line ---------
for shape in "1.2.3\\t${DIGEST_A}\\t${SHA_A}\\nextra" "1.2.3\\tnot-a-digest\\t${SHA_A}" "1.2.3\\t${DIGEST_A}\\tdeadbeef" "1.2.3\\t\\t${SHA_A}"; do
  bad_table="$WORK/bad.tsv"; write_table "$bad_table" "$first_consumer" "$shape" ''
  observer_bad="$(make_observer "$bad_table" bad)"
  out7="$WORK/out7.tsv"; rm -f "$out7"
  if run_gen "$observer_bad" "$pin_ok" "$out7" 2026-01-01 "$WORK/log7"; then
    fail "malformed observer answer accepted: $shape"
  else
    if [ ! -e "$out7" ]; then
      pass "malformed observer answer refused: $shape"
    else
      fail "malformed answer refused but a file was written: $shape"
    fi
  fi
done

# --- 7. Without a kube context the default observer fails closed ------------------------
out8="$WORK/out8.tsv"
if env -u PUBLISH_KUBE_CONTEXT APPROVED_REVISION_PIN_RESOLVER="$pin_ok" APPROVED_REVISIONS_FILE="$out8" \
  APPROVED_REVISIONS_OBSERVED_ON=2026-01-01 "$SCRIPT" >"$WORK/log8" 2>&1; then
  fail 'default observer produced a set with no cluster to observe'
else
  if grep -q 'PUBLISH_KUBE_CONTEXT' "$WORK/log8" && [ ! -e "$out8" ]; then
    pass 'default observer refuses without a kube context, no file written'
  else
    fail 'no-context refusal was not explained or wrote a file'; cat "$WORK/log8"
  fi
fi

# --- 8. A bad date override is refused before anything runs -----------------------------
if APPROVED_REVISIONS_OBSERVED_ON=yesterday APPROVED_REVISION_OBSERVER="$observer_full" \
  APPROVED_REVISION_PIN_RESOLVER="$pin_ok" APPROVED_REVISIONS_FILE="$WORK/out9.tsv" "$SCRIPT" >"$WORK/log9" 2>&1; then
  fail 'a non-date APPROVED_REVISIONS_OBSERVED_ON was accepted'
else
  pass 'a non-date APPROVED_REVISIONS_OBSERVED_ON is refused'
fi

# --- 9. The release candidate (#3960) ---------------------------------------------------
app_consumer="$(printf '%s\n' "$consumers" | awk -F'\t' '$2 == "publish-app" { print $1; exit }')"
if [ -z "$app_consumer" ]; then
  fail 'discovery found no publish-app consumer; every release-candidate case would pass vacuously'
else
  # An unreadable release fails the run by consumer name and writes nothing.
  release_down="$(make_release_resolver down '' 7)"
  out10="$WORK/out10.tsv"
  if run_gen "$observer_full" "$pin_ok" "$out10" 2026-01-01 "$WORK/log10" "$release_down"; then
    fail 'an unreadable release was accepted'
  elif [ ! -e "$out10" ] && grep -qF -- "$app_consumer" "$WORK/log10" && grep -qF -- 'release candidate' "$WORK/log10"; then
    pass 'an unreadable release fails the run naming the consumer, no file written'
  else
    fail 'unreadable-release refusal did not name the consumer or wrote a file'; cat "$WORK/log10"
  fi
  # A failed read is still the one attempt: later publish-app rows must not retry it.
  app_rows="$(printf '%s\n' "$consumers" | awk -F'\t' '$2 == "publish-app"' | grep -c .)"
  down_calls="$(grep -c . "$WORK/release-down.calls" || true)"
  if [ "$app_rows" -lt 2 ]; then
    fail "only $app_rows publish-app consumer(s); the single-attempt case would pass vacuously"
  elif [ "$down_calls" -eq 1 ]; then
    pass "an unreadable release is attempted once across $app_rows publish-app rows"
  else
    fail "an unreadable release was attempted $down_calls time(s) across $app_rows publish-app rows"
  fi

  # Malformed release answers are refused, never parsed on their first line.
  for shape in 'deadbeef' "${SHA_R}\\n${SHA_C}" '-'; do
    release_bad="$(make_release_resolver bad "$shape" 0)"
    out11="$WORK/out11.tsv"; rm -f "$out11"
    if run_gen "$observer_full" "$pin_ok" "$out11" 2026-01-01 "$WORK/log11" "$release_bad"; then
      fail "malformed release answer accepted: $shape"
    elif [ ! -e "$out11" ]; then
      pass "malformed release answer refused: $shape"
    else
      fail "malformed release answer refused but a file was written: $shape"
    fi
  done

  # One shared fact: the release is read once however many publish-app consumers there are.
  release_once="$(make_release_resolver once "$SHA_R" 0)"
  out12="$WORK/out12.tsv"
  if run_gen "$observer_full" "$pin_ok" "$out12" 2026-01-01 "$WORK/log12" "$release_once"; then
    calls="$(grep -c . "$WORK/release-once.calls" || true)"
    if [ "$calls" -eq 1 ] && grep -qxF publish-app "$WORK/release-once.calls"; then
      pass 'the release is resolved once, for the publish-app workflow'
    else
      fail "the release resolver ran $calls time(s): $(tr '\n' ' ' <"$WORK/release-once.calls")"
    fi
  else
    fail 'a resolvable release failed the run'; cat "$WORK/log12"
  fi

  # A new release moves only the publish-app rows, which are re-stamped; the others keep
  # their date because their tuple did not move.
  release_new="$(make_release_resolver new "$SHA_C" 0)"
  if run_gen "$observer_full" "$pin_ok" "$out12" 2026-04-04 "$WORK/log13" "$release_new" &&
    tail -n +2 "$out12" | awk -F'\t' -v n="$SHA_C" '
      $2 == "publish-app" && ($7 != n || $8 != "2026-04-04") { bad = 1 }
      $2 != "publish-app" && ($7 != "-" || $8 != "2026-01-01") { bad = 1 } END { exit bad }'; then
    pass 'a new release re-stamps only the publish-app rows'
  else
    fail 'a new release did not move exactly the publish-app rows'; cat "$out12"
  fi
fi

# ── The default signer lookup says WHY it refused (#4127) ─────────────────────────────────
# The daily run refused `.github` four times on inputs that resolved an hour later or when
# run locally, and every refusal read the same: "no single successful CD run published <tag>".
# A failed read, a listing that did not contain the run, and a tag that was genuinely never
# signed were indistinguishable, so the cause could not be established. These cases drive
# the real `signer_for_tag` against a stubbed `gh`, in a separate process because sourcing
# the generator declares read-only names this test also uses.
#
# The driver serves its fixtures in order, one per runs read, repeating the last; it counts
# the reads in "<driver>.reads" so a case can assert how many listings were taken (#4128).
make_signer_driver() { # <runs-fixture|FAIL>...
  local path="$WORK/signer-driver-$RANDOM.sh" fixtures="$*"
  cat >"$path" <<DRIVER
#!/usr/bin/env bash
set -euo pipefail
source "$SCRIPT"
sleep() { :; }  # both lookups back off between reads; the stubs answer deterministically
reads_file='$path.reads'
printf '0' >"\$reads_file"
gh() {
  case "\$*" in
    *'/commits/v9.9.9'*) printf '%s' '$SHA_A' ;;
    *'/actions/runs'*)
      local n fixture
      n=\$((\$(cat "\$reads_file") + 1))
      printf '%s' "\$n" >"\$reads_file"
      set -- $fixtures
      [ "\$n" -le "\$#" ] && fixture="\${!n}" || fixture="\${!#}"
      [ "\$fixture" != FAIL ] || return 1
      cat "\$fixture" ;;
    *) return 1 ;;
  esac
}
signer_for_tag consumer-x publish-manifests v9.9.9
DRIVER
  printf '%s\n' "$path"
}

empty_fixture() {
  local path="$WORK/runs-empty.json"
  printf '%s\n' '{"total_count": 0, "workflow_runs": []}' >"$path"
  printf '%s\n' "$path"
}

runs_fixture() { # <name> <head_sha> <conclusion>
  local path="$WORK/runs-$1.json"
  cat >"$path" <<JSON
{"total_count": 1, "workflow_runs": [{"head_branch": "v9.9.9", "path": ".github/workflows/cd.yaml",
  "head_sha": "$2", "conclusion": "$3",
  "referenced_workflows": [{"path": "devantler-tech/actions/.github/workflows/publish-manifests.yaml@$SHA_B", "sha": "$SHA_B"}]}]}
JSON
  printf '%s\n' "$path"
}

# The resolving case: exactly one successful run at the tag's commit names one signer.
driver="$(make_signer_driver "$(runs_fixture ok "$SHA_A" success)")"
if out="$(bash "$driver" 2>"$WORK/signer-ok.err")" && [ "$out" = "$SHA_B" ] && [ ! -s "$WORK/signer-ok.err" ]; then
  pass 'the signer lookup resolves one successful run at the tag commit, silently'
else
  fail "the signer lookup did not resolve a signed tag (out='${out:-}')"; cat "$WORK/signer-ok.err" >&2
fi

# A failed runs read is named as a failed read, never as a missing run.
driver="$(make_signer_driver FAIL)"
if bash "$driver" >/dev/null 2>"$WORK/signer-fail.err"; then
  fail 'a failed runs read resolved a signer'
elif grep -q 'runs read for v9.9.9 failed' "$WORK/signer-fail.err" &&
  ! grep -q 'at_commit=' "$WORK/signer-fail.err"; then
  pass 'a failed runs read is refused as a failed read'
else
  fail 'a failed runs read was not named as one'; cat "$WORK/signer-fail.err" >&2
fi

# A listing without the matching run prints what it DID contain, so a late or incomplete
# listing can be told apart from a tag that was never signed.
driver="$(make_signer_driver "$(runs_fixture moved "$SHA_C" success)")"
if bash "$driver" >/dev/null 2>"$WORK/signer-miss.err"; then
  fail 'a run at another commit was attributed as the signer'
elif grep -q 'total_count=1 returned=1 for_tag=1 cd=1 at_commit=0 successful=0 signer_refs=0' \
  "$WORK/signer-miss.err"; then
  pass 'a listing without the matching run is refused with its bounded counts'
else
  fail 'a listing without the matching run did not report its counts'; cat "$WORK/signer-miss.err" >&2
fi

# The counts track the conclusion too: a run at the right commit that did not succeed.
driver="$(make_signer_driver "$(runs_fixture failed "$SHA_A" failure)")"
if bash "$driver" >/dev/null 2>"$WORK/signer-concl.err"; then
  fail 'a failed CD run was attributed as the signer'
elif grep -q 'at_commit=1 successful=0 signer_refs=0 conclusions=failure' "$WORK/signer-concl.err"; then
  pass 'a failed CD run at the tag commit is refused with its conclusion'
else
  fail 'a failed CD run did not report its conclusion'; cat "$WORK/signer-concl.err" >&2
fi

# A transiently EMPTY listing is read again, and the run it then shows is taken (#4128).
driver="$(make_signer_driver "$(empty_fixture)" "$(runs_fixture ok "$SHA_A" success)")"
if out="$(bash "$driver" 2>"$WORK/signer-late.err")" && [ "$out" = "$SHA_B" ] &&
  [ "$(cat "$driver.reads")" = 2 ] && grep -q 'resolved on read 2 of 3' "$WORK/signer-late.err"; then
  pass 'a transiently empty listing is re-read and the late run resolves, logged'
else
  fail "a transiently empty listing did not recover (out='${out:-}', reads=$(cat "$driver.reads"))"
  cat "$WORK/signer-late.err" >&2
fi

# A listing that stays empty is still refused, after exactly the bounded number of reads.
driver="$(make_signer_driver "$(empty_fixture)")"
if bash "$driver" >/dev/null 2>"$WORK/signer-empty.err"; then
  fail 'a persistently empty listing resolved a signer'
elif [ "$(cat "$driver.reads")" = 3 ] &&
  grep -q 'after 3 reads: total_count=0 returned=0' "$WORK/signer-empty.err"; then
  pass 'a persistently empty listing is refused after 3 reads'
else
  fail "a persistently empty listing was not refused after 3 reads (reads=$(cat "$driver.reads"))"
  cat "$WORK/signer-empty.err" >&2
fi

if [ "$failures" -gt 0 ]; then
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
printf '\nall approved-revisions cases passed\n'
