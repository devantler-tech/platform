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
# A controlled non-shipping executable keeps serving its original certificate.
# The renewal control must reject it; sending a signal alone used to pass.
original_serve="${RUNNER_TEMP:?}/dex-serve-original.go"
cp "$source_dir/cmd/dex/serve.go" "$original_serve"
trap 'cp "$original_serve" "$source_dir/cmd/dex/serve.go"' EXIT
[[ $(grep -Fc 'ptr.Store(loaded)' "$original_serve") == 1 ]]
sed 's/ptr.Store(loaded)/_ = loaded/' "$original_serve" >"$source_dir/cmd/dex/serve.go"
CC=musl-gcc CGO_ENABLED=1 go -C "$source_dir" build -mod=readonly -trimpath -tags netgo,osusergo \
  -ldflags '-X main.version=v2.46.1-devantler.1 -linkmode external -extldflags "-static"' \
  -o "$RUNNER_TEMP/dex-no-reload" ./cmd/dex
cp "$original_serve" "$source_dir/cmd/dex/serve.go"
trap - EXIT
git -C "$source_dir" diff --exit-code
mkdir "$source_dir/dist"
# Use musl's static runtime, preserving SQLite without introducing glibc into
# the distribution. Go handles network and user lookups directly.
CC=musl-gcc CGO_ENABLED=1 go -C "$source_dir" build -mod=readonly -trimpath -tags netgo,osusergo \
  -ldflags '-X main.version=v2.46.1-devantler.1 -linkmode external -extldflags "-static"' \
  -o "$source_dir/dist/dex" ./cmd/dex
file "$source_dir/dist/dex" | grep -q 'statically linked'
mkdir -p "${RUNNER_TEMP:?}/dex-tools"
GOBIN="$RUNNER_TEMP/dex-tools" go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://vuln.go.dev/ID/GO-2024-2476.json --output "$RUNNER_TEMP/dex-advisory.json"
DEX_NATIVE_BINARY="$source_dir/dist/dex" DEX_STALE_CERTIFICATE_BINARY="$RUNNER_TEMP/dex-no-reload" DEX_NATIVE_WEB="$source_dir/web" \
  go -C "$root" test -count=1 ./scripts/check-dex-binary
go -C "$root" run ./scripts/check-dex-binary --binary "$source_dir/dist/dex" \
  --scanner "$RUNNER_TEMP/dex-tools/govulncheck" --advisory "$RUNNER_TEMP/dex-advisory.json" --web "$source_dir/web"

# Go copies dependency notices when vendoring. Keep only those documents in the
# distribution; no dependency source, tests, credentials or build caches are shipped.
go -C "$source_dir" mod vendor
mkdir -p "$source_dir/dist/licenses"
cp "$source_dir/LICENSE" "$source_dir/dist/licenses/LICENSE"
cp "$root/k8s/bases/infrastructure/controllers/dex/IMAGE-NOTICE" "$source_dir/dist/licenses/NOTICE"
cp "$(go env GOROOT)/LICENSE" "$source_dir/dist/licenses/GO-LICENSE"
cp "$(go env GOROOT)/PATENTS" "$source_dir/dist/licenses/GO-PATENTS"
cp /usr/share/doc/musl/copyright "$source_dir/dist/licenses/MUSL-COPYRIGHT"
cp -L /usr/share/doc/libgcc-s1/copyright "$source_dir/dist/licenses/GCC-RUNTIME-COPYRIGHT"
cp /usr/share/common-licenses/GPL-3 "$source_dir/dist/licenses/GPL-3"
cp /usr/share/common-licenses/LGPL-3 "$source_dir/dist/licenses/LGPL-3"
cp /usr/share/common-licenses/LGPL-2.1 "$source_dir/dist/licenses/LGPL-2.1"
(
  cd "$source_dir"
  find vendor -type f \( -iname 'license*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'copyright*' -o -iname 'authors*' \) -print0 |
    xargs -0 -r cp --parents --target-directory=dist/licenses
)
[[ $(find "$source_dir/dist/licenses/vendor" -type f | wc -l) -gt 0 ]]
# Supply the complete patched upstream and vendored dependency sources, including
# MPL-covered dependency files, alongside their notices. This is passive data;
# no source helper or second executable is introduced into the runtime.
tar --exclude=./.git --exclude=./dist --exclude=./bin --exclude=./tmp \
  -czf "$source_dir/dist/licenses/source.tar.gz" -C "$source_dir" .
mkdir "$RUNNER_TEMP/dex-context"
cp -R "$source_dir/dist" "$source_dir/web" "$RUNNER_TEMP/dex-context/"
docker build --file "$root/k8s/bases/infrastructure/controllers/dex/DexSecurity.Dockerfile" \
  --label org.opencontainers.image.source=https://github.com/devantler-tech/platform \
  --label "org.opencontainers.image.revision=${GITHUB_SHA:?}" \
  --label io.devantler.dex.upstream-revision=75eb19238e10fc4750e7efb05a4031b254b04029 \
  --tag "$image" "$RUNNER_TEMP/dex-context"
docker image inspect "$image" | jq -e 'length == 1 and (.[0] | .Os == "linux" and .Architecture == "amd64" and .Config.User == "65534:65534" and ((.Config.Entrypoint // []) | length == 0))' >/dev/null
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
sha256sum "$RUNNER_TEMP/dex-image.tar" | awk '{print "archive_sha=" $1}' >>"${GITHUB_OUTPUT:?}"
docker image inspect --format 'image_id={{.Id}}' "$image" >>"$GITHUB_OUTPUT"
sha256sum "$source_dir/dist/dex" | awk '{print "binary_sha=" $1}' >>"$GITHUB_OUTPUT"
