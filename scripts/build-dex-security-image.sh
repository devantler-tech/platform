#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_dir="${1:?usage: build-dex-security-image.sh upstream-directory image-tag}"
image="${2:?usage: build-dex-security-image.sh upstream-directory image-tag}"
source_dir="$(cd "$source_dir" && pwd)"
[[ $# == 2 && $(uname -s) == Linux && $(uname -m) == x86_64 ]]
[[ $(go env GOVERSION) == go1.27.3 ]]
[[ $(git -C "$source_dir" rev-parse HEAD) == 75eb19238e10fc4750e7efb05a4031b254b04029 ]]
[[ -z $(git -C "$source_dir" status --porcelain) ]]
git -C "$source_dir" merge-base --is-ancestor 5bbdb4420254ba73b9c4df4775fe7bdacf233b17 HEAD
git -C "$source_dir" apply --check --unidiff-zero "$root/k8s/bases/infrastructure/controllers/dex/dex-v2.46.1-network.patch"
git -C "$source_dir" apply --unidiff-zero "$root/k8s/bases/infrastructure/controllers/dex/dex-v2.46.1-network.patch"
git -C "$source_dir" diff --check
git -C "$source_dir" add -- go.mod go.sum
[[ $(git -C "$source_dir" write-tree) == 6ba6ad659479412f2a33c391e6c64a3cfcc3f05c ]]

export GOTOOLCHAIN=local GOWORK=off
export GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org GOVCS='*:off'
export GOPRIVATE='' GONOPROXY='' GONOSUMDB=''
CGO_ENABLED=1 go -C "$source_dir" test -mod=readonly -timeout=5m ./cmd/dex ./server ./storage/memory ./connector/oidc
git -C "$source_dir" diff --exit-code
[[ -z $(git -C "$source_dir" ls-files --others --exclude-standard) ]]
mkdir "$source_dir/dist"
# Static libc preserves SQLite without adding a second dynamic runtime to the image.
CGO_ENABLED=1 go -C "$source_dir" build -mod=readonly -trimpath -tags netgo,osusergo \
  -ldflags '-X main.version=v2.46.1-devantler.1 -linkmode external -extldflags "-static"' \
  -o "$source_dir/dist/dex" ./cmd/dex
file "$source_dir/dist/dex" | grep -q 'statically linked'
mkdir -p "${RUNNER_TEMP:?}/dex-tools"
GOBIN="$RUNNER_TEMP/dex-tools" go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://vuln.go.dev/ID/GO-2024-2476.json --output "$RUNNER_TEMP/dex-advisory.json"
DEX_NATIVE_BINARY="$source_dir/dist/dex" DEX_NATIVE_WEB="$source_dir/web" \
  go -C "$root" test -count=1 ./scripts/check-dex-binary
go -C "$root" run ./scripts/check-dex-binary --binary "$source_dir/dist/dex" \
  --scanner "$RUNNER_TEMP/dex-tools/govulncheck" --advisory "$RUNNER_TEMP/dex-advisory.json" --web "$source_dir/web"

# Go copies dependency notices when vendoring. Keep only those documents in the
# distribution; no dependency source, tests, credentials or build caches are shipped.
go -C "$source_dir" mod vendor
mkdir -p "$source_dir/dist/licenses"
cp "$source_dir/LICENSE" "$source_dir/dist/licenses/LICENSE"
cp "$root/k8s/bases/infrastructure/controllers/dex/IMAGE-NOTICE" "$source_dir/dist/licenses/NOTICE"
(
  cd "$source_dir"
  find vendor -type f \( -iname 'license*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'authors*' \) -print0 |
    xargs -0 -r cp --parents --target-directory=dist/licenses
)
[[ $(find "$source_dir/dist/licenses/vendor" -type f | wc -l) -gt 0 ]]
mkdir "$RUNNER_TEMP/dex-context"
cp -R "$source_dir/dist" "$source_dir/web" "$RUNNER_TEMP/dex-context/"
docker build --file "$root/k8s/bases/infrastructure/controllers/dex/DexSecurity.Dockerfile" \
  --label org.opencontainers.image.source=https://github.com/devantler-tech/platform \
  --label "org.opencontainers.image.revision=${GITHUB_SHA:?}" \
  --label io.devantler.dex.upstream-revision=75eb19238e10fc4750e7efb05a4031b254b04029 \
  --tag "$image" "$RUNNER_TEMP/dex-context"
docker image inspect "$image" | jq -e 'length == 1 and (.[0] | .Os == "linux" and .Architecture == "amd64" and .Config.User == "65534:65534" and ((.Config.Entrypoint // []) | length == 0))' > /dev/null
docker run --rm --read-only --cap-drop=ALL --security-opt=no-new-privileges "$image" dex version

# Inspect the packaged bytes, not just the pre-image build directory.
container_id="$(docker create "$image")"
trap 'docker rm "$container_id" >/dev/null' EXIT
mkdir "$RUNNER_TEMP/dex-packaged"
docker cp "$container_id:/usr/local/bin/dex" "$RUNNER_TEMP/dex-packaged/dex"
docker cp "$container_id:/srv/dex/web" "$RUNNER_TEMP/dex-packaged/web"
cmp "$source_dir/dist/dex" "$RUNNER_TEMP/dex-packaged/dex"
go -C "$root" run ./scripts/check-dex-binary --binary "$RUNNER_TEMP/dex-packaged/dex" \
  --scanner "$RUNNER_TEMP/dex-tools/govulncheck" --advisory "$RUNNER_TEMP/dex-advisory.json" --web "$RUNNER_TEMP/dex-packaged/web"
docker save --output "$RUNNER_TEMP/dex-image.tar" "$image"
sha256sum "$RUNNER_TEMP/dex-image.tar" | awk '{print "archive_sha=" $1}' >> "${GITHUB_OUTPUT:?}"
docker image inspect --format 'image_id={{.Id}}' "$image" >> "$GITHUB_OUTPUT"
sha256sum "$source_dir/dist/dex" | awk '{print "binary_sha=" $1}' >> "$GITHUB_OUTPUT"
