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
# ENFORCE gates only whether a missing-evidence verdict FAILS the deploy. The
# verification itself always runs, and always reports — a gate wrong about the
# identity or issuer would break every production deploy, so it earns the right
# to block by first being observed passing on real deploys.

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

failures=0
log_dir="$(mktemp -d)"
trap 'rm -rf "${log_dir}"' EXIT

# check runs one verification and records its verdict without aborting the rest.
# All three are reported every run: stopping at the first failure would hide a
# missing provenance behind a missing signature and turn one fix into three
# deploys.
check() {
  local label="$1"
  shift

  if "$@" >"${log_dir}/${label}.log" 2>&1; then
    echo "  ✅ ${label}"
    return 0
  fi

  echo "  ❌ ${label}"
  sed 's/^/       /' "${log_dir}/${label}.log" >&2
  failures=$((failures + 1))

  return 0
}

echo "Verifying published evidence for ${ref}"
echo "  identity: ${identity}"
echo "  issuer:   ${oidc_issuer}"

check "cosign signature" \
  cosign verify \
  --certificate-identity "${identity}" \
  --certificate-oidc-issuer "${oidc_issuer}" \
  "${ref}"

check "SBOM attestation" \
  gh attestation verify "oci://${ref}" \
  --repo "${repo}" \
  --predicate-type "${sbom_predicate}"

check "provenance attestation" \
  gh attestation verify "oci://${ref}" \
  --repo "${repo}" \
  --predicate-type "${provenance_predicate}"

# Compare as a string. A numeric test on a value that somehow became non-numeric
# evaluates as an error inside a conditional and reads as "no failures", which
# would let the gate pass exactly when its own bookkeeping broke.
if [[ "${failures}" == "0" ]]; then
  echo "All published evidence verified against ${digest}."
  exit 0
fi

if [[ "${ENFORCE:-false}" == "true" ]]; then
  echo "::error::${failures} evidence check(s) failed for ${digest}; refusing to promote it." >&2
  exit 1
fi

echo "::warning::${failures} evidence check(s) failed for ${digest}." \
  "This gate is not yet enforcing, so the deploy continues — see platform#2859." >&2
exit 0
