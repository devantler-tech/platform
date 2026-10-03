#!/usr/bin/env bash
# Build the unmodified Helm command with the audited controller's SDK metadata.
set -euo pipefail
if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
  echo 'usage: build-controller-helm.sh <output-binary>' >&2
  exit 2
fi
module="$(cd "$(dirname "${BASH_SOURCE[0]}")/render-helm-chart" && pwd)"
output="$1"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GOWORK=off GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 GOFLAGS='' GOEXPERIMENT=''
unset GOOS GOARCH
go -C "$module" list -mod=readonly -m -json helm.sh/helm/v4 >"$work/sdk.json"
jq -e '.Version == "v4.2.0" and .Replace == null and .Sum == "h1:J+0TmTtPK2NuS6z9Z2WOcIX0nGGJylokEZLt0fi0X4U="' "$work/sdk.json" >/dev/null || {
  echo 'build-controller-helm: the audited Helm SDK module is required' >&2
  exit 2
}
go -C "$module" build -mod=readonly -trimpath -o "$output" helm.sh/helm/v4/cmd/helm
export HELM_CACHE_HOME="$work/cache" HELM_CONFIG_HOME="$work/config" HELM_DATA_HOME="$work/data" HELM_PLUGINS="$work/plugins"
metadata="$("$output" version --template '{{ printf "%#v" . }}')"
[ "$metadata" = 'version.BuildInfo{Version:"v4.2", GitCommit:"", GitTreeState:"", GoVersion:"go1.26.3", KubeClientVersion:"v1.36"}' ] || {
  echo 'build-controller-helm: the built command has unaudited capability metadata' >&2
  exit 2
}
