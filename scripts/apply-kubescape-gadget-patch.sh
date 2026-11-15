#!/usr/bin/env bash
# Build Kubescape's node-agent against a reviewed patch of the exact
# inspektor-gadget fork it already uses (devantler-tech/platform#6327).
#
# node-agent replaces github.com/inspektor-gadget/inspektor-gadget with
# Kubescape's fork. This refuses unless that replace and its go.sum checksum are
# exactly the pinned fork version, downloads that version, refuses unless its
# module checksum matches, copies it into the node-agent source as
# third_party/inspektor-gadget, applies the reviewed patch strictly, and points
# the replace at that copy. Nothing else in the build changes.
#
# Usage: apply-kubescape-gadget-patch.sh <node-agent source dir> <patch file>
# Environment: GADGET_FORK_MODULE, GADGET_FORK_VERSION, GADGET_FORK_SUM.
set -euo pipefail

source_dir="$1"
patch_file="$2"
: "${GADGET_FORK_MODULE:?}" "${GADGET_FORK_VERSION:?}" "${GADGET_FORK_SUM:?}"
readonly upstream_module=github.com/inspektor-gadget/inspektor-gadget
readonly copy=third_party/inspektor-gadget

fail() {
  printf 'apply-kubescape-gadget-patch: %s\n' "$1" >&2
  exit 1
}

[[ -f "${patch_file}" ]] || fail 'the reviewed gadget patch is missing'
cd "${source_dir}"
[[ ! -e "${copy}" ]] || fail "${copy} already exists"

replaced="$(go mod edit -json | jq -r --arg old "${upstream_module}" '[.Replace[]? | select(.Old.Path == $old)] | if length == 1 then "\(.[0].New.Path) \(.[0].New.Version // "")" else "" end')"
[[ "${replaced}" == "${GADGET_FORK_MODULE} ${GADGET_FORK_VERSION}" ]] ||
  fail "node-agent no longer replaces ${upstream_module} with exactly ${GADGET_FORK_MODULE} ${GADGET_FORK_VERSION}"
grep -qxF "${GADGET_FORK_MODULE} ${GADGET_FORK_VERSION} ${GADGET_FORK_SUM}" go.sum ||
  fail 'node-agent go.sum does not record the pinned fork checksum'

download="$(go mod download -json "${GADGET_FORK_MODULE}@${GADGET_FORK_VERSION}")"
[[ "$(jq -r '.Sum' <<<"${download}")" == "${GADGET_FORK_SUM}" ]] ||
  fail 'the downloaded fork does not match its pinned checksum'
module_dir="$(jq -er '.Dir' <<<"${download}")"

mkdir -p "$(dirname "${copy}")"
cp -R "${module_dir}" "${copy}"
chmod -R u+w "${copy}"
git apply --check --directory="${copy}" "${patch_file}"
git apply --directory="${copy}" "${patch_file}"

go mod edit -replace="${upstream_module}=./${copy}"
[[ "$(go mod edit -json | jq -r --arg old "${upstream_module}" '[.Replace[]? | select(.Old.Path == $old) | .New.Path] | join(",")')" == "./${copy}" ]] ||
  fail 'the replace was not pointed at the patched copy'
printf 'node-agent now builds %s@%s with the reviewed patch from %s\n' "${GADGET_FORK_MODULE}" "${GADGET_FORK_VERSION}" "${copy}"
