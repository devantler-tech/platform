#!/usr/bin/env bash
# Hermetic coverage for scripts/cosign-failure-lib.sh and for the ci.yaml step that uses it.
#
# The subject is the wording of a FAILED `cosign verify`. A ghcr.io timeout used to be reported as
# "the cosign matcher in the manifests does not verify" (#3545): a supply-chain finding the run had
# no evidence for. The classifier exists so that finding is reserved for output showing cosign read
# a signature and refused its identity, while an outage, a refused credential or a server error is
# named as what it is — and so that every one of them still FAILS.
#
# Two halves:
#   classifier  every fixture under fixtures/cosign-verify-failures/ is classified, and its filename
#               prefix is the expected class. Fixtures include existing captured cosign error
#               shapes and representative transport and negative boundary cases.
#               They validate diagnostics, not cosign's internal service/fallback behavior.
#   ci step     the matcher-efficacy step is lifted out of ci.yaml and executed with `go` and
#               `cosign` stubbed on PATH, because that step — not the library — is where #3545
#               fired. Asserting the library alone would pass while the step still printed the old
#               verdict for every failure.
#
# No registry, network, token or Go toolchain is involved.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly fixtures="${root_dir}/scripts/tests/fixtures/cosign-verify-failures"
readonly workflow="${root_dir}/.github/workflows/ci.yaml"
readonly step_name="🔏 Check the configured matcher verifies the real artifact"
readonly matcher_finding="does not verify"

# shellcheck source=scripts/cosign-failure-lib.sh
source "${root_dir}/scripts/cosign-failure-lib.sh"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

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

echo "cosign-failure-lib: classifier"

# --- Every fixture lands in the class its name declares -------------------------
# A glob that matched nothing would make this loop assert nothing and report success, so the
# fixture population is asserted first, per class: an empty class is a gap, not a pass.
seen_rejected=0
seen_infrastructure=0
seen_unrecognised=0
for fixture in "${fixtures}"/*.log; do
  [[ -e "${fixture}" ]] || continue
  name="${fixture##*/}"
  name="${name%.log}"
  want="${name%%-*}"
  case "${want}" in
    rejected) seen_rejected=$((seen_rejected + 1)) ;;
    infrastructure) seen_infrastructure=$((seen_infrastructure + 1)) ;;
    unrecognised) seen_unrecognised=$((seen_unrecognised + 1)) ;;
    *)
      bad "fixture ${name} names a known class" "prefix '${want}' is not rejected/infrastructure/unrecognised"
      continue
      ;;
  esac
  got="$(cosign_failure_class "${fixture}")"
  if [[ "${got}" == "${want}" ]]; then
    ok "${name} → ${got}"
  else
    bad "${name} → ${want}" "classified as '${got}'"
  fi
done

if ((seen_rejected >= 2 && seen_infrastructure >= 5 && seen_unrecognised >= 2)); then
  ok "every class has fixtures (rejected=${seen_rejected} infrastructure=${seen_infrastructure} unrecognised=${seen_unrecognised})"
else
  bad "every class has fixtures" \
    "rejected=${seen_rejected} infrastructure=${seen_infrastructure} unrecognised=${seen_unrecognised}"
fi

# The issue's own evidence: the exact failure that was reported as a matcher finding.
if [[ -f "${fixtures}/infrastructure-dial-timeout.log" ]] &&
  grep -q 'i/o timeout' "${fixtures}/infrastructure-dial-timeout.log"; then
  ok "the #3545 dial timeout is among the fixtures"
else
  bad "the #3545 dial timeout is among the fixtures" "fixture missing or no longer carries the timeout"
fi

# --- No evidence is never a rejection --------------------------------------------
for missing in "" "${work}/does-not-exist.log"; do
  got="$(cosign_failure_class "${missing}")"
  if [[ "${got}" == "unrecognised" ]]; then
    ok "a missing log ('${missing:-<empty path>}') is unrecognised"
  else
    bad "a missing log ('${missing:-<empty path>}') is unrecognised" "classified as '${got}'"
  fi
done

# --- The evidence line is the one that decided the class -------------------------
evidence="$(cosign_failure_evidence "${fixtures}/infrastructure-dial-timeout.log" infrastructure)"
if [[ "${evidence}" == 'Error: Get "https://ghcr.io/v2/": dial tcp 192.0.2.10:443: i/o timeout' ]]; then
  ok "the evidence for an outage is cosign's own first error line"
else
  bad "the evidence for an outage is cosign's own first error line" "got: '${evidence}'"
fi

evidence="$(cosign_failure_evidence "${fixtures}/unrecognised-unknown-shape.log" unrecognised)"
if [[ -z "${evidence}" ]]; then
  ok "an unrecognised failure claims no deciding line"
else
  bad "an unrecognised failure claims no deciding line" "got: '${evidence}'"
fi

