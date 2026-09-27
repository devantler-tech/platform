#!/usr/bin/env bash
# Verify that the supply-chain evidence for a staged digest actually reached the
# registry, before that digest is promoted to the tag production follows.
#
# The publication transaction signs the resolved digest, attests an SBOM and
# build provenance, and then promotes those exact bytes to latest. Every one of
# those steps can report success while leaving no usable evidence behind: a
# swallowed failure, a shadowed cosign, a resolver returning a constant digest,
# or an upload that never happened all end with a green job and an artifact
# production is about to consume with nothing signed for it.
#
# A static check over the workflow text cannot close that. It can decide what
# order the steps appear in and what each is wired to, but not what the named
# program actually did — which is exactly where the remaining bypasses live.
# This asks the registry instead, which is decidable: it either carries valid
# evidence for those exact bytes or it does not.
#
# Producer-side and complementary to the cluster's spec.verify: this fails fast
# in CI with a clear message, that one refuses the pull. Neither replaces the
# other.
#
# ENFORCE gates only whether a missing-evidence verdict FAILS the deploy; the
# verification itself always runs and always reports. It defaults to enforcing
# and is validated against a closed domain, so the gate cannot be turned off by
# an empty value, a typo, or dropped wiring — a gate that is silently off is
# indistinguishable from a passing one, and that is the single failure this
# script exists to prevent.
#
# ENFORCE=false remains available to stage a NEW evidence kind through the same
# warn-then-enforce ratchet this check itself went through: a verification wrong
# about the identity or issuer would break every production deploy, so a new
# check earns the right to block by first being observed passing on real ones.

set -uo pipefail

readonly subject_name="${SUBJECT_NAME:-ghcr.io/devantler-tech/platform/manifests}"
readonly oidc_issuer="${OIDC_ISSUER:-https://token.actions.githubusercontent.com}"

# Predicate types the two attestation steps write. Checked by TYPE rather than
# by counting attestations: any signed attestation would satisfy a bare presence
# check, including one carrying neither of the predicates production relies on.
readonly sbom_predicate="${SBOM_PREDICATE:-https://cyclonedx.org/bom}"
readonly provenance_predicate="${PROVENANCE_PREDICATE:-https://slsa.dev/provenance/v1}"

usage() {
  echo "usage: ${0##*/} <sha256:digest>" >&2
  echo "env: ENFORCE=true|false  WORKFLOW_REF=<owner/repo/.github/workflows/x.yaml@ref>" >&2
}

digest="${1-}"

# Validate the digest rather than interpolating whatever arrived. An empty or
# malformed value would otherwise be pasted into a reference and verified
# against SOMETHING — most likely the mutable tag — which is precisely the
# "verified the wrong bytes" outcome this gate exists to rule out.
if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "::error::Refusing to verify an invalid digest: ${digest:-<empty>}" >&2
  usage
  exit 2
fi

# Validate the flag's DOMAIN, not just read it. The enforcing branch below tests
# for the literal "true", so without this every other value — ENFORCE=True,
# ENFORCE=1, a typo — would quietly select the non-enforcing path and be
# indistinguishable from a deliberate ENFORCE=false. The operator who set it
# would see a ::warning:: and a green job and conclude the gate was refusing
# promotion, when it was off. Rejected here, with the other input validation, so
# a misconfigured run fails before it spends registry calls to reach the same
# verdict.
#
# The default is ENFORCING, so the two ways this flag goes missing without being
# a typo — an empty value from `ENFORCE: ${{ inputs.x }}` when the input is
# renamed, and an absent variable when the env block is dropped — fail closed
# rather than silently reverting the gate to warn-only on a green job. Disabling
# it is therefore always an explicit, reviewable "false".
enforce="${ENFORCE:-true}"
if [[ "${enforce}" != "true" && "${enforce}" != "false" ]]; then
  echo "::error::ENFORCE must be exactly 'true' or 'false' (got: '${enforce}')" >&2
  usage
  exit 2
fi
readonly enforce

# The identity is this run's own workflow, taken from the runner rather than
# from a pattern. GITHUB_WORKFLOW_REF is "owner/repo/.github/workflows/f.yaml@ref",
# and the Fulcio SAN is that with the GitHub host prefixed — so the signature is
# checked against the exact workflow that claims to have produced it, not
# against any workflow in the repository.
workflow_ref="${WORKFLOW_REF:-${GITHUB_WORKFLOW_REF:-}}"
if [[ -z "${workflow_ref}" ]]; then
  echo "::error::WORKFLOW_REF is empty; cannot pin the signing identity" >&2
  exit 2
fi

readonly identity="https://github.com/${workflow_ref}"
readonly ref="${subject_name}@${digest}"
readonly repo="${GITHUB_REPOSITORY:-devantler-tech/platform}"

# A registry read can fail for a moment and succeed seconds later: platform#3089
# saw the SBOM check denied while the provenance check, with the same command and
# token, passed five seconds after it. Failing the gate on that evicts the PR from
# the merge queue and burns a production deploy cycle. So a read that failed in a
# transient way is retried a bounded number of times.
#
# Only the READ is retried, never the verdict. A tool that reached the registry
# and found the evidence absent or invalid fails on the first attempt, so a
# retry can never turn missing evidence into a pass. A read that is still failing
# when the attempts run out fails the gate too, and says it was the read.
read_attempts="${EVIDENCE_READ_ATTEMPTS:-3}"
read_backoff="${EVIDENCE_READ_BACKOFF_SECONDS:-5}"
if [[ ! "${read_attempts}" =~ ^[1-9][0-9]?$ ]]; then
  echo "::error::EVIDENCE_READ_ATTEMPTS must be an integer from 1 to 99 (got: '${read_attempts}')" >&2
  exit 2
