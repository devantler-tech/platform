#!/usr/bin/env bash
# The KSV-01010 dispositions in .trivyignore.yaml are PATH-SCOPED, and this test is what keeps that
# scoping meaningful.
#
# WHY THIS EXISTS
# KSV-01010 "ConfigMap with sensitive content" matches key and value TEXT, so it cannot tell a
# public ACME contact address from a credential. Three ConfigMaps are dispositioned (#3240):
#
#   k8s/clusters/{prod,local}/bootstrap/config-map.yaml  — matched on admin_email, the ACME
#                                                          registration contact the cluster issuer
#                                                          requires. Public by construction.
#   k8s/bases/apps/actual-budget/config-map.yaml         — matched on the identifier names in an
#                                                          embedded script that READS credentials
#                                                          from an ESO-mounted Secret.
#
# The premise is that the EXCEPTION IS THE PATH, not the content. If the scoping ever stopped
# deciding — a glob widened to k8s/**, the id skipped outright — then a genuinely secret-bearing
# application ConfigMap would be suppressed too, and nothing else would fail: the scan still runs
# and the count does not move, which reads exactly like nothing happened.
#
# So this asserts the boundary with a PAIRED CONTROL: the same bytes at an excepted path and at a
# non-excepted one. Same content, different path, so only the path can explain a difference.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly IGNOREFILE="${REPO_ROOT}/.trivyignore.yaml"
readonly SOURCE_CM="${REPO_ROOT}/k8s/clusters/prod/bootstrap/config-map.yaml"
readonly EXCEPTED_PATH="k8s/clusters/prod/bootstrap/config-map.yaml"
readonly PROBE_PATH="k8s/bases/apps/trivyignore-boundary-probe/config-map.yaml"
readonly CHECK_ID="KSV-01010"

# The two content-premise detectors, defined once so the self-test below exercises the SAME
# expressions the file loop uses. A self-test against a copied regex proves nothing about the
# regex that actually runs.
readonly OPAQUE_VALUE_RE='^[[:space:]]*[A-Za-z0-9_.-]+:[[:space:]]*"?[A-Za-z0-9+/=_.-]{40,}"?[[:space:]]*$'
readonly ISSUED_PREFIX_RE='^[[:space:]]*[A-Za-z0-9_.-]+:[[:space:]]*"?(gh[pousr]_|github_pat_|xox[abprs]-|sk-[A-Za-z0-9]|AKIA[A-Z0-9]{8}|ASIA[A-Z0-9]{8}|eyJ[A-Za-z0-9_-]{8,}\.)'

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --- Layer 1: structural checks. These need only yq, so they still hold in a CI job that has no
# --- trivy — which is exactly where the behavioural control below would otherwise skip silently.
command -v yq >/dev/null 2>&1 || fail "yq is required to check the KSV-01010 disposition boundary"

readonly EXPECTED_PATHS="k8s/bases/apps/actual-budget/config-map.yaml
k8s/clusters/local/bootstrap/config-map.yaml
k8s/clusters/prod/bootstrap/config-map.yaml"

