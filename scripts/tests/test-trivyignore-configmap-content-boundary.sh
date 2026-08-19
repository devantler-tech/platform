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

fail() { echo "FAIL: $*" >&2; exit 1; }


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

# --- Layer 2: behavioural paired control. Needs trivy; skips (loudly) where it is unavailable.
command -v trivy >/dev/null 2>&1 || { echo "SKIP(behavioural): trivy not installed; structural checks above still passed"; exit 0; }
[ -r "$IGNOREFILE" ] || fail "ignorefile not readable: $IGNOREFILE"
[ -r "$SOURCE_CM" ]  || fail "source ConfigMap not readable: $SOURCE_CM"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/$(dirname "$EXCEPTED_PATH")" "$WORK/$(dirname "$PROBE_PATH")"
cp "$SOURCE_CM" "$WORK/$EXCEPTED_PATH"
cp "$SOURCE_CM" "$WORK/$PROBE_PATH"
cp "$IGNOREFILE" "$WORK/.trivyignore.yaml"
[ -d "${REPO_ROOT}/.trivy/data" ] && { mkdir -p "$WORK/.trivy"; cp -R "${REPO_ROOT}/.trivy/data" "$WORK/.trivy/data"; }

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
  local out="$1"; shift
  ( cd "$WORK" && trivy fs --scanners misconfig --config-data .trivy/data --format json "$@" . ) > "$out" 2>/dev/null
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
