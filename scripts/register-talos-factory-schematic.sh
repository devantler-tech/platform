#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
schematic_file="$root_dir/talos/factory-schematic.yaml"
cluster_config="$root_dir/ksail.prod.yaml"
factory_url=${TALOS_FACTORY_URL:-https://factory.talos.dev}
curl_bin=${CURL_BIN:-curl}

for dependency in jq yq; do
  command -v "$dependency" >/dev/null 2>&1 || {
    printf 'required command is unavailable: %s\n' "$dependency" >&2
    exit 1
  }
done

[[ -f "$schematic_file" ]] || {
  printf 'canonical Talos Image Factory schematic is missing: %s\n' "$schematic_file" >&2
  exit 1
}

if command -v sha256sum >/dev/null 2>&1; then
  schematic_id=$(sha256sum "$schematic_file" | awk '{print $1}')
else
  schematic_id=$(shasum -a 256 "$schematic_file" | awk '{print $1}')
fi

talos_version=$(yq eval '.spec.cluster.talos.version' "$cluster_config")
[[ "$talos_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] || {
  printf 'invalid Talos version in ksail.prod.yaml: %s\n' "$talos_version" >&2
  exit 1
}

response=$(
  "$curl_bin" --fail-with-body --silent --show-error \
    --connect-timeout 15 --max-time 300 \
    --retry 3 --retry-all-errors \
    --request POST \
    --header 'Content-Type: application/yaml' \
    --data-binary "@$schematic_file" \
    "$factory_url/schematics"
)
registered_id=$(jq -er '.id | select(type == "string" and length == 64)' <<<"$response")

[[ "$registered_id" == "$schematic_id" ]] || {
  printf 'Talos Image Factory returned schematic ID %s, expected canonical hash %s\n' \
    "$registered_id" "$schematic_id" >&2
  exit 1
}

image_url="$factory_url/image/$schematic_id/$talos_version/hcloud-amd64.raw.xz"
"$curl_bin" --fail --silent --show-error --head \
  --connect-timeout 15 --max-time 300 \
  --retry 5 --retry-all-errors --retry-delay 5 \
  "$image_url" >/dev/null

printf 'Talos Image Factory schematic %s is registered and image %s is available\n' \
  "$schematic_id" "$talos_version"