entry_count="$(yq "[.misconfigurations[] | select(.id == \"$CHECK_ID\")] | length" "$IGNOREFILE" 2>/dev/null || printf '0')"
[ "$entry_count" -gt 0 ] || fail \
  "no $CHECK_ID disposition found in $IGNOREFILE — if the findings were fixed at the manifest \
instead, delete this test with them"

# Every entry must be path-scoped. An entry with no paths key skips the id repository-wide.
while IFS= read -r n; do
  [ "$n" -gt 0 ] || fail \
    "a $CHECK_ID entry has no paths key, so the check is skipped repository-wide rather than \
scoped to the three reviewed ConfigMaps"
done < <(yq ".misconfigurations[] | select(.id == \"$CHECK_ID\") | (.paths // []) | length" "$IGNOREFILE")

# The scoped set must be exactly the three reviewed files. A widened glob (k8s/**) or a new path
# added without review both land here.
actual_paths="$(yq -r ".misconfigurations[] | select(.id == \"$CHECK_ID\") | .paths[]" "$IGNOREFILE" | sort)"
if [ "$actual_paths" != "$EXPECTED_PATHS" ]; then
  fail "$CHECK_ID scope changed. Each path is excepted because its content was reviewed, so a new
or widened path needs its own reason recorded in $IGNOREFILE.
expected:
$EXPECTED_PATHS
actual:
$actual_paths"
fi

echo "PASS(structural): $CHECK_ID is path-scoped to exactly the 3 reviewed ConfigMaps."

# --- Layer 1b: content premises. Path scoping decides WHICH files are excepted; it says nothing
# --- about what those files may later contain. A credential added to one of them would be
# --- suppressed by an exception written when the file held none — the disposition outliving its
# --- premise. These are shape checks, so they need no scanner and always run.
premise_failures=0
while IFS= read -r cm; do
  [ -f "$REPO_ROOT/$cm" ] || fail "excepted file is missing: $cm"

  # A private key pasted into an excepted ConfigMap is the concrete risk. PEM is unambiguous.
  if grep -qE -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$REPO_ROOT/$cm"; then
    echo "FAIL: $cm contains a PEM private key block, but $CHECK_ID is excepted for it" >&2
    premise_failures=$((premise_failures + 1))
  fi

  # A long unbroken run as a VALUE is the usual shape of a pasted credential. The character class
  # includes _ . - because modern tokens are base64url or prefixed (ghp_…, github_pat_…, JWTs); a
  # class limited to standard base64 misses every one of them. That gap is not cosmetic: a token
  # dropped into an EXISTING reviewed key leaves the key set unchanged, so the matched-key
  # assertion below would not catch it either, and the file would keep its exception while
  # holding live credential material.
  if grep -qE "$OPAQUE_VALUE_RE" "$REPO_ROOT/$cm"; then
    echo "FAIL: $cm assigns a long opaque value that looks like credential material, but \
$CHECK_ID is excepted for it" >&2
    premise_failures=$((premise_failures + 1))
  fi

  # Length alone is not sufficient — several issued-credential formats are shorter than 40
  # characters (xoxb- Slack tokens, sk- API keys). These prefixes are issuance markers rather than
  # an entropy guess, so they are matched at any length.
  if grep -qE "$ISSUED_PREFIX_RE" "$REPO_ROOT/$cm"; then
    echo "FAIL: $cm assigns a value carrying an issued-credential prefix, but \
$CHECK_ID is excepted for it" >&2
    premise_failures=$((premise_failures + 1))
  fi
done <<EOF
$EXPECTED_PATHS
EOF

[ "$premise_failures" -eq 0 ] || fail \
  "$premise_failures content-premise violation(s). Each of these files is excepted from $CHECK_ID \
because it was reviewed and found to hold no credential material. Move the value to a Secret, or \
re-review the exception."

echo "PASS(structural): content premises hold — no PEM block or opaque credential-shaped value."

# --- Layer 1c: detector self-test. The premise checks above are only worth their PASS if they
# --- actually fire, and the failure mode is silent: a detector that stops matching reports the
# --- same "premises hold" line as one that is working.
# ---
# --- Each detector is asserted SEPARATELY. An OR'd assertion looks equivalent and is not: the
# --- prefix detector catches ghp_/xoxb-/JWT fixtures on its own, so it would keep the suite green
# --- while the opaque-value class was silently narrowed back — the exact regression this layer
# --- exists to catch. `base64url blob` is the discriminating fixture: no issued prefix, so only
# --- the opaque class can match it.
selftest_failures=0
selftest_probe="$(mktemp)"

# args: <expect_opaque yes|no> <expect_prefix yes|no> <label> <value>
check_fixture() {
  local want_opaque="$1" want_prefix="$2" label="$3" value="$4"
  printf '  admin_email: "%s"\n' "$value" >"$selftest_probe"
  local got_opaque=no got_prefix=no
  grep -qE "$OPAQUE_VALUE_RE" "$selftest_probe" && got_opaque=yes
  grep -qE "$ISSUED_PREFIX_RE" "$selftest_probe" && got_prefix=yes
  if [ "$got_opaque" != "$want_opaque" ] || [ "$got_prefix" != "$want_prefix" ]; then
    echo "FAIL(selftest): $label — opaque expected=$want_opaque got=$got_opaque; \
prefix expected=$want_prefix got=$got_prefix" >&2
    selftest_failures=$((selftest_failures + 1))
  fi
}

#             opaque prefix  label                    value
check_fixture yes    yes     "github ghp_ token"      'ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'
check_fixture yes    yes     "github fine-grained"    'github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz123456'
check_fixture no     yes     "slack bot token"        'xoxb-123456789012-abcdef'
check_fixture no     yes     "api key sk-"            'sk-proj-abc123'
check_fixture no     yes     "aws access key id"      'AKIAIOSFODNN7EXAMPLE'
check_fixture yes    yes     "jwt"                    'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij'
check_fixture yes    no      "long base64 blob"       'QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWY='
check_fixture yes    no      "base64url blob"         'dGVzdF92YWx1ZS13aXRoX3VuZGVyc2NvcmVzLWFuZC1kYXNoZXM_zQ'

# MUST NOT be detected by either — the real reviewed values these exceptions exist for.
check_fixture no     no      "acme contact address"   'ned@devantler.tech'
check_fixture no     no      "public domain"          'platform.devantler.tech'
check_fixture no     no      "public client id"       'Iv23limfvbk93bAXZI6b'

rm -f "$selftest_probe"

[ "$selftest_failures" -eq 0 ] || fail \
  "$selftest_failures detector self-test failure(s). The content-premise checks above cannot be \
trusted until these pass — a detector that no longer matches reports success identically."
echo "PASS(selftest): both content detectors pin their own coverage (8 credential fixtures, 3 cleared)."

# --- Layer 2: behavioural paired control. Needs trivy; skips (loudly) where it is unavailable.
command -v trivy >/dev/null 2>&1 || {
  echo "SKIP(behavioural): trivy not installed; structural checks above still passed"
  exit 0
}
[ -r "$IGNOREFILE" ] || fail "ignorefile not readable: $IGNOREFILE"
[ -r "$SOURCE_CM" ] || fail "source ConfigMap not readable: $SOURCE_CM"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$(dirname "$EXCEPTED_PATH")" "$WORK/$(dirname "$PROBE_PATH")"
cp "$SOURCE_CM" "$WORK/$EXCEPTED_PATH"
cp "$SOURCE_CM" "$WORK/$PROBE_PATH"
cp "$IGNOREFILE" "$WORK/.trivyignore.yaml"

# All three excepted files, so each one's own matched-key set can be asserted below. The paired
# control above only ever exercises the prod bootstrap ConfigMap's content.
while IFS= read -r cm; do
  mkdir -p "$WORK/$(dirname "$cm")"
  cp "$REPO_ROOT/$cm" "$WORK/$cm"
done <<EOF
$EXPECTED_PATHS
EOF
[ -d "${REPO_ROOT}/.trivy/data" ] && {
  mkdir -p "$WORK/.trivy"
  cp -R "${REPO_ROOT}/.trivy/data" "$WORK/.trivy/data"
}

# Byte-identical is the whole point of a paired control — assert it rather than trusting cp.
if ! cmp -s "$WORK/$EXCEPTED_PATH" "$WORK/$PROBE_PATH"; then
  fail "paired control is not paired: the two copies differ, so a path conclusion would be unfounded"
fi

# Count CHECK_ID findings at one target path. Emits a bare integer.
count_at() {
  local json="$1" target="$2"
  jq -r --arg t "$target" --arg id "$CHECK_ID" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id) ] | length' \
    "$json"
}