# --- The no-verdict report never borrows the matcher finding ---------------------
for class in infrastructure unrecognised; do
  for fixture in "${fixtures}/${class}"-*.log; do break; done
  report="$(cosign_report_no_verdict "${class}" "ghcr.io/example/artifact:tag" "${fixture}")"
  if printf '%s\n' "${report}" | grep -q "${matcher_finding}"; then
    bad "the ${class} report does not claim the matcher refused" "report: ${report}"
  else
    ok "the ${class} report does not claim the matcher refused"
  fi
  if printf '%s\n' "${report}" | grep -qv '^::error::'; then
    bad "every ${class} report line is an error annotation" "report: ${report}"
  else
    ok "every ${class} report line is an error annotation"
  fi
done

report="$(cosign_report_no_verdict infrastructure "ghcr.io/example/artifact:tag" "${fixtures}/infrastructure-dial-timeout.log")"
if printf '%s\n' "${report}" | grep -q '^::error::cosign reported: Error: Get "https://ghcr.io/v2/": dial tcp 192.0.2.10:443: i/o timeout$' &&
  printf '%s\n' "${report}" | grep -q 'NOT a verdict on the cosign matcher'; then
  ok "an outage is named as infrastructure, quoting cosign's error"
else
  bad "an outage is named as infrastructure, quoting cosign's error" "report: ${report}"
fi

# The library is SOURCED into callers that hold their own readonly globals. A plain `local log`
# fails against a caller's `readonly log`, and under `set -e` the caller then dies before printing
# any diagnostic — caught while wiring the digest gate, which has exactly that global.
collision_output="$(
  set -euo pipefail
  log="${fixtures}/infrastructure-dial-timeout.log" artifact=x class=x evidence=x pattern=x
  # shellcheck disable=SC2034  # unused on purpose: they exist only to be readonly
  readonly log artifact class evidence pattern
  cosign_report_no_verdict "$(cosign_failure_class "${log}")" "ghcr.io/example/artifact:tag" "${log}"
  echo "reached the end"
)" || true
if printf '%s\n' "${collision_output}" | grep -q '::error::cosign reported: ' &&
  printf '%s\n' "${collision_output}" | grep -q 'reached the end'; then
  ok "the library works beside a caller's readonly globals"
else
  bad "the library works beside a caller's readonly globals" "output: ${collision_output}"
fi

echo "cosign-failure-lib: the ci.yaml matcher-efficacy step"

# --- Lift the real step out of the workflow --------------------------------------
step_script="${work}/step.sh"
yq ".jobs[\"validate-matcher-efficacy\"].steps[] | select(.name == \"${step_name}\") | .run" \
  "${workflow}" >"${step_script}"
# Non-vacuity: a renamed step makes yq print nothing, and an empty script exits 0 for every case.
if grep -q 'cosign verify' "${step_script}" && grep -q 'cosign_failure_class' "${step_script}"; then
  ok "the step is found, and it verifies with cosign and classifies the failure"
else
  bad "the step is found, and it verifies with cosign and classifies the failure" \
    "extracted: $(head -c 200 "${step_script}")"
fi

good_json="$(jq -nc --arg i '^https://token\.actions\.githubusercontent\.com$' \
  --arg s '^https://github\.com/devantler-tech/platform/\.github/workflows/.*$' '{issuer:$i,subject:$s}')"
readonly good_json

# run_step executes the lifted step the way the runner does (bash -e -o pipefail) from the
# repository root, with cosign answering the POSITIVE checks with <positive_code> and <fixture> on
# stderr. The negative control's wrong subject is always refused, as a real cosign would.
run_step() { # positive_codes fixture [step]
  local positive_codes="$1" fixture="$2" selected_step="${3-${step_script}}" dir
  dir="$(mktemp -d "${work}/stub.XXXXXX")"
  printf '%s\n' "${positive_codes}" >"${dir}/positive_codes"
  printf '0\n' >"${dir}/positive_calls"

  cat >"${dir}/go" <<EOF
#!/usr/bin/env bash
case "\$1" in
  test) exit 0 ;;
  run) printf '%s' '${good_json}' ;;
esac
EOF

  cat >"${dir}/cosign" <<EOF
#!/usr/bin/env bash
case "\$1" in
  login) cat >/dev/null; exit 0 ;;
esac
if printf '%s' "\$*" | grep -q 'NOT-A-REAL-REPO'; then
  cat '${fixtures}/rejected-bundle-identity.log' >&2
  exit 1
