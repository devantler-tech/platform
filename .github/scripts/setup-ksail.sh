#!/usr/bin/env bash
set -euo pipefail

if [ -z "${KSAIL_VERSION:-}" ]; then
  echo "::error::KSAIL_VERSION must be set"
  exit 1
fi

asset_name="ksail_${KSAIL_VERSION}_linux_amd64.tar.gz"
release_url="https://github.com/devantler-tech/ksail/releases/download/v${KSAIL_VERSION}/${asset_name}"
api_url="https://api.github.com/repos/devantler-tech/ksail/releases/tags/v${KSAIL_VERSION}"
tarball="${RUNNER_TEMP:-/tmp}/${asset_name}"
release_json="${RUNNER_TEMP:-/tmp}/ksail-release-${KSAIL_VERSION}.json"

curl_headers=(-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
if [ -n "${GITHUB_TOKEN:-}" ]; then
  curl_headers+=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
fi

curl -fsSL "${release_url}" -o "${tarball}"
curl -fsSL "${curl_headers[@]}" "${api_url}" -o "${release_json}"

expected_digest=$(python3 - "${release_json}" "${asset_name}" <<'PY'
import json
import sys

release_path, asset_name = sys.argv[1:3]
with open(release_path, encoding="utf-8") as release_file:
    release = json.load(release_file)

for asset in release.get("assets", []):
    if asset.get("name") == asset_name:
        digest = asset.get("digest", "")
        if not digest.startswith("sha256:"):
            print(f"asset {asset_name} has no sha256 digest in GitHub release metadata", file=sys.stderr)
            sys.exit(1)
        print(digest.removeprefix("sha256:"))
        sys.exit(0)

print(f"asset {asset_name} was not found in release metadata", file=sys.stderr)
sys.exit(1)
PY
)

printf '%s  %s\n' "${expected_digest}" "${tarball}" | sha256sum --check --status

tar -xzf "${tarball}" -C "${RUNNER_TEMP:-/tmp}" ksail
sudo install "${RUNNER_TEMP:-/tmp}/ksail" /usr/local/bin/ksail
ksail --version
