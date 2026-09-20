#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
register_script="$root_dir/scripts/register-talos-factory-schematic.sh"
schematic="$root_dir/talos/factory-schematic.yaml"
cluster_config="$root_dir/ksail.prod.yaml"
apparmor_config="$root_dir/talos/cluster/enable-apparmor.yaml"
deploy_action="$root_dir/.github/actions/deploy-prod/action.yml"
ci_workflow="$root_dir/.github/workflows/ci.yaml"

[[ -x "$register_script" ]] || {
  printf 'Talos factory schematic registration script is missing or not executable\n' >&2
  exit 1
}

[[ -f "$schematic" ]] || {
  printf 'canonical Talos Image Factory schematic is missing\n' >&2
  exit 1
}

if ! grep -Fq "'scripts/register-talos-factory-schematic.sh'" "$ci_workflow" ||
  ! grep -Fq "'talos/**'" "$ci_workflow" ||
  ! grep -Fq "'scripts/tests/test-talos-factory-schematic-registration.sh'" "$ci_workflow" ||
  ! grep -Fq 'bash scripts/tests/test-talos-factory-schematic-registration.sh' "$ci_workflow"; then
  printf 'CI must detect and execute the Talos factory schematic registration contract\n' >&2
  exit 1
fi

register_line=$(grep -nF './scripts/register-talos-factory-schematic.sh' "$deploy_action" | cut -d: -f1)
publish_line=$(grep -nF 'id: publish_platform_manifest' "$deploy_action" | cut -d: -f1)
[[ -n "$register_line" && -n "$publish_line" && "$register_line" -lt "$publish_line" ]] || {
  printf 'deployment must register and verify the Talos schematic before publishing mutable production state\n' >&2
  exit 1
}

declared_extensions=$(yq eval -o=json '.spec.cluster.talos.extensions' "$cluster_config" | jq -r '.[]' | sort -u)
schematic_extensions=$(yq eval -o=json '.customization.systemExtensions.officialExtensions' "$schematic" | jq -r '.[]')
[[ "$declared_extensions" == "$schematic_extensions" ]] || {
  printf 'factory schematic extensions must exactly match normalized ksail.prod.yaml extensions\n' >&2
  exit 1
}

declared_args=$(yq eval -o=json '.machine.install.extraKernelArgs' "$apparmor_config" | jq -r '.[]')
schematic_args=$(yq eval -o=json '.customization.extraKernelArgs' "$schematic" | jq -r '.[]')
[[ "$declared_args" == "$schematic_args" ]] || {
  printf 'factory schematic kernel arguments must exactly match the Talos AppArmor patch\n' >&2
  exit 1
}

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
mock_log="$tmp_dir/curl.log"
mock_curl="$tmp_dir/curl"

cat >"$mock_curl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >>"$MOCK_CURL_LOG"
printf '\n' >>"$MOCK_CURL_LOG"

if [[ " $* " == *' --data-binary '* ]]; then
  case "$MOCK_SCENARIO" in
    success | unavailable)
      printf '{"id":"%s","schematic":"canonical"}\n' "$MOCK_SCHEMATIC_ID"
      ;;
    mismatch)
      printf '{"id":"%064d","schematic":"wrong"}\n' 0
      ;;
  esac
  exit 0
fi

if [[ "$MOCK_SCENARIO" == unavailable ]]; then
  exit 22
fi

[[ "${*: -1}" == "$MOCK_EXPECTED_IMAGE_URL" ]] || {
  printf 'unexpected image URL: %s\n' "${*: -1}" >&2
  exit 64
}
MOCK
chmod +x "$mock_curl"

schematic_id=$(sha256sum "$schematic" | awk '{print $1}')
talos_version=$(yq eval '.spec.cluster.talos.version' "$cluster_config")
factory_url='https://factory.test.invalid'
expected_image_url="$factory_url/image/$schematic_id/$talos_version/hcloud-amd64.raw.xz"

run_registration() {
  local scenario=$1
  MOCK_SCENARIO="$scenario" \
    MOCK_SCHEMATIC_ID="$schematic_id" \
    MOCK_EXPECTED_IMAGE_URL="$expected_image_url" \
    MOCK_CURL_LOG="$mock_log" \
    CURL_BIN="$mock_curl" \
    TALOS_FACTORY_URL="$factory_url" \
    "$register_script"
}

run_registration success
grep -Fq -- '--data-binary' "$mock_log" || {
  printf 'registration must POST the canonical schematic body\n' >&2
  exit 1
}
grep -Fq -- "$expected_image_url" "$mock_log" || {
  printf 'registration must verify the exact Talos version image URL\n' >&2
  exit 1
}

if run_registration mismatch >/dev/null 2>&1; then
  printf 'registration must reject a factory ID that differs from the canonical schematic hash\n' >&2
  exit 1
fi

if run_registration unavailable >/dev/null 2>&1; then
  printf 'registration must reject an unavailable image before production publication\n' >&2
  exit 1
fi

printf 'PASS: Talos factory schematic is declaratively registered before production publication\n'
