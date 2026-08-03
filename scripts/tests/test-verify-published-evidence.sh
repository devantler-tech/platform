#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/verify-published-evidence.sh.
#
# The behaviour half drives the gate through its tool seam: cosign and gh are
# stubbed on PATH, so every verdict is exercised without a registry, a token, or
# a network. The wiring half asserts the gate is actually reached — a gate the
# publication action does not call, or calls AFTER the promotion, protects
# nothing, which is the failure mode the whole slice exists to prevent.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly gate="${root_dir}/scripts/verify-published-evidence.sh"
readonly action="${root_dir}/.github/actions/deploy-prod/publish-platform-manifests/action.yml"

good_digest="sha256:$(printf 'a%.0s' {1..64})"
readonly good_digest
readonly workflow_ref="devantler-tech/platform/.github/workflows/ci.yaml@refs/heads/main"

pass_count=0
fail_count=0

ok() {
  echo "  ✅ $1"
  pass_count=$((pass_count + 1))
}

bad() {
  echo "  ❌ $1"
  echo "     $2"
  fail_count=$((fail_count + 1))
}

# stub_dir builds a PATH shim whose cosign and gh exit with the given codes, so
# a verdict is chosen rather than discovered.
#
# Each stub also APPENDS its own argv to ${dir}/argv. The gate captures tool
# output into a per-check log it only prints on failure, so asserting on the
# gate's stdout cannot see what a passing check was actually asked to verify —
# and "it passed" is not the property under test here. The argv file is the seam
# that makes the pinned identity and issuer observable.
stub_dir() {
  local cosign_rc="$1" gh_rc="$2"
  local dir
  dir="$(mktemp -d)"

  cat >"${dir}/cosign" <<EOF
#!/usr/bin/env bash
echo "cosign \$*" >>"${dir}/argv"
echo "cosign stub: \$*"
exit ${cosign_rc}
EOF

  cat >"${dir}/gh" <<EOF
#!/usr/bin/env bash
echo "gh \$*" >>"${dir}/argv"
echo "gh stub: \$*"
exit ${gh_rc}
EOF

  chmod +x "${dir}/cosign" "${dir}/gh"
  echo "${dir}"
}

# run_gate invokes the gate with stubs and returns its exit code plus output.
#
# The digest default uses ${4-} rather than ${4:-}: an explicitly EMPTY digest is
# one of the inputs under test, and :- would silently substitute the good one,
# turning that case into a duplicate of the happy path.
run_gate() {
  local cosign_rc="$1" gh_rc="$2" enforce="$3" digest="${4-${good_digest}}"
  local dir
  dir="$(stub_dir "${cosign_rc}" "${gh_rc}")"

  set +e
  gate_output="$(
    PATH="${dir}:${PATH}" \
      ENFORCE="${enforce}" \
      WORKFLOW_REF="${workflow_ref}" \
      bash "${gate}" "${digest}" 2>&1
  )"
  gate_rc=$?
  set -e

  rm -rf "${dir}"
}

echo "verify-published-evidence: behaviour"

# --- All evidence present -----------------------------------------------------
run_gate 0 0 "false"
if [[ "${gate_rc}" == "0" ]]; then
  ok "complete evidence passes"
else
  bad "complete evidence passes" "exit ${gate_rc}: ${gate_output}"
fi

if grep -q "All published evidence verified" <<<"${gate_output}"; then
  ok "complete evidence reports success"
else
  bad "complete evidence reports success" "${gate_output}"
fi

# --- BOTH STATES of the rollout flag, on the SAME failing input ---------------
# This pair is the feature flag's contract: the verification runs identically in
# each state and only the consequence differs. A flag that changed whether the
# check ran would make the non-enforcing period prove nothing about the
# enforcing one.
run_gate 1 0 "false"
if [[ "${gate_rc}" == "0" ]]; then
  ok "non-enforcing: a missing signature does NOT fail the deploy"
else
  bad "non-enforcing: a missing signature does NOT fail the deploy" "exit ${gate_rc}"
fi

if grep -q "::warning::" <<<"${gate_output}"; then
  ok "non-enforcing: reports the failure as a warning"
else
  bad "non-enforcing: reports the failure as a warning" "${gate_output}"
fi

run_gate 1 0 "true"
if [[ "${gate_rc}" == "1" ]]; then
  ok "enforcing: a missing signature FAILS the deploy"
else
  bad "enforcing: a missing signature FAILS the deploy" "exit ${gate_rc}: ${gate_output}"
fi

if grep -q "::error::" <<<"${gate_output}"; then
  ok "enforcing: reports the failure as an error"
else
  bad "enforcing: reports the failure as an error" "${gate_output}"
fi

# --- Each evidence kind is load-bearing ---------------------------------------
# Without this, a gate that only ever ran cosign would pass every test above.
run_gate 0 1 "true"
if [[ "${gate_rc}" == "1" ]]; then
  ok "enforcing: missing ATTESTATIONS fail the deploy even when signed"
else
  bad "enforcing: missing attestations fail the deploy" "exit ${gate_rc}: ${gate_output}"
fi

if grep -q "SBOM attestation" <<<"${gate_output}" &&
  grep -q "provenance attestation" <<<"${gate_output}"; then
  ok "both attestation predicates are checked by name"
else
  bad "both attestation predicates are checked by name" "${gate_output}"
fi

