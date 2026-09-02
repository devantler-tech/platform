#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/verify-matcher-accepts-digest.sh.
#
# The behaviour half drives the gate through its tool seam: `go` and `cosign`
# are stubbed on PATH, so every verdict is chosen rather than discovered, with
# no registry, token, network or Go toolchain involved.
#
# The wiring half asserts the gate is actually reached, and reached at the only
# moment that decides what production gets: after the signature and attestations
# exist, and BEFORE latest is promoted. A gate that runs after promotion has
# verified bytes production is already following — which is the failure this
# slice exists to prevent (#3289), and the same shape as #3005.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly gate="${root_dir}/scripts/verify-matcher-accepts-digest.sh"
readonly action="${root_dir}/.github/actions/deploy-prod/publish-platform-manifests/action.yml"

good_digest="sha256:$(printf 'a%.0s' {1..64})"
readonly good_digest
readonly real_issuer='^https://token\.actions\.githubusercontent\.com$'
readonly real_subject='^https://github\.com/devantler-tech/platform/\.github/workflows/.*$'

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

# refused asserts the gate REFUSED, not merely that something went wrong.
# A missing script exits 127 and a crashed one exits non-zero too, so a bare
# "rc != 0" check passes for the very reasons that mean nothing was tested —
# the vacuous-control trap this gate itself exists to close, reproduced in its
# own test. Require the gate to have produced its own diagnostic.
refused() {
  [[ "${gate_rc}" != "0" && "${gate_rc}" != "127" ]] &&
    printf "%s" "${gate_output}" | grep -q "::error::"
}

