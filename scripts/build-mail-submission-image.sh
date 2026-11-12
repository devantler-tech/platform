#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="${1:?usage: build-mail-submission-image.sh image-tag}"
[[ $# == 1 && $(uname -s) == Linux ]]
export GOTOOLCHAIN=local GOWORK=off
export DOCKER_BUILDKIT=1
[[ $(go env GOVERSION) == "go$(awk '$1 == "go" { print $2 }' "$root/go.mod")" ]]
[[ "${GITHUB_SHA:?}" =~ ^[0-9a-f]{40}$ ]]
stage="${RUNNER_TEMP:?}/mail-submission-context"
packaged="${RUNNER_TEMP}/mail-submission-packaged"
mkdir "$stage" "$packaged"
mkdir "$stage/licenses"

CGO_ENABLED=1 go -C "$root" test -mod=readonly -count=1 -race -timeout=3m ./scripts/mail-submission
go -C "$root" vet ./scripts/mail-submission
CGO_ENABLED=1 go -C "$root/third_party/go-smtp" test -mod=readonly -count=1 -race -timeout=2m ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$root" build -mod=readonly -trimpath \
  -ldflags='-s -w' -o "$stage/mail-submission" ./scripts/mail-submission
cp "$(go env GOROOT)/LICENSE" "$stage/licenses/GO-LICENSE"
cp "$(go env GOROOT)/PATENTS" "$stage/licenses/GO-PATENTS"
cp "$root/third_party/go-smtp/LICENSE" "$stage/licenses/SMTP-LICENSE"
cp "$(go -C "$root" list -m -f '{{.Dir}}' github.com/emersion/go-sasl)/LICENSE" "$stage/licenses/SASL-LICENSE"
cp "$(go -C "$root" list -m -f '{{.Dir}}' golang.org/x/time)/LICENSE" "$stage/licenses/TIME-LICENSE"
cp "$root/scripts/mail-submission/IMAGE-NOTICE" "$stage/licenses/NOTICE"
chmod 0444 "$stage/licenses/"*
chmod 0555 "$stage/licenses"
docker build --platform linux/amd64 --file "$root/scripts/mail-submission/Dockerfile" \
  --label org.opencontainers.image.source=https://github.com/devantler-tech/platform \
  --label "org.opencontainers.image.revision=${GITHUB_SHA}" --tag "$image" "$stage"
docker image inspect "$image" | jq -e 'length == 1 and (.[0] | .Os == "linux" and .Architecture == "amd64" and .Config.User == "65532:65532" and .Config.Entrypoint == ["/usr/local/bin/mail-submission"] and ((.Config.Cmd // []) | length == 0))' >/dev/null

container="$(docker create --platform linux/amd64 "$image" check --config-dir /missing)"
trap 'docker rm "$container" >/dev/null' EXIT
docker cp "$container:/usr/local/bin/mail-submission" "$packaged/mail-submission"
docker cp "$container:/usr/local/share/licenses/mail-submission" "$packaged/licenses"
cmp "$stage/mail-submission" "$packaged/mail-submission"
diff -r "$stage/licenses" "$packaged/licenses"
MAIL_SUBMISSION_BINARY="$packaged/mail-submission" go -C "$root" test -count=1 -timeout=1m \
  -run '^TestPackagedExecutable$' ./scripts/mail-submission

# Default image identity starts without capabilities, writable root or network.
# No private inputs are embedded, so this must refuse configuration cleanly.
set +e
output="$(docker run --rm --platform linux/amd64 --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges "$image" check --config-dir /missing 2>&1)"
status=$?
set -e
[[ $status == 1 && "$output" == 'Submission configuration refused' ]]
docker save --output "${RUNNER_TEMP}/mail-submission-image.tar" "$image"
sha256sum "${RUNNER_TEMP}/mail-submission-image.tar" | awk '{print "archive_sha=" $1}' >>"${GITHUB_OUTPUT:?}"
docker image inspect --format 'image_id={{.Id}}' "$image" >>"$GITHUB_OUTPUT"
sha256sum "$packaged/mail-submission" | awk '{print "binary_sha=" $1}' >>"$GITHUB_OUTPUT"