# All three failures are reported in one run, so one fix per deploy is not
# required to discover the next.
run_gate 1 1 "false"
if [[ "$(grep -c '❌' <<<"${gate_output}")" == "3" ]]; then
  ok "every failing check is reported, not just the first"
else
  bad "every failing check is reported, not just the first" "${gate_output}"
fi

# --- A malformed digest is refused, in BOTH flag states -----------------------
# The non-enforcing flag must not soften this: verifying an empty or malformed
# digest would check the mutable tag instead of the staged bytes, which is the
# "verified the wrong thing" outcome the gate exists to rule out.
for enforce in "false" "true"; do
  run_gate 0 0 "${enforce}" "sha256:not-a-digest"
  if [[ "${gate_rc}" == "2" ]]; then
    ok "malformed digest is refused (ENFORCE=${enforce})"
  else
    bad "malformed digest is refused (ENFORCE=${enforce})" "exit ${gate_rc}"
  fi
done

run_gate 0 0 "false" ""
if [[ "${gate_rc}" == "2" ]]; then
  ok "empty digest is refused"
else
  bad "empty digest is refused" "exit ${gate_rc}"
fi

# --- The identity is pinned to the calling workflow ---------------------------
# Asserted against the recorded argv, not the gate's stdout: a PASSING check's
# tool output is captured into a log the gate never prints, so stdout cannot
# show what was verified. What matters is not that cosign was run, but what it
# was asked to prove.
dir="$(stub_dir 0 0)"
set +e
PATH="${dir}:${PATH}" ENFORCE="false" WORKFLOW_REF="${workflow_ref}" \
  bash "${gate}" "${good_digest}" >/dev/null 2>&1
set -e
argv="$(cat "${dir}/argv")"
rm -rf "${dir}"

if grep -q -- "--certificate-identity https://github.com/${workflow_ref}" <<<"${argv}"; then
  ok "signature is verified against the calling workflow's exact identity"
else
  bad "signature is verified against the calling workflow's exact identity" "${argv}"
fi

if grep -q -- "--certificate-oidc-issuer https://token.actions.githubusercontent.com" <<<"${argv}"; then
  ok "signature is verified against the GitHub Actions OIDC issuer"
else
  bad "signature is verified against the GitHub Actions OIDC issuer" "${argv}"
fi

# The verification must name the resolved digest, never the mutable tag.
if grep -q -- "@${good_digest}" <<<"${argv}" && ! grep -q ":latest" <<<"${argv}"; then
  ok "every check targets the digest, never the mutable tag"
else
  bad "every check targets the digest, never the mutable tag" "${argv}"
fi

# An absent identity must refuse rather than fall back to a wildcard: verifying
# that SOMEONE signed the bytes is not the property being asserted.
dir="$(stub_dir 0 0)"
set +e
PATH="${dir}:${PATH}" ENFORCE="false" WORKFLOW_REF="" GITHUB_WORKFLOW_REF="" \
  bash "${gate}" "${good_digest}" >/dev/null 2>&1
missing_identity_rc=$?
set -e
rm -rf "${dir}"

if [[ "${missing_identity_rc}" == "2" ]]; then
  ok "an unknown signing identity is refused, not wildcarded"
else
  bad "an unknown signing identity is refused, not wildcarded" "exit ${missing_identity_rc}"
fi

echo "verify-published-evidence: wiring"

# --- The gate is reached, and BEFORE the promotion ----------------------------
# Order is the half a static read CAN decide, and it is the half that matters
# here: a gate that runs after latest already moved has verified bytes
# production is already following.
if grep -q "scripts/verify-published-evidence.sh" "${action}"; then
  ok "the publication action calls the gate"
else
  bad "the publication action calls the gate" "no reference in ${action}"
fi

gate_line="$(grep -n "scripts/verify-published-evidence.sh" "${action}" | head -1 | cut -d: -f1)"
promote_line="$(grep -n "id: promote_latest" "${action}" | head -1 | cut -d: -f1)"
provenance_line="$(grep -n "id: attest_provenance" "${action}" | head -1 | cut -d: -f1)"

if [[ -n "${gate_line}" && -n "${promote_line}" ]] && ((gate_line < promote_line)); then
  ok "the gate runs BEFORE latest is promoted"
else
  bad "the gate runs BEFORE latest is promoted" "gate=${gate_line:-none} promote=${promote_line:-none}"
fi

if [[ -n "${gate_line}" && -n "${provenance_line}" ]] && ((provenance_line < gate_line)); then
  ok "the gate runs AFTER both attestations are written"
else
  bad "the gate runs AFTER both attestations are written" "provenance=${provenance_line:-none} gate=${gate_line:-none}"
fi

# The gate must verify the digest the run actually resolved. Passing anything
# else — most of all the mutable tag — would verify bytes other than the ones
# about to be promoted.
# shellcheck disable=SC2016  # the literal ${STAGING_DIGEST} is the text being matched
if grep -q 'verify-published-evidence.sh "\${STAGING_DIGEST}"' "${action}"; then
  ok "the gate is given the resolved staging digest"
else
  bad "the gate is given the resolved staging digest" "unexpected argument in ${action}"
fi

# The rollout ratchet: default-off on this landing, so the enforcing flip is a
# separate, deliberate change rather than something that arrives with it.
if grep -q "enforce-evidence-verification" "${action}"; then
  ok "enforcement is a declared input"
else
  bad "enforcement is a declared input" "no input in ${action}"
fi

echo
echo "passed=${pass_count} failed=${fail_count}"
[[ "${fail_count}" == "0" ]]