# stub_dir builds a PATH shim for `go` and `cosign`.
#
# The cosign stub keys its exit code on WHICH identity it was asked to verify:
# the gate's negative control passes a deliberately wrong subject, so a stub
# that returned one code for every call could not tell the positive check from
# the control and every case would collapse into the happy path.
#
# positive_codes is a space-separated sequence consumed one per positive call,
# which is what makes the liveness sandwich testable: the gate re-runs the
# positive check after the control, and "0 1" is an outage arriving exactly
# there. The last value repeats if the gate calls more often than expected.
#
# Every invocation appends its argv, so what the gate actually asked is
# observable rather than inferred from an exit code.
stub_dir() {
  local positive_codes="$1" negative_code="$2" go_json="$3" go_stderr="${4-}"
  local dir
  dir="$(mktemp -d)"

  printf '%s\n' "${positive_codes}" >"${dir}/positive_codes"
  printf '0\n' >"${dir}/positive_calls"

  cat >"${dir}/go" <<EOF
#!/usr/bin/env bash
echo "go \$*" >>"${dir}/argv"
printf '%s' '${go_stderr}' >&2
printf '%s' '${go_json}'
EOF

  cat >"${dir}/cosign" <<EOF
#!/usr/bin/env bash
echo "cosign \$*" >>"${dir}/argv"
# The bogus identity the gate's own negative control uses carries this marker.
if printf '%s' "\$*" | grep -q 'NOT-A-REAL-REPO'; then
  exit ${negative_code}
fi
n=\$(cat "${dir}/positive_calls")
n=\$((n + 1))
echo "\${n}" >"${dir}/positive_calls"
read -r -a codes <"${dir}/positive_codes"
idx=\$((n - 1))
if [ "\${idx}" -ge "\${#codes[@]}" ]; then
  idx=\$(( \${#codes[@]} - 1 ))
fi
exit "\${codes[\${idx}]}"
EOF

  chmod +x "${dir}/go" "${dir}/cosign"
  echo "${dir}"
}

# run_gate invokes the gate with stubs, capturing rc, output and the argv trace.
#
# The digest default uses ${4-} rather than ${4:-} because an explicitly EMPTY
# digest is one of the inputs under test; :- would substitute the good one and
# silently turn that case into a duplicate of the happy path.
run_gate() {
  local positive_codes="$1" negative_code="$2" go_json="$3" digest="${4-${good_digest}}" go_stderr="${5-}"
  local dir
  dir="$(stub_dir "${positive_codes}" "${negative_code}" "${go_json}" "${go_stderr}")"

  set +e
  gate_output="$(cd "${root_dir}" && PATH="${dir}:${PATH}" bash "${gate}" "${digest}" 2>&1)"
  gate_rc=$?
  set -e
  gate_argv="$(cat "${dir}/argv" 2>/dev/null || true)"

  rm -rf "${dir}"
}

# Built with jq so the fixture is valid JSON by construction. Hand-writing it is a
# trap: a regex like `\.` is a legal PCRE and an ILLEGAL JSON escape, so a
# hand-rolled fixture silently becomes the unparseable-payload case and the happy
# path never runs.
good_json="$(jq -nc --arg i "${real_issuer}" --arg s "${real_subject}" "{issuer:\$i,subject:\$s}")"
readonly good_json

echo "verify-matcher-accepts-digest: behaviour"

# --- Happy path ---------------------------------------------------------------
run_gate "0 0" 1 "${good_json}"
if [[ "${gate_rc}" == "0" ]]; then
  ok "a matcher that accepts the staged digest passes"
else
  bad "a matcher that accepts the staged digest passes" "exit ${gate_rc}: ${gate_output}"
fi

# The property the whole slice exists for: the gate must ask about the IMMUTABLE
# staged digest, never the mutable tag. Asserting the exit code cannot see this
# — a gate verifying :latest also passes here, which is exactly today's bug.
if printf '%s' "${gate_argv}" | grep -q -- "@${good_digest}"; then
  ok "the gate verifies the staged digest by reference"
else
  bad "the gate verifies the staged digest by reference" "argv: ${gate_argv}"
fi

if printf '%s' "${gate_argv}" | grep -qE 'cosign .*manifests:latest'; then
  bad "the gate does NOT verify the mutable tag" "argv mentions :latest — ${gate_argv}"
else
  ok "the gate does NOT verify the mutable tag"
fi

# The patterns must come out of the manifests, not be restated in the gate.
if printf '%s' "${gate_argv}" | grep -q -- '--print-matcher'; then
  ok "the identity patterns are read from the manifests"
else
  bad "the identity patterns are read from the manifests" "argv: ${gate_argv}"
fi

# --- The extractor's stderr must not contaminate the payload -------------------
# 🔴 REGRESSION GUARD (merge-queue eviction, 2026-09-02). The gate captured the
# extractor with `2>&1`, so anything `go run` wrote to stderr — module downloads
# on a cold build cache, toolchain notices — landed in FRONT of the JSON and jq
# parsed build noise instead of the matcher:
#
#   jq: parse error: Invalid numeric literal at line 1, column 3
#   ::error::the matcher payload is missing an issuer or subject, or is not JSON
#
# It survived 22 green cases and a full green PR because the `go` stub here wrote
# only stdout, and because ci.yaml's own caller — the same command, same args —
# correctly captures stdout alone. The gate runs solely inside the prod deploy,
# so nothing before the merge queue ever exercised it against a real toolchain.
#
# This case models what a real `go run` does: noise on stderr, payload on stdout.
run_gate "0 0" 1 "${good_json}" "${good_digest}" 'go: downloading github.com/example/mod v1.2.3'
if [[ "${gate_rc}" == "0" ]]; then
  ok "extractor stderr does not contaminate the matcher payload"
else
  bad "extractor stderr does not contaminate the matcher payload" "exit ${gate_rc}: ${gate_output}"
fi

# Non-vacuity for the case above: a genuinely unparseable payload on STDOUT must
# still refuse, or the fix would have been "stop parsing" rather than "stop
# merging the streams".
run_gate "0 0" 1 'not json at all' "${good_digest}" 'go: downloading github.com/example/mod v1.2.3'
if refused; then
  ok "an unparseable payload still refuses even when stderr is clean-looking"
else
  bad "an unparseable payload still refuses even when stderr is clean-looking" "exit 0: ${gate_output}"
fi

# --- The defect this gate catches ---------------------------------------------
run_gate "1" 1 "${good_json}"
if refused; then
  ok "a matcher that does NOT accept the staged digest stops promotion"
else
  bad "a matcher that does NOT accept the staged digest stops promotion" "exit 0: ${gate_output}"
fi

# --- Non-vacuity --------------------------------------------------------------
run_gate "0 0" 0 "${good_json}"
if refused; then
  ok "a negative control that ACCEPTS a wrong subject fails the gate"
else
  bad "a negative control that ACCEPTS a wrong subject fails the gate" "exit 0: ${gate_output}"
fi

# --- The liveness sandwich ----------------------------------------------------
# The control's non-zero exit is not yet evidence of a refusal: an outage exits
# non-zero too. Second positive call fails => the run cannot prove the control
# refused for the right reason, and an unprovable control is a failure.
run_gate "0 1" 1 "${good_json}"
if refused; then
  ok "an outage during the control is failed, not counted as a refusal"
else
  bad "an outage during the control is failed, not counted as a refusal" "exit 0: ${gate_output}"
fi

# --- Input validation ---------------------------------------------------------
run_gate "0 0" 1 "${good_json}" ""
if refused; then
  ok "an empty digest is refused"
else
  bad "an empty digest is refused" "exit 0: ${gate_output}"
fi

run_gate "0 0" 1 "${good_json}" "latest"
if refused; then
  ok "a non-digest reference is refused"
else
  bad "a non-digest reference is refused" "exit 0: ${gate_output}"
fi

# A malformed handoff must stop the build rather than yield an empty pattern:
# cosign treats an empty identity regexp as matching anything, so a gate that
# carried one through would verify with a wildcard and report success.
run_gate "0 0" 1 '{"subject":"x"}'
if refused; then
  ok "a matcher payload missing the issuer is refused, not wildcarded"
else
  bad "a matcher payload missing the issuer is refused, not wildcarded" "exit 0: ${gate_output}"
fi

run_gate "0 0" 1 'not json at all'
if refused; then
  ok "an unparseable matcher payload is refused"
else
  bad "an unparseable matcher payload is refused" "exit 0: ${gate_output}"
fi

echo "verify-matcher-accepts-digest: wiring"

# --- The gate is reached, and BEFORE the promotion ----------------------------
if grep -q "scripts/verify-matcher-accepts-digest.sh" "${action}"; then
  ok "the publication action calls the gate"
else
  bad "the publication action calls the gate" "no reference in ${action}"
fi

# The action invokes the gate DIRECTLY (`./scripts/...`) rather than through an
# interpreter, so the tracked mode decides whether the step can run at all. CI
# clones from git, so the mode that matters is the one in the INDEX — a local
# `chmod` that was never committed leaves the runner with a 100644 file and the
# step dies `permission denied` before the gate has verified anything.
#
# That failure is invisible to every other check in this suite: shellcheck,
# `bash <file>` and all the assertions above pass on a non-executable file,
# because none of them exec it. This assertion is the only one that would fire.
#
# The requirement is derived from the invocation rather than hard-coded, so
# switching the action to `bash scripts/...` correctly relaxes it instead of
# leaving a stale rule behind.
gate_rel="scripts/verify-matcher-accepts-digest.sh"
gate_mode="$(git -C "${root_dir}" ls-files -s -- "${gate_rel}" | awk '{print $1}')"
if grep -qE '(^|[^[:alnum:]_/])\./scripts/verify-matcher-accepts-digest\.sh' "${action}"; then
  if [[ "${gate_mode}" == "100755" ]]; then
    ok "the gate is tracked executable, as its direct invocation requires"
  else
    bad "the gate is tracked executable, as its direct invocation requires" \
      "the action runs ./${gate_rel} but git tracks it as ${gate_mode:-<untracked>}"
  fi
else
  ok "the gate is not invoked directly, so no executable bit is required"
fi

gate_line="$(grep -n "scripts/verify-matcher-accepts-digest.sh" "${action}" | head -1 | cut -d: -f1 || true)"
promote_line="$(grep -n "id: promote_latest" "${action}" | head -1 | cut -d: -f1 || true)"
sign_line="$(grep -n "id: cosign_sign" "${action}" | head -1 | cut -d: -f1 || true)"
provenance_line="$(grep -n "id: attest_provenance" "${action}" | head -1 | cut -d: -f1 || true)"

if [[ -n "${gate_line}" && -n "${promote_line}" ]] && ((gate_line < promote_line)); then
  ok "the gate runs BEFORE latest is promoted"
else
  bad "the gate runs BEFORE latest is promoted" "gate=${gate_line:-none} promote=${promote_line:-none}"
fi

if [[ -n "${gate_line}" && -n "${sign_line}" ]] && ((sign_line < gate_line)); then
  ok "the gate runs AFTER the staged digest is signed"
else
  bad "the gate runs AFTER the staged digest is signed" "sign=${sign_line:-none} gate=${gate_line:-none}"
fi

if [[ -n "${gate_line}" && -n "${provenance_line}" ]] && ((provenance_line < gate_line)); then
  ok "the gate runs AFTER both attestations are written"
else
  bad "the gate runs AFTER both attestations are written" "provenance=${provenance_line:-none} gate=${gate_line:-none}"
fi

# shellcheck disable=SC2016  # the literal ${STAGING_DIGEST} is the text being matched
if grep -q 'verify-matcher-accepts-digest.sh "\${STAGING_DIGEST}"' "${action}"; then
  ok "the gate is given the resolved staging digest"
else
  bad "the gate is given the resolved staging digest" "unexpected argument in ${action}"
fi

# The gate needs a Go toolchain to read the matcher out of the manifests, and
# the publication path had none. Without this step the gate fails closed on
# every deploy — safe, but it would block promotion permanently rather than
# verify anything.
setup_go_line="$(grep -n "actions/setup-go@" "${action}" | head -1 | cut -d: -f1 || true)"
if [[ -n "${setup_go_line}" && -n "${gate_line}" ]] && ((setup_go_line < gate_line)); then
  ok "a Go toolchain is set up before the gate needs it"
else
  bad "a Go toolchain is set up before the gate needs it" \
    "setup-go=${setup_go_line:-none} gate=${gate_line:-none}"
fi

# The gate must verify the subject the attestations were published under.
# Comparing the passed value against the published one asserts the property;
# asserting the line merely exists would pass for any value, including a wrong
# one.
published_subjects="$(sed -nE 's/^[[:space:]]*subject-name:[[:space:]]*//p' "${action}" | sort -u || true)"
published_count="$(printf '%s\n' "${published_subjects}" | grep -c . || true)"
gate_subject="$(yq '.runs.steps[] | select(.id == "verify_matcher_accepts_staged") | .env.SUBJECT_NAME' "${action}")"

if [[ -n "${gate_subject}" && "${gate_subject}" != "null" &&
  "${published_count}" == "1" && "${gate_subject}" == "${published_subjects}" ]]; then
  ok "the gate verifies the subject the attestations were published under (${gate_subject})"
else
  bad "the gate verifies the subject the attestations were published under" \
    "gate=${gate_subject:-<unset>} published(${published_count})=[${published_subjects}]"
fi

# This gate has NO enforcement toggle, deliberately. The evidence gate carries
# one to stage a new evidence KIND through the same path; there is no equivalent
# rollout here, and a bypass on the check that stops #3005 would reintroduce
# exactly the "present, green, and inert" class it exists to close.
gate_step_env="$(yq '.runs.steps[] | select(.id == "verify_matcher_accepts_staged") | .env | keys | .[]' "${action}" 2>/dev/null || true)"
if printf '%s\n' "${gate_step_env}" | grep -qx 'ENFORCE'; then
  bad "the gate has no enforcement bypass" "verify_matcher_accepts_staged reads ENFORCE"
else
  ok "the gate has no enforcement bypass"
fi

echo
echo "passed=${pass_count} failed=${fail_count}"
[[ "${fail_count}" == "0" ]]