fi
n=\$(cat '${dir}/positive_calls')
n=\$((n + 1))
echo "\${n}" >'${dir}/positive_calls'
read -r -a codes <'${dir}/positive_codes'
idx=\$((n - 1))
if [ "\${idx}" -ge "\${#codes[@]}" ]; then
  idx=\$((\${#codes[@]} - 1))
fi
if [ "\${codes[\${idx}]}" != 0 ] || [ "\${n}" = 1 ]; then
  cat '${fixture}' >&2
fi
exit "\${codes[\${idx}]}"
EOF
  chmod +x "${dir}/go" "${dir}/cosign"

  set +e
  step_output="$(cd "${root_dir}" && PATH="${dir}:${PATH}" \
    ARTIFACT=ghcr.io/devantler-tech/platform/manifests:latest \
    REGISTRY_USER=stub REGISTRY_TOKEN=stub \
    bash --noprofile --norc -e -o pipefail "${selected_step}" 2>&1)"
  step_rc=$?
  set -e
  step_positive_calls="$(cat "${dir}/positive_calls")"
}

run_step 0 /dev/null
if [[ "${step_rc}" == "0" ]] && printf '%s' "${step_output}" | grep -q 'the check above is not vacuous'; then
  ok "a matcher that verifies passes the step"
else
  bad "a matcher that verifies passes the step" "exit ${step_rc}: ${step_output}"
fi

# NEGATIVE CONTROL for the wording: a genuine identity mismatch keeps the original finding, in both
# signature formats, and still fails.
for fixture in rejected-legacy-identity rejected-bundle-identity; do
  run_step '12 0' "${fixtures}/${fixture}.log"
  if [[ "${step_rc}" != "0" ]] &&
    [[ "${step_positive_calls}" = 1 ]] &&
    printf '%s' "${step_output}" | grep -q "::error::the cosign matcher in the manifests ${matcher_finding} " &&
    printf '%s' "${step_output}" | grep -q '::error::a matcher that matches nothing is inert'; then
    ok "a genuine rejection (${fixture}) keeps the matcher finding and fails"
  else
    bad "a genuine rejection (${fixture}) keeps the matcher finding and fails" "exit ${step_rc}: ${step_output}"
  fi
done

# THE DEFECT: an outage, a refused credential and a server error each fail WITHOUT the finding.
for fixture in infrastructure-dial-timeout infrastructure-dns infrastructure-unauthorized \
  infrastructure-denied infrastructure-server-error infrastructure-tag-not-found; do
  run_step '1 0' "${fixtures}/${fixture}.log"
  if [[ "${step_rc}" == "0" ]]; then
    bad "${fixture} fails the step" "exit 0: ${step_output}"
  elif printf '%s' "${step_output}" | grep -q "${matcher_finding}"; then
    bad "${fixture} is not reported as a matcher finding" "output: ${step_output}"
  elif ! printf '%s' "${step_output}" | grep -q '::error::cosign reported: '; then
    bad "${fixture} names cosign's underlying error" "output: ${step_output}"
  elif [[ "${step_positive_calls}" != 1 ]]; then
    bad "${fixture} stops at the first failed verification" "positive calls: ${step_positive_calls}"
  else
    ok "${fixture} fails as infrastructure, naming cosign's error, without the matcher finding"
  fi
done

run_step '1 0' "${fixtures}/unrecognised-unknown-shape.log"
if [[ "${step_rc}" != "0" ]] &&
  ! printf '%s' "${step_output}" | grep -q "${matcher_finding}" &&
  printf '%s' "${step_output}" | grep -q 'cannot say whether the matcher refused'; then
  ok "an unrecognised failure fails without claiming a verdict either way"
else
  bad "an unrecognised failure fails without claiming a verdict either way" "exit ${step_rc}: ${step_output}"
fi

# The first verification must stop even if the later liveness check would succeed.
# Removing that one exit used to survive these tests because every call failed.
ablated_step="${work}/step-without-first-exit.sh"
awk '/FAILURE_CLASS=/{first_failure=1} first_failure && /^[[:space:]]*exit 1$/{first_failure=0; next} {print}' \
  "${step_script}" >"${ablated_step}"
run_step '1 0' "${fixtures}/infrastructure-dial-timeout.log" "${ablated_step}"
if [[ "${step_rc}" = 0 && "${step_positive_calls}" = 2 ]]; then
  ok "the first-failure fixture detects removal of its stopping exit"
else
  bad "the first-failure fixture detects removal of its stopping exit" "exit ${step_rc}, positive calls ${step_positive_calls}: ${step_output}"
fi

# Successful verification can still warn about a fallback. Preserve the first
# call's diagnostic even though the later successful check is quiet.
success_warning="${work}/success-warning.log"
printf '%s\n' 'verification succeeded with a trusted-root fallback warning' >"${success_warning}"
run_step '0 0' "${success_warning}"
if [[ "${step_rc}" = 0 ]] && printf '%s' "${step_output}" | grep -qF 'trusted-root fallback warning'; then
  ok "the first successful verification retains its stderr warning"
else
  bad "the first successful verification retains its stderr warning" "exit ${step_rc}: ${step_output}"
fi

# cosign's raw output still reaches the log on every failure path.
run_step 1 "${fixtures}/infrastructure-rate-limited.log"
if [[ "${step_rc}" != "0" ]] && printf '%s' "${step_output}" | grep -q 'TOOMANYREQUESTS: retry-after'; then
  ok "cosign's full output is printed on failure"
else
  bad "cosign's full output is printed on failure" "exit ${step_rc}: ${step_output}"
fi

echo
echo "passed=${pass_count} failed=${fail_count}"
[[ "${fail_count}" == "0" ]]
