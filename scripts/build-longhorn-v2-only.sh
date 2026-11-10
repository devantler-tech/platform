#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
build_files="$root/k8s/providers/hetzner/infrastructure/controllers/longhorn"
release=v1.13.0
manager_revision=20d9c0790312b45aafd5668674e73dbf2eba1832
manager_port=cbf776c32bc406304545ca36a7291730b95148ff
chart_revision=c3d88832bd8cec4f38dec65e79436984871dc313
manager_patch=1861d01f016889dd622f479978a4a06a410a0e9984f3211084d81cd90a797356
chart_patch=5cb59537b01c8099c7789ccbeffc930a27d48603436b8c0302c1043ed68f10ca

if [[ $# != 2 || "$1" != /* || -e "$1" || -L "$1" || ! "$2" =~ ^[1-9][0-9]{0,11}$ ]]; then
  printf '%s\n' 'Usage: build-longhorn-v2-only.sh NEW_ABSOLUTE_OUTPUT_DIR PUBLISH_RUN_NUMBER' >&2
  exit 1
fi
output="$1"
chart_version="1.13.0-platform.$2"
source_dir="$(mktemp -d)"
trap 'rm -rf -- "$source_dir"' EXIT

checkout_release() {
  local repository="$1" target="$2" revision="$3"
  git init --quiet "$target"
  git -C "$target" fetch --quiet --no-tags --depth=1 "$repository" "+refs/tags/$release:refs/tags/$release"
  if [[ "$(git -C "$target" rev-parse "refs/tags/$release^{commit}")" != "$revision" ]]; then
    printf '%s\n' 'The upstream release no longer resolves to its reviewed revision.' >&2
    exit 1
  fi
  git -C "$target" checkout --quiet --detach "$revision"
}

check_patch() {
  printf '%s  %s\n' "$1" "$2" | sha256sum --check
}

manager_source="$source_dir/manager"
chart_source="$source_dir/chart"
manager_diff="$build_files/longhorn-manager-v1.13.0-v2-only.patch"
chart_diff="$build_files/longhorn-chart-v1.13.0-v2-only.patch"
check_patch "$manager_patch" "$manager_diff"
check_patch "$chart_patch" "$chart_diff"
checkout_release https://github.com/longhorn/longhorn-manager.git "$manager_source" "$manager_revision"
checkout_release https://github.com/longhorn/longhorn.git "$chart_source" "$chart_revision"

helm template longhorn "$chart_source/chart" --namespace longhorn-system --kube-version 1.34.0 >"$source_dir/stock.yaml"
git -C "$chart_source" apply --check --whitespace=error-all "$chart_diff"
git -C "$chart_source" apply --whitespace=error-all "$chart_diff"
CHART_DEFAULT_BASELINE="$source_dir/stock.yaml" bash "$chart_source/scripts/test-data-engine-mode-chart.sh"
helm lint --strict "$chart_source/chart"

git -C "$manager_source" apply --check --whitespace=error-all "$manager_diff"
git -C "$manager_source" apply --whitespace=error-all "$manager_diff"
mkdir -- "$output"
mkdir -- "$output/manager" "$output/chart"
(
  cd "$manager_source"
  export GOENV=off GOFLAGS='' GOWORK=off GOTOOLCHAIN=local
  go test -race -count=1 -timeout=8m -run 'DataEngineMode|V2Only|Capability' \
    ./api ./app ./controller ./controller/monitor ./datastore ./manager ./types ./util/client ./webhook/...
  build_date="$(git show -s --format=%cI "$manager_revision")"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/longhorn/longhorn-manager/meta.Version=$release -X github.com/longhorn/longhorn-manager/meta.GitCommit=$manager_port -X github.com/longhorn/longhorn-manager/meta.BuildDate=$build_date" \
    -o "$output/manager/longhorn-manager" .
)
helm package --version "$chart_version" --destination "$output/chart" "$chart_source/chart"
(
  cd "$output"
  sha256sum manager/longhorn-manager "chart/longhorn-$chart_version.tgz" >SHA256SUMS
)
