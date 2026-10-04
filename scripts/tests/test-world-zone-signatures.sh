#!/usr/bin/env bash
# Zone releases use one exact publisher without widening other image trust.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
policy="${root}/k8s/bases/infrastructure/cluster-policies/best-practices/verify-app-images.yaml"
talos="${root}/talos/cluster/verify-first-party-images.yaml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

subject="$(yq -r '.spec.attestors[] | select(.name == "publishwarzone") | .cosign.keyless.identities[0].subjectRegExp' "${policy}")"
[[ -n "${subject}" && "${subject}" != null ]] || fail 'a complete zone signature identity is required'
[[ "${subject}" == "$(yq -r '.rules[] | select(.image == "ghcr.io/devantler-tech/world-at-ruin/zone") | .keyless.subjectRegex' "${talos}")" ]] || fail 'admission and host signer identities must agree'
[[ "$(yq -r '.spec.attestors[] | select(.name == "publishwarzone") | .cosign.keyless.identities[0].issuer' "${policy}")" == 'https://token.actions.githubusercontent.com' ]] || fail 'the zone signer must retain the GitHub OIDC issuer'
[[ "$(yq -r '.rules[] | select(.image == "ghcr.io/devantler-tech/world-at-ruin/zone") | .keyless.issuer' "${talos}")" == 'https://token.actions.githubusercontent.com' ]] || fail 'host verification must retain the GitHub OIDC issuer'
for tag in v0.114.0 v1.2.3 v10.20.30; do
  [[ "https://github.com/devantler-tech/world-at-ruin/.github/workflows/server-cd.yaml@refs/tags/${tag}" =~ ${subject} ]] || fail "stable release ${tag} must be accepted"
done
for ref in refs/heads/main refs/tags/v1.0.0-rc.1 refs/tags/v01.2.3 refs/tags/v1.2.3/evil; do
  [[ ! "https://github.com/devantler-tech/world-at-ruin/.github/workflows/server-cd.yaml@${ref}" =~ ${subject} ]] || fail "non-release ${ref} must be refused"
done
for wrong in 'world-at-ruin/.github/workflows/cd.yaml' 'wedding-app/.github/workflows/server-cd.yaml'; do
  [[ ! "https://github.com/devantler-tech/${wrong}@refs/tags/v1.2.3" =~ ${subject} ]] || fail 'another repository or workflow must not sign the zone'
done
[[ "$(yq -r '.spec.validations[] | select(.expression | contains("attestors.publishwarzone")) | .expression' "${policy}")" == *"image == 'ghcr.io/devantler-tech/world-at-ruin/zone'"* ]] || fail 'the zone attestor must route the exact image repository'
[[ "$(yq -r '.spec.validations[] | select(.expression | contains("attestors.publishapp")) | .expression' "${policy}")" == *"image != 'ghcr.io/devantler-tech/world-at-ruin/zone'"* ]] || fail 'generic app verification must not also gate the zone'
[[ "$(yq -r '.spec.attestors[] | select(.name == "publishapp") | .cosign.keyless.identities[0].subjectRegExp' "${policy}")" != *world-at-ruin* ]] || fail 'the trial must not broaden the shared app signer'

# Exercise first-match image routing through the real inventory tool. The stub
# accepts only the subject selected by the rule, never an unrelated publisher.
cat >"${work}/verify" <<'SH'
#!/usr/bin/env bash
[[ "$3" == "${EXPECTED_SUBJECT}" ]]
SH
cat >"${work}/probe" <<'SH'
#!/usr/bin/env bash
printf '200\n'
SH
chmod +x "${work}/verify" "${work}/probe"
inventory="${root}/scripts/inventory-first-party-image-signatures.sh"
check_image() {
  local image="$1" expected="$2" want="$3" got=0
  printf '%s\n' "${image}" >"${work}/images"
  EXPECTED_SUBJECT="${expected}" INVENTORY_VERIFY_CMD="${work}/verify" INVENTORY_PROBE_CMD="${work}/probe" \
    bash "${inventory}" --rules "${talos}" --images "${work}/images" >"${work}/inventory.log" 2>&1 || got=$?
  [[ "${got}" == "${want}" ]] || fail "image signer routing for ${image} returned ${got}, wanted ${want}"
}
check_image 'ghcr.io/devantler-tech/world-at-ruin/zone:v0.114.0' "${subject}" 0
check_image 'ghcr.io/devantler-tech/world-at-ruin/zone:v0.114.0@sha256:aa' "${subject}" 0
check_image 'ghcr.io/devantler-tech/world-at-ruin/zone-helper:v0.114.0' "${subject}" 1
check_image 'ghcr.io/devantler-tech/wedding-app:v1.2.3' "${subject}" 1
app_subject="$(yq -r '.spec.attestors[] | select(.name == "publishapp") | .cosign.keyless.identities[0].subjectRegExp' "${policy}")"
check_image 'ghcr.io/devantler-tech/world-at-ruin/zone:v0.114.0' "${app_subject}" 1

printf 'PASS: exact zone release signer and image routing boundaries\n'