scan() {
  local out="$1"
  shift
  (cd "$WORK" && trivy fs --scanners misconfig --config-data .trivy/data --format json "$@" .) >"$out" 2>/dev/null
}

# --- Ablation first: WITHOUT the ignorefile both copies must report, or the test proves nothing. ---
scan "$WORK/no-ignore.json"
BASE_EXCEPTED="$(count_at "$WORK/no-ignore.json" "$EXCEPTED_PATH")"
readonly BASE_EXCEPTED
BASE_PROBE="$(count_at "$WORK/no-ignore.json" "$PROBE_PATH")"
readonly BASE_PROBE

[ "$BASE_EXCEPTED" -gt 0 ] || fail \
  "VACUOUS: without the ignorefile, $CHECK_ID does not fire at $EXCEPTED_PATH. The check no longer \
matches this content (a trivy rule change?), so suppressing it below would prove nothing."
[ "$BASE_PROBE" -gt 0 ] || fail \
  "VACUOUS: without the ignorefile, $CHECK_ID does not fire at $PROBE_PATH."

# --- With the ignorefile: the excepted path is suppressed, the probe is NOT. ---
scan "$WORK/with-ignore.json" --ignorefile .trivyignore.yaml
SCOPED_EXCEPTED="$(count_at "$WORK/with-ignore.json" "$EXCEPTED_PATH")"
readonly SCOPED_EXCEPTED
SCOPED_PROBE="$(count_at "$WORK/with-ignore.json" "$PROBE_PATH")"
readonly SCOPED_PROBE