fi
if [[ ! "${read_backoff}" =~ ^[0-9]{1,3}$ ]]; then
  echo "::error::EVIDENCE_READ_BACKOFF_SECONDS must be an integer from 0 to 999 (got: '${read_backoff}')" >&2
  exit 2
fi
readonly read_attempts read_backoff

# Failures that mean the read did not complete, as the registry and the tools
# report them. Anything else — including "no attestations found" and a
# verification mismatch — is a verdict about the evidence and is not retried.
readonly transient_read_pattern='denied access to the requested resource|DENIED: denied|UNAUTHORIZED|TOOMANYREQUESTS|Too Many Requests|500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Time-?out|i/o timeout|TLS handshake timeout|connection reset by peer|connection refused|context deadline exceeded|unexpected EOF|no such host'

failures=0
read_failures=0
log_dir="$(mktemp -d)"
trap 'rm -rf "${log_dir}"' EXIT

# check runs one verification and records its verdict without aborting the rest.
# All three are reported every run: stopping at the first failure would hide a
# missing provenance behind a missing signature and turn one fix into three
# deploys.
check() {
  local label="$1"
  shift
  local log="${log_dir}/${label}.log"
  local attempt=1

  while true; do
    if "$@" >"${log}" 2>&1; then
      if ((attempt > 1)); then
        echo "  ✅ ${label} (read succeeded on attempt ${attempt} of ${read_attempts})"
      else
        echo "  ✅ ${label}"
      fi
      return 0
    fi

    if ! grep -Eq -- "${transient_read_pattern}" "${log}"; then
      echo "  ❌ ${label} — evidence absent or invalid"
      sed 's/^/       /' "${log}" >&2
      failures=$((failures + 1))
      return 0
    fi

    if ((attempt >= read_attempts)); then
      echo "  ❌ ${label} — registry read failed on all ${read_attempts} attempt(s); this says nothing about whether the evidence exists"
      sed 's/^/       /' "${log}" >&2
      failures=$((failures + 1))
      read_failures=$((read_failures + 1))
      return 0
    fi

    echo "  ⚠️ ${label} — registry read failed on attempt ${attempt} of ${read_attempts}; retrying in ${read_backoff}s"
    sed 's/^/       /' "${log}" >&2
    attempt=$((attempt + 1))
    sleep "${read_backoff}"
  done
}

echo "Verifying published evidence for ${ref}"
echo "  identity: ${identity}"
echo "  issuer:   ${oidc_issuer}"

check "cosign signature" \
  cosign verify \
  --certificate-identity "${identity}" \
  --certificate-oidc-issuer "${oidc_issuer}" \
  "${ref}"

# --bundle-from-oci on both: the attestations are published with
# `create-storage-record: false`, so the bundles live in the registry beside the
# image and were never written to the GitHub Attestations API that this command
# reads by default. Without the flag both checks query the API, find nothing,
# and fail for ABSENT evidence — indistinguishable in the verdict from evidence
# that is genuinely missing or invalid, which is the one distinction this gate
# exists to make.
#
# --cert-identity on both: --repo scopes the lookup to this repository but says
# nothing about WHICH workflow signed. Without it, any workflow here that can
# mint an attestation satisfies the gate, so a less-trusted one becomes a path
# to a promotable digest. That is the same substitution the cosign check above
# already refuses, and the two must not disagree about who is allowed to vouch
# for a digest.
check "SBOM attestation" \
  gh attestation verify "oci://${ref}" \
  --bundle-from-oci \
  --repo "${repo}" \
  --cert-identity "${identity}" \
  --predicate-type "${sbom_predicate}"

check "provenance attestation" \
  gh attestation verify "oci://${ref}" \
  --bundle-from-oci \
  --repo "${repo}" \
  --cert-identity "${identity}" \
  --predicate-type "${provenance_predicate}"

# Compare as a string. A numeric test on a value that somehow became non-numeric
# evaluates as an error inside a conditional and reads as "no failures", which
# would let the gate pass exactly when its own bookkeeping broke.
if [[ "${failures}" == "0" ]]; then
  echo "All published evidence verified against ${digest}."
  exit 0
fi

# Name which kind of failure happened. A read that never completed is not
# evidence that the SBOM is missing, and an operator told "the token was denied"
# goes looking for an expired credential when the likelier cause is a registry
# blip; a re-queue is the remedy for that, not a credential rotation.
if ((read_failures > 0)); then
  echo "::error::${read_failures} of ${failures} failed check(s) could not read the registry after ${read_attempts} attempt(s)." \
    "That is a failed read, not missing evidence; if the token is valid, re-queue." >&2
fi

if [[ "${enforce}" == "true" ]]; then
  echo "::error::${failures} evidence check(s) failed for ${digest}; refusing to promote it." >&2
  exit 1
fi

# Reached only when a caller explicitly set ENFORCE=false to stage a new evidence
# kind. Name that as the reason the deploy continued, so the line cannot be read
# as the gate merely not being armed yet.
echo "::warning::${failures} evidence check(s) failed for ${digest}." \
  "ENFORCE=false was set explicitly, so the deploy continues; promotion is" \
  "refused whenever this gate runs with its default." >&2
exit 0
