#!/usr/bin/env bash
# Verify the cosign matcher this repository DEPLOYS accepts the signature on the
# exact digest about to become `latest`.
#
# ci.yaml's matcher-efficacy job asks the same question of the MUTABLE
# `:latest` tag, at pull-request and merge-queue time. That catches a matcher
# that matches nothing at all — wrong org, wrong ref, malformed pattern — which
# was the immediate ask of #3288 and is worth keeping: it fails early and
# cheaply, before a deploy is even attempted.
#
# It cannot catch a change that alters the publishing IDENTITY itself, because
# at that moment `latest` still names the PREVIOUSLY published artifact, signed
# by the old identity. The old signature verifies, the job goes green, and the
# deploy then signs and promotes a new digest under the new identity that the
# deployed matcher does not accept. Flux rejects the root source and GitOps
# delivery stops — the self-locking outage of #3005, which #3006 records could
# not self-recover.
#
# So this gate asks the same question one step later and about the right bytes:
# after the staged digest is signed and attested, before it is promoted. That is
# the point of no return — once `latest` names these bytes, production follows
# them within the minute.
#
# Usage: verify-matcher-accepts-digest.sh <sha256:...>
set -euo pipefail

readonly digest="${1-}"

# Defaults are carried here rather than required from the caller so the DR
# publication path gets the same gate without restating them, but each stays
# overridable for tests.
readonly subject_name="${SUBJECT_NAME:-ghcr.io/devantler-tech/platform/manifests}"
readonly config_manifest="${CONFIG_MANIFEST:-ksail.prod.yaml}"
readonly flux_instance_manifest="${FLUX_INSTANCE_MANIFEST:-k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml}"

# The negative control's deliberately wrong subject. It must be a well-formed
# pattern that cannot match a real signer, so a refusal is attributable to the
# identity rather than to a malformed regex cosign would reject anyway.
readonly wrong_subject='^https://github\.com/devantler-tech/NOT-A-REAL-REPO/.*$'

if [[ ! "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "::error::expected an immutable digest, got: '${digest:-<empty>}'" >&2
  echo "::error::this gate must verify the bytes about to be promoted, never a mutable tag." >&2
  exit 1
fi

readonly artifact="${subject_name}@${digest}"

# 🔴 THE PATTERNS ARE NEVER SPELLED HERE. They are printed out of the manifests
# we deploy and handed straight to cosign, so this tests the deployed rule
# rather than a copy of it that can drift — and it reuses ci.yaml's extractor
# rather than re-reading the YAML, so the two gates cannot disagree about what
# the matcher IS.
if ! matcher_json="$(go run ./scripts/validate-matcher-efficacy --print-matcher \
  "${config_manifest}" "${flux_instance_manifest}" 2>&1)"; then
  echo "::error::could not read the matcher out of the manifests" >&2
  echo "${matcher_json}" >&2
  exit 1
fi

# 🔴 DECODE AS JSON, never line-per-value. A matcher value may legitimately
# contain a newline — a YAML literal block is valid for these fields — and a
# line-based read would take only its first line, verifying against a truncated
# regex while Flux enforces the whole thing.
#
# `jq -e` fails on a null or missing field, which matters more than it looks:
# cosign treats an EMPTY identity regexp as matching anything, so a malformed
# handoff carried through would verify with a wildcard and report success. That
# is the inert-but-green class this gate exists to close, reproduced inside the
# gate itself.
if ! issuer="$(jq -er '.issuer' <<<"${matcher_json}")" ||
  ! subject="$(jq -er '.subject' <<<"${matcher_json}")" ||
  [[ -z "${issuer}" || -z "${subject}" ]]; then
  echo "::error::the matcher payload is missing an issuer or subject, or is not JSON" >&2
  exit 1
fi

echo "matcher issuer:  ${issuer}"
echo "matcher subject: ${subject}"
echo "staged artifact: ${artifact}"

# verify_as captures cosign's own output rather than discarding it. When this
# gate fires for real it is standing between a bad matcher and production, and
# the operator reading the log needs cosign's reason — not just the fact that
# it said no. The log is only printed on the failure paths, so a passing deploy
# stays quiet.
log="$(mktemp)"
readonly log
trap 'rm -f "${log}"' EXIT

verify_as() {
  cosign verify \
    --certificate-oidc-issuer-regexp "${issuer}" \
    --certificate-identity-regexp "$1" \
    "${artifact}" >"${log}" 2>&1
}

# THE CHECK. Fails when the matcher production will actually enforce would not
# accept the signature on the digest we are about to point production at.
if ! verify_as "${subject}"; then
  echo "::error::the cosign matcher in the manifests does not verify ${artifact}." >&2
  echo "::error::promoting this digest would point production at bytes its own matcher rejects," >&2
  echo "::error::stopping GitOps delivery until a human intervenes (#3005, #3006)." >&2
  cat "${log}" >&2
  exit 1
fi
echo "the configured matcher verifies the staged digest"

# THE NEGATIVE CONTROL, run on every deploy rather than asserted once. A
# deliberately wrong subject MUST be refused; if it is accepted, the check above
# proves nothing and is itself the vacuous control this gate exists to prevent.
if verify_as "${wrong_subject}"; then
  echo "::error::negative control PASSED — a deliberately wrong subject was accepted." >&2
  echo "::error::this gate cannot distinguish a good matcher from a broken one. Fix it before trusting it." >&2
  exit 1
fi

# 🔴 THAT NON-ZERO EXIT IS NOT YET EVIDENCE OF A REFUSAL. A network, registry or
# auth failure also exits non-zero, and would report the control as having
# passed while proving nothing. Re-run the POSITIVE check: if the same path
# still verifies, the refusal above was a real refusal rather than an outage.
# This is a liveness sandwich, deliberately not a match on cosign's error text,
# which is not a stable interface.
if ! verify_as "${subject}"; then
  echo "::error::the negative control exited non-zero, but the staged digest can no longer be" >&2
  echo "::error::verified either — so this run cannot prove the control refused for the right" >&2
  echo "::error::reason. Treating an unprovable control as a failure, not a pass." >&2
  cat "${log}" >&2
  exit 1
fi

echo "negative control refused a wrong subject, and the positive path still verifies"
echo "=> the matcher genuinely accepts the digest about to be promoted"