[ "$SCOPED_EXCEPTED" -eq 0 ] || fail \
  "the disposition does not cover $EXCEPTED_PATH ($SCOPED_EXCEPTED finding(s) still reported)"

[ "$SCOPED_PROBE" -gt 0 ] || fail \
  "BOUNDARY BREACHED: identical content at the non-excepted path $PROBE_PATH is ALSO suppressed. \
The $CHECK_ID disposition has stopped being path-scoped, so a genuinely secret-bearing application \
ConfigMap would now be hidden. Re-scope the entry in .trivyignore.yaml."

echo "PASS: $CHECK_ID is path-scoped."
echo "  without ignorefile : excepted=$BASE_EXCEPTED  probe=$BASE_PROBE   (ablation fired)"
echo "  with    ignorefile : excepted=$SCOPED_EXCEPTED  probe=$SCOPED_PROBE   (same bytes, path decides)"

# --- Content premise, behaviourally: WHICH keys the check matches in each excepted file. ---
# Path scoping and shape checks both stop short of this. trivy names the matching keys in its
# message, so the reviewed premise can be asserted directly: if a credential key is ever added to
# one of these files, the matched set changes and this fails — instead of being silently excepted.
#
# Reviewed sets (trivy 0.74.0), whitespace-normalised, deduplicated and sorted:
readonly REVIEWED_KEYS_prod="admin_email"
readonly REVIEWED_KEYS_local="admin_email"
readonly REVIEWED_KEYS_actual="const secretKey,enablebanking_secretKey"

# Extract the brace-delimited key list trivy reports for one target, normalise it, and join.
matched_keys_at() {
  local json="$1" target="$2"
  jq -r --arg t "$target" --arg id "$CHECK_ID" \
    '[ .Results[]? | select(.Target == $t) | .Misconfigurations[]? | select(.ID == $id) | .Message ] | join(" ")' \
    "$json" |
    grep -oE '\{[^}]*\}' |
    tr ',' '\n' |
    sed -e 's/[{}"]//g' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' |
    sed '/^$/d' |
    sort -u |
    paste -sd, -
}

TREEWIDE="$(jq -r --arg id "$CHECK_ID" '[ .Results[]?.Misconfigurations[]? | select(.ID == $id) ] | length' "$WORK/no-ignore.json")"
readonly TREEWIDE

if [ "$TREEWIDE" -eq 0 ]; then
  # The running trivy does not implement this check at all (CI pins an older version than the one
  # these sets were reviewed with). Nothing to assert, and nothing is wrong — say so rather than
  # failing, and rather than passing silently.
  echo "SKIP(premise): $CHECK_ID is not emitted by this trivy version; matched-key sets not asserted."
else
  premise_behavioural_failures=0
  for pair in \
    "k8s/clusters/prod/bootstrap/config-map.yaml:$REVIEWED_KEYS_prod" \
    "k8s/clusters/local/bootstrap/config-map.yaml:$REVIEWED_KEYS_local" \
    "k8s/bases/apps/actual-budget/config-map.yaml:$REVIEWED_KEYS_actual"; do
    cm="${pair%%:*}"
    want="${pair#*:}"
    got="$(matched_keys_at "$WORK/no-ignore.json" "$cm")"
    if [ "$got" != "$want" ]; then
      echo "FAIL: $cm — $CHECK_ID matches a different key set than was reviewed." >&2
      echo "  reviewed: $want" >&2
      echo "  actual  : $got" >&2
      premise_behavioural_failures=$((premise_behavioural_failures + 1))
    fi
  done

  [ "$premise_behavioural_failures" -eq 0 ] || fail \
    "the exception for the file(s) above was written against a reviewed key set that no longer \
matches. A new key means new content this disposition was never reviewed to cover."

  echo "PASS(premise): each excepted file matches exactly the reviewed key set."
fi
