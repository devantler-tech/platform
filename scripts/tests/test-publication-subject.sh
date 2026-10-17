#!/usr/bin/env bash
# Publication uses one subject definition; an absent verifier input fails before tools run.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
action="$root/.github/actions/deploy-prod/publish-platform-manifests/action.yml"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
failures=0
assertions=0
check() {
  assertions=$((assertions + 1))
  if "$@"; then echo "ok $*"; else echo "FAIL $*"; failures=$((failures + 1)); fi
}
# shellcheck disable=SC2016 # A literal Actions expression, never a shell expansion.
reference='${{ steps.staging_reference.outputs.subject_name }}'
subject="$(yq -r '.runs.steps[] | select(.id == "staging_reference").env.SUBJECT_NAME' "$action")"

single_definition() {
  local file="$1" candidate count values
  candidate="$(yq -o=json "$file" | jq -er '[.runs.steps[] | select(.id == "staging_reference")] | select(length == 1) | .[0].env.SUBJECT_NAME | select(type == "string" and length > 0)')" || return 1
  count="$(grep -oF "$candidate" "$file" | wc -l | tr -d ' ')"
  [ "$count" = 1 ] || return 1
  values="$(yq -o=json "$file" | jq -er '[.runs.steps[] | select(.id == "cosign_sign" or .id == "generate_sbom" or .id == "verify_evidence" or .id == "verify_matcher_accepts_staged" or .id == "promote_latest") | .env.SUBJECT_NAME] + [.runs.steps[] | select(.id == "attest_sbom" or .id == "attest_provenance") | .with["subject-name"]] | select(length == 7) | .[]')" || return 1
  while IFS= read -r value; do [ "$value" = "$reference" ] || return 1; done <<<"$values"
}
check single_definition "$action"
cp "$action" "$scratch/duplicate.yml"
printf '\n# duplicate subject: %s\n' "$subject" >>"$scratch/duplicate.yml"
if single_definition "$scratch/duplicate.yml"; then check false; else check true; fi
cp "$action" "$scratch/renamed.yml"
yq -i '.runs.steps[] |= (select(.id == "staging_reference").env.SUBJECT_NAME = "example.invalid/renamed-artifact")' "$scratch/renamed.yml"
check single_definition "$scratch/renamed.yml"
cp "$action" "$scratch/retargeted.yml"
yq -i '.runs.steps[] |= (select(.id == "verify_evidence").env.SUBJECT_NAME = "example.invalid/wrong-artifact")' "$scratch/retargeted.yml"
if single_definition "$scratch/retargeted.yml"; then check false; else check true; fi

# Execute the actual staging shell after changing only its declared subject.
yq -r '.runs.steps[] | select(.id == "staging_reference").run' "$scratch/renamed.yml" >"$scratch/staging.sh"
SUBJECT_NAME=example.invalid/renamed-artifact GITHUB_SHA=fixture GITHUB_RUN_ID=42 GITHUB_RUN_ATTEMPT=3 GITHUB_OUTPUT="$scratch/output" bash "$scratch/staging.sh"
check grep -Fx 'subject_name=example.invalid/renamed-artifact' "$scratch/output"
check grep -Fx 'oci_ref=oci://example.invalid/renamed-artifact:staging-fixture-42-3' "$scratch/output"
check grep -Fx 'registry_ref=example.invalid/renamed-artifact:staging-fixture-42-3' "$scratch/output"

mkdir "$scratch/bin"
export SUBJECT_TEST_TRACE="$scratch/trace"
cat >"$scratch/bin/cosign" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SUBJECT_TEST_TRACE"
SH
cp "$scratch/bin/cosign" "$scratch/bin/gh"
chmod +x "$scratch/bin/cosign" "$scratch/bin/gh"
digest="sha256:$(printf 'a%.0s' {1..64})"
export PATH="$scratch/bin:$PATH" WORKFLOW_REF=devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/main EVIDENCE_READ_BACKOFF_SECONDS=0
for enforce in true false; do
  : >"$SUBJECT_TEST_TRACE"
  rc=0
  env -u SUBJECT_NAME ENFORCE="$enforce" bash "$root/scripts/verify-published-evidence.sh" "$digest" >"$scratch/gate.log" 2>&1 || rc=$?
  check test "$rc" -eq 2
  check test ! -s "$SUBJECT_TEST_TRACE"
  : >"$SUBJECT_TEST_TRACE"
  rc=0
  SUBJECT_NAME='' ENFORCE="$enforce" bash "$root/scripts/verify-published-evidence.sh" "$digest" >"$scratch/gate.log" 2>&1 || rc=$?
  check test "$rc" -eq 2
  check test ! -s "$SUBJECT_TEST_TRACE"
done
: >"$SUBJECT_TEST_TRACE"
rc=0
SUBJECT_NAME=example.invalid/renamed-artifact ENFORCE=true bash "$root/scripts/verify-published-evidence.sh" "$digest" >"$scratch/gate.log" 2>&1 || rc=$?
check test "$rc" -eq 0
check test "$(grep -Fc "example.invalid/renamed-artifact@$digest" "$SUBJECT_TEST_TRACE")" -eq 3
echo "$assertions assertions, $failures failures"
[ "$failures" -eq 0 ]
