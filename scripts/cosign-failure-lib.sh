# shellcheck shell=bash
# Say WHY a `cosign verify` failed, so a gate names the failure it actually saw.
#
# WHY THIS EXISTS (#3545)
# The two matcher gates — ci.yaml's matcher-efficacy job and verify-matcher-accepts-digest.sh —
# read any non-zero `cosign verify` exit as the matcher refusing the signature. cosign exits
# non-zero for that, and just as readily for a registry it could not reach, a credential the
# registry refused, a rate limit, a server error, or a reference that is not there. So a ghcr.io
# timeout reported "the cosign matcher in the manifests does not verify": a supply-chain finding
# the run had no evidence for, which sends whoever reads it to the matcher regex and the signing
# identity when neither was involved — and teaches readers to re-run the gate without reading it,
# the reflex that would wave a REAL rejection through.
#
# 🔴 THIS DECIDES THE WORDING OF A FAILURE, NEVER WHETHER THERE IS ONE.
# Every class below is a failure, and every caller exits non-zero on all of them. Nothing here may
# be used to pass, skip or retry a verification into success: cosign's error text is not a stable
# interface, and a verdict that hung on it would open the moment the text moved.
#
# 🔴 "rejected" NEEDS POSITIVE EVIDENCE; A REPORTED OUTAGE OUTRANKS IT.
# It is reported only when cosign's output carries an identity-mismatch shape and NO transport or
# registry failure. When both appear — several signatures, one refused and one never read — the
# unread one might have matched, so the run proves nothing about the matcher and says so. A
# bundle verifier may suppress a failed lookup or use fallback trust material; absent output
# does not prove every dependency was reachable. The classifier diagnoses the reported failure,
# never service health or the completeness of cosign's internal attempts. Anything
# unrecognised (an empty log, a crash, a shape a newer cosign prints) is "unrecognised", which
# callers word as "no conclusion either way". A cosign upgrade that rewords its errors therefore
# degrades the diagnosis to an honest "unknown" — never to a false matcher finding, never to a pass.
#
# Classes, printed one per call by cosign_failure_class:
#   rejected        cosign read a signature and its certificate identity did not satisfy the
#                   issuer/subject regexps. The ONLY class that is a verdict about the matcher.
#   infrastructure  cosign could not finish reading: network, DNS, TLS, timeout, an auth refusal,
#                   a rate limit, a registry server error, or a reference the registry does not have.
#   unrecognised    anything else, including no output at all.
#
# Every local below is `cfl_`-prefixed. This file is SOURCED, and a caller's `readonly log` (the
# digest gate has one) makes a plain `local log` fail — which, under `set -e`, aborted the gate
# before it printed any diagnostic at all.

# The shapes cosign prints when it READ a signature and the certificate's identity did not match —
# one per signature format cosign v3 verifies. Taken from the source of the versions this repository
# runs (cosign v3.0.6 from cosign-installer in ci.yaml, v3.1.3 from setup-supply-chain-tools.sh on
# the deploy path; sigstore-go v1.1.4 and v1.2.2 beneath them), not from memory:
#   legacy signatures   cosign pkg/cosign/verify.go
#                         "none of the expected identities matched what was in the certificate"
#   Sigstore bundles    sigstore-go pkg/verify/signed_entity.go + certificate_identity.go
#                         "failed to verify certificate identity: no matching CertificateIdentity found"
# Both cover an issuer mismatch as well as a subject mismatch: each is the error for "no configured
# identity accepted this certificate".
cosign_rejection_pattern() {
  printf '%s' 'none of the expected identities matched what was in the certificate|failed to verify certificate identity: no matching CertificateIdentity found'
}

# Failures that mean cosign never got an answer about the signature.
#   transport  the Go net/http and TLS errors a dial, lookup or read surfaces.
#   registry   go-containerregistry's transport.Error: "<CODE>: <message>" when the registry sent a
#              distribution error body, "unexpected status code <n> <text>" when it sent a bare
#              status; plus cosign's own wrapper for a missing reference.
# The registry codes are matched WITH their ": " separator, so a word like DENIED appearing inside a
# certificate subject cannot turn a genuine rejection into an outage.
cosign_infrastructure_pattern() {
  printf '%s' 'i/o timeout|TLS handshake timeout|context deadline exceeded|Client\.Timeout exceeded|Client\.Timeout or context cancellation while reading body|connection refused|connection reset by peer|no such host|server misbehaving|network is unreachable|no route to host|unexpected EOF|(^|[[:space:]:])EOF$|http2: client connection lost|(UNAUTHORIZED|DENIED|TOOMANYREQUESTS|UNAVAILABLE|MANIFEST_UNKNOWN|NAME_UNKNOWN|BLOB_UNKNOWN): |unexpected status code [0-9]{3}|http status code: [0-9]{3}|image tag not found'
}

# Print the class of the failure recorded in <log>: rejected, infrastructure or unrecognised.
# A missing, unreadable or empty log is unrecognised — never rejected.
cosign_failure_class() { # log
  local cfl_log="${1-}"
  if [[ -z "${cfl_log}" || ! -s "${cfl_log}" ]]; then
    echo "unrecognised"
  elif grep -Eq -- "$(cosign_infrastructure_pattern)" "${cfl_log}"; then
    echo "infrastructure"
  elif grep -Eq -- "$(cosign_rejection_pattern)" "${cfl_log}"; then
    echo "rejected"
  else
    echo "unrecognised"
  fi
}

# Print the first line of <log> that decided <class>, or nothing. This is what lets the annotation
# NAME the underlying error rather than leave it one screen up in the raw output.
cosign_failure_evidence() { # log class
  local cfl_log="${1-}" cfl_class="${2-}" cfl_pattern
  case "${cfl_class}" in
    infrastructure) cfl_pattern="$(cosign_infrastructure_pattern)" ;;
    rejected) cfl_pattern="$(cosign_rejection_pattern)" ;;
    *) return 0 ;;
  esac
  [[ -n "${cfl_log}" && -r "${cfl_log}" ]] || return 0
  grep -E -m 1 -- "${cfl_pattern}" "${cfl_log}" || true
}

# Print the ::error:: block for a failure that is NOT a matcher verdict, on stdout. Callers print
# their own unchanged wording for "rejected" and route this one wherever their errors go.
cosign_report_no_verdict() { # class artifact log
  local cfl_class="${1-}" cfl_artifact="${2-}" cfl_log="${3-}" cfl_evidence
  cfl_evidence="$(cosign_failure_evidence "${cfl_log}" "${cfl_class}")"
  case "${cfl_class}" in
    infrastructure)
      echo "::error::cosign could not complete the check of ${cfl_artifact}: the registry or a Sigstore service was unreachable, refused the request, failed, or did not have the reference."
      if [[ -n "${cfl_evidence}" ]]; then
        echo "::error::cosign reported: ${cfl_evidence}"
      fi
      echo "::error::this is an infrastructure failure, NOT a verdict on the cosign matcher — this run learned nothing about whether the matcher verifies the artifact."
      ;;
    *)
      echo "::error::cosign failed on ${cfl_artifact} without reporting an identity mismatch or a recognised registry/network failure."
      echo "::error::this run cannot say whether the matcher refused the signature or the check never completed; read cosign's output below."
      ;;
  esac
  echo "::error::failing closed regardless: an artifact this run could not verify is never treated as verified."
}
