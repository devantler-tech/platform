#!/usr/bin/env bash
set -euo pipefail

# Exercise the deployed action's actual verifier invocation. A release change
# must never keep verifying an unrelated, previously published runtime digest.
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
mkdir -p "${scratch}/bin" "${scratch}/scripts" "${scratch}/k8s/clusters/prod/bootstrap" \
  "${scratch}/k8s/bases/apps/data-product-controller" \
  "${scratch}/k8s/providers/hetzner/infrastructure/controllers/flux-instance"
for file in k8s/clusters/prod/bootstrap/config-map.yaml \
  k8s/bases/apps/data-product-controller/helm-release.yaml \
  k8s/bases/apps/data-product-controller/deployment-contract-probe.yaml \
  k8s/bases/apps/data-product-controller/oci-repository.yaml \
  k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml; do
  cp "${root_dir}/${file}" "${scratch}/${file}"
done
index="sha256:$(printf 'a%.0s' {1..64})"
amd64="sha256:$(printf 'b%.0s' {1..64})"
arm64="sha256:$(printf 'c%.0s' {1..64})"
export index amd64 arm64
probe_index="sha256:$(printf 'e%.0s' {1..64})"
probe_child="sha256:$(printf 'f%.0s' {1..64})"
probe_image="ghcr.io/devantler-tech/khoist:9.0.5@${probe_index}"
export probe_index probe_child probe_image
probe_manifest="${scratch}/k8s/bases/apps/data-product-controller/deployment-contract-probe.yaml"
probe_image="$probe_image" yq -i '.spec.template.spec.containers[0].image = strenv(probe_image)' "$probe_manifest"
image_manifest="${scratch}/k8s/bases/apps/data-product-controller/helm-release.yaml"
index="$index" yq -i '.spec.values.image.digest = strenv(index)' "$image_manifest"
yq -r '.runs.steps[] | select(.name == "Verify the deployed data product UI release") | .run' \
  "${root_dir}/.github/actions/deploy-prod/action.yml" >"${scratch}/verify.sh"
legacy_runtime="$(yq -r '.runs.steps[] | select(.name == "Verify the deployed data product UI release") | .env.DPC_RUNTIME_DIGEST // ""' \
  "${root_dir}/.github/actions/deploy-prod/action.yml")"

cat >"${scratch}/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "buildx imagetools inspect ghcr.io/devantler-tech/data-product-controller@${index} --raw")
    manifest=$REGISTRY_MANIFEST; failure=${REGISTRY_FAILURE:-false} ;;
  "buildx imagetools inspect ${probe_image} --raw")
    manifest=$REGISTRY_PROBE_MANIFEST; failure=${PROBE_REGISTRY_FAILURE:-false} ;;
  *) exit 91 ;;
esac
[[ "$failure" != empty ]] || exit 92
cat "$manifest"
[[ "$failure" == false ]] || exit 92
SH
cat >"${scratch}/scripts/verify-data-product-ui-rollout.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
actual=()
actual_probe=()
while (($#)); do
  case "$1" in
    --runtime-digest) actual+=("$2"); shift 2 ;;
    --probe-runtime-digest) actual_probe+=("$2"); shift 2 ;;
    --image-digest) [[ "$2" == "$index" ]] || exit 93; shift 2 ;;
    --context|--domain|--chart-digest|--apps-digest|--apps-verify-file) shift 2 ;;
    *) exit 94 ;;
  esac
