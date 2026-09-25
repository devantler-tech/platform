#!/usr/bin/env bash
# Verify the proposed Upbound Crossplane provider packages before merge (#4189).
#
# The first-party check (check-first-party-package-signatures.sh) covers only
# ghcr.io/devantler-tech packages. The Upbound AWS providers hold the platform's
# AWS CI identity, so a swapped or tampered package would run with it. Upbound
# signs every release keylessly from one build workflow, so each rendered
# xpkg.upbound.io/upbound/ package must carry a signature from exactly that
# workflow.
#
# Every run also proves the check is not vacuous. A deliberately wrong identity
# must be refused, and the real identity must still verify afterwards. The
# recheck separates a real refusal from a registry outage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

# Upbound's signing identity, read with `cosign verify` on
# provider-family-aws:v2.6.1 and provider-aws-iam:v2.6.1 (2026-09-25).
UPBOUND_ISSUER='https://token.actions.githubusercontent.com'
UPBOUND_IDENTITY='https://github.com/upbound/upbound-official-build/.github/workflows/supplychain.yml@refs/heads/main'
WRONG_IDENTITY='https://github.com/upbound/NOT-A-REAL-REPO/.github/workflows/supplychain.yml@refs/heads/main'
UPBOUND_PREFIX='xpkg.upbound.io/upbound/'
cosign="${COSIGN:-cosign}"

rendered=""
while [[ $# -gt 0 ]]; do
  [[ $# -ge 2 && "$1" == --rendered ]] || {
    echo 'expected --rendered PATH' >&2
    exit 2
  }
  rendered="$2"
  shift 2
done

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
if [[ -z "$rendered" ]]; then
  rendered="$scratch/rendered.yaml"
  # The same layers the first-party check renders: Flux reconciles them
  # separately, so building clusters/prod alone yields no Provider objects.
  for layer in k8s/clusters/prod k8s/clusters/prod/bootstrap \
    k8s/providers/hetzner/infrastructure/controllers \
    k8s/providers/hetzner/infrastructure k8s/providers/hetzner/apps; do
    if ! kubectl kustomize "$layer" >>"$rendered"; then
      echo "UNKNOWN: could not render $layer" >&2
      exit 1
    fi
    printf '\n---\n' >>"$rendered"
  done
fi

yq ea -r 'select(.apiVersion == "pkg.crossplane.io/v1" and .kind == "Provider") | .spec.package' \
  "$rendered" >"$scratch/all.txt"
# A prefix match, so a lookalike registry or organisation never qualifies.
{ grep -E "^xpkg\\.upbound\\.io/upbound/" "$scratch/all.txt" || [[ $? -eq 1 ]]; } | sort -u >"$scratch/packages.txt"
count="$(wc -l <"$scratch/packages.txt" | tr -d ' ')"
# Nothing to check would pass silently, so it is a failure. Upbound providers
# are rendered today; if they are removed, remove this check with them.
if [[ "$count" -eq 0 ]]; then
  echo "UNKNOWN: no $UPBOUND_PREFIX Provider package was rendered — refusing to pass vacuously" >&2
  exit 1
fi

# verify IDENTITY PACKAGE: succeed only when PACKAGE carries a keyless
# signature from IDENTITY, issued by GitHub Actions.
verify() {
  "$cosign" verify --certificate-oidc-issuer "$UPBOUND_ISSUER" \
    --certificate-identity "$1" "$2" >/dev/null 2>"$scratch/cosign.log"
}

while IFS= read -r package; do
  if ! verify "$UPBOUND_IDENTITY" "$package"; then
    echo "FAIL: $package is not signed by Upbound's build workflow" >&2
    tail -n 5 "$scratch/cosign.log" >&2
    exit 1
  fi
  echo "PASS $package"
  if verify "$WRONG_IDENTITY" "$package"; then
    echo "UNKNOWN: $package verified against a deliberately wrong identity — the check proves nothing" >&2
    exit 1
  fi
  # A refusal during an outage proves nothing, so the real identity must
  # still verify after the negative control.
  if ! verify "$UPBOUND_IDENTITY" "$package"; then
    echo "UNKNOWN: $package stopped verifying after the negative control, so its refusal is unproven" >&2
    exit 1
  fi
done <"$scratch/packages.txt"
echo "$count Upbound package signatures verified, and each refused a wrong identity"
