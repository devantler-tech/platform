#!/usr/bin/env bash
#
# Reports whether production runs what main says, after a push to main (#3848).
#
# It resolves the digest `:latest` names, asks scripts/resolve-prod-convergence for
# a verdict, annotates the run, and writes `verdict=<CONVERGED|BEHIND|DIVERGED>` to
# $GITHUB_OUTPUT. It never deploys itself; the workflow redeploys main on BEHIND
# (#3869). No verdict is written when the check fails, so a broken check can never
# trigger a deploy.
#
#   exit 0  CONVERGED, or BEHIND/DIVERGED reported as a warning
#   exit 1  the verdict could not be established (UNKNOWN, an unreadable digest,
#           or resolver output that contradicts its own exit status)
#
# BEHIND and DIVERGED warn instead of failing because one of them is expected
# briefly: a queued merge group deploys before it merges, so a push that lands
# while the next group's deploy has already promoted `:latest` reads DIVERGED
# until that group merges. A red main on that interleaving would be a false
# alarm. UNKNOWN fails, because a broken check must never read as a converged
# prod.

set -uo pipefail

subject="${CONVERGENCE_SUBJECT:-ghcr.io/devantler-tech/platform/manifests}"
main_ref="${CONVERGENCE_MAIN_REF:-origin/main}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

fail() {
  printf '::error title=Production convergence unknown::%s\n' "$1"
  printf '### 🧭 Production convergence\n\n**UNKNOWN**: %s\n' "$1" >>"$summary"
  exit 1
}

err_file="$(mktemp)"
trap 'rm -f "$err_file"' EXIT

# stderr is kept apart from the digest: a harmless warning must not read as a broken digest.
if ! digest="$(docker buildx imagetools inspect "${subject}:latest" --format '{{.Manifest.Digest}}' 2>"$err_file")"; then
  fail "could not read the digest ${subject}:latest names: $(tr '\n' ' ' <"$err_file")"
fi
digest="${digest//[[:space:]]/}"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "${subject}:latest resolved to '${digest}', not a sha256 digest"

# The resolver needs every attested commit locally. After a merge-group heal, `:latest`
# is attested with the ejected group's commit, whose queue branch is already gone, so a
# normal checkout lacks it and the verdict would be UNKNOWN for a prod that is fine.
# The commit list read here only decides what to FETCH; the resolver verifies every
# attestation again, so a failure here is left for it to report.
if attested="$(gh attestation verify "oci://${subject}@${digest}" --bundle-from-oci \
  --repo devantler-tech/platform --predicate-type https://slsa.dev/provenance/v1 \
  --format json --jq '.[].verificationResult.signature.certificate.sourceRepositoryDigest' 2>"$err_file")"; then
  for commit in $attested; do
    [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || continue
    git cat-file -e "${commit}^{commit}" 2>/dev/null ||
      git fetch --quiet --no-tags origin "$commit" ||
      printf '::warning::could not fetch attested commit %s\n' "$commit"
  done
fi

if output="$(go run ./scripts/resolve-prod-convergence --digest "$digest" --subject "$subject" --main-ref "$main_ref")"; then
  rc=0
else
  rc=$?
fi
verdict="${output%% *}"

case "${rc}:${verdict}" in
  0:CONVERGED)
    printf '::notice title=Production converged::%s\n' "$output"
    ;;
  1:BEHIND | 1:DIVERGED)
    printf '::warning title=Production is not on main::%s (digest %s). A queued merge group that deployed before merging also reads DIVERGED until it merges.\n' "$output" "$digest"
    ;;
  *)
    fail "resolver exited ${rc} with '${output}' for ${digest}"
    ;;
esac

printf 'verdict=%s\n' "$verdict" >>"${GITHUB_OUTPUT:-/dev/null}"

# shellcheck disable=SC2016 # the backticks are Markdown code spans, not expansions
printf '### 🧭 Production convergence\n\n`%s` for `%s@%s` against `%s`.\n' "$output" "$subject" "$digest" "$main_ref" >>"$summary"