done
printf '%s\n' "${actual[@]}" | sort -u >"${RUNNER_TEMP}/actual"
cmp -s "${RUNNER_TEMP}/expected" "${RUNNER_TEMP}/actual" || exit 95
[[ ${#actual_probe[@]} -gt 0 ]] || exit 96
printf '%s\n' "${actual_probe[@]}" | sort -u >"${RUNNER_TEMP}/actual-probe"
cmp -s "${RUNNER_TEMP}/expected-probe" "${RUNNER_TEMP}/actual-probe" || exit 97
touch "${RUNNER_TEMP}/verified"
SH
chmod +x "${scratch}/bin/docker" "${scratch}/scripts/verify-data-product-ui-rollout.sh"
export REGISTRY_MANIFEST="${scratch}/manifest.json"
export REGISTRY_PROBE_MANIFEST="${scratch}/probe-manifest.json"
export RUNNER_TEMP="$scratch"
jq -n --arg child "$probe_child" '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",manifests:[
  {digest:$child,platform:{os:"linux",architecture:"amd64"}},
  {digest:"sha256:attestation-not-a-runtime",platform:{os:"unknown",architecture:"unknown"}}]}' >"$REGISTRY_PROBE_MANIFEST"
# Native CRI status can report the published index instead of its platform child.
# Both identities belong to this validated immutable release; attestations do not.
printf '%s\n' "$probe_index" "$probe_child" | sort -u >"${scratch}/expected-probe"

run_case() {
  local name=$1 expected=$2 registry_failure=${3:-false} probe_registry_failure=${4:-false} result=0
  rm -f "${scratch}/verified" "${scratch}/actual"
  (
    cd "$scratch"
    PATH="${scratch}/bin:$PATH" DPC_RUNTIME_DIGEST="$legacy_runtime" \
      PLATFORM_MANIFEST_DIGEST="$index" REGISTRY_FAILURE="$registry_failure" \
      PROBE_REGISTRY_FAILURE="$probe_registry_failure" \
      bash -e -o pipefail verify.sh
  ) >"${scratch}/stdout" 2>"${scratch}/stderr" || result=$?
  if [[ "$expected" == pass ]]; then
    [[ "$result" == 0 && -f "${scratch}/verified" ]] || {
      printf 'FAIL: %s did not verify the selected release runtime digests (exit %s)\n' "$name" "$result" >&2
      exit 1
    }
  else
    [[ "$result" != 0 && ! -f "${scratch}/verified" ]] || {
      printf 'FAIL: %s reached verification with unproven runtime digests\n' "$name" >&2
      exit 1
    }
  fi
  printf 'PASS: %s\n' "$name"
}

jq -n --arg amd64 "$amd64" --arg arm64 "$arm64" '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",manifests:[
  {digest:$amd64,platform:{os:"linux",architecture:"amd64"}},
  {digest:$arm64,platform:{os:"linux",architecture:"arm64"}},
  {digest:"sha256:attestation-not-a-runtime",platform:{os:"unknown",architecture:"unknown"}}]}' >"$REGISTRY_MANIFEST"
printf '%s\n' "$index" "$amd64" "$arm64" | sort -u >"${scratch}/expected"
run_case changed-multi-platform-release pass

jq '.mediaType="application/vnd.docker.distribution.manifest.list.v2+json"' "$REGISTRY_MANIFEST" >"${scratch}/docker-index.json"
cp "${scratch}/docker-index.json" "$REGISTRY_MANIFEST"
run_case docker-manifest-list pass

printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}\n' >"$REGISTRY_MANIFEST"
printf '%s\n' "$index" >"${scratch}/expected"
run_case single-platform-release pass
printf '{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{},"layers":[]}\n' >"$REGISTRY_MANIFEST"
run_case single-platform-docker-release pass

printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}\n' >"$REGISTRY_MANIFEST"
run_case empty-index fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"latest","platform":{"os":"linux","architecture":"amd64"}}]}\n' >"$REGISTRY_MANIFEST"
run_case malformed-runtime-digest fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:ignored","platform":{"os":"unknown","architecture":"unknown"}}]}\n' >"$REGISTRY_MANIFEST"
run_case no-supported-runtime fail
printf '{"schemaVersion":1,"mediaType":"application/vnd.oci.image.manifest.v1+json"}\n' >"$REGISTRY_MANIFEST"
run_case wrong-schema fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.unknown"}\n' >"$REGISTRY_MANIFEST"
run_case unknown-media-type fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}\n{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}\n' >"$REGISTRY_MANIFEST"
run_case multiple-manifests fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":null,"platform":{"os":"linux","architecture":"amd64"}}]}\n' >"$REGISTRY_MANIFEST"
run_case missing-runtime-digest fail
printf 'not JSON\n' >"$REGISTRY_MANIFEST"
run_case invalid-json fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}\n' >"$REGISTRY_MANIFEST"
run_case registry-read-failure fail true
run_case empty-registry-read-failure fail empty

run_case probe-registry-read-failure fail false true
run_case empty-probe-registry-read-failure fail false empty
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}\n' >"$REGISTRY_PROBE_MANIFEST"
run_case empty-probe-index fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"latest","platform":{"os":"linux","architecture":"amd64"}}]}\n' >"$REGISTRY_PROBE_MANIFEST"
run_case malformed-probe-runtime fail
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}\n' >"$REGISTRY_PROBE_MANIFEST"
printf '%s\n' "$probe_index" >"${scratch}/expected-probe"
run_case single-platform-probe pass
probe_image='unapproved.example/other:latest' yq -i '.spec.template.spec.containers[0].image = strenv(probe_image)' "$probe_manifest"
run_case unapproved-probe-image fail
