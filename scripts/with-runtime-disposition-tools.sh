#!/usr/bin/env bash
# Run one command with private, byte-verified native runtime-disposition tools.
# No system installation, GITHUB_PATH write or persistent PATH change.
set -euo pipefail
die() {
  echo "Runtime disposition tools refused: $*" >&2
  exit 2
}
kubectl_only=0
if [ "${1:-}" = --kubectl-only ]; then
  kubectl_only=1
  shift
fi
if [ "${1:-}" != -- ]; then
  [ "$#" -eq 0 ] && die 'command required after --'
  die 'unsupported option; expected [--kubectl-only] -- command args'
fi
shift
[ "$#" -gt 0 ] && [ -n "$1" ] || die 'command required after --'
platform="$(uname -s)/$(uname -m)"
case "$platform" in
Linux/x86_64)
  os=linux
  arch=amd64
  kyverno_arch=x86_64
  kubectl_sha=1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82
  kyverno_archive_sha=357eaf1df8bc24ddd5a73979fadeaae6a032e8710f11af264229756e4956bf4a
  kyverno_binary_sha=dd492a230e3f7c68235efbf925932cdb34cefbabbd4436d932c3287d9450eea8
  ;;
Darwin/arm64)
  os=darwin
  arch=arm64
  kyverno_arch=arm64
  kubectl_sha=4408c85c83fd3a31adaa555bdf3c7a6c81f74b19449a9060ba31ab91926f023d
  kyverno_archive_sha=7915c03c613c22363a100adb3da8d06f2e2626d9d21a98b97f921795ffdc536f
  kyverno_binary_sha=cdf8eac3745ea9f41abf6f8e0a6053feee50b27425b37c16e46cb92dd6afc897
  ;;
*) die "unsupported platform $platform" ;;
esac
for tool in mktemp mkdir cp chmod rm awk basename tar curl jq; do
  command -v "$tool" >/dev/null || die "$tool required"
done
if command -v sha256sum >/dev/null; then
  hash() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null; then
  hash() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die 'SHA-256 verifier required'
fi
umask 077
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/bin"
download() {
  curl --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --silent --show-error --location \
    --connect-timeout 10 --max-time 60 --max-filesize 268435456 --retry 2 --retry-delay 1 \
    --output "$2" "$1" || die 'approved release download failed'
}
verify() {
  [ "$(hash "$1")" = "$2" ] || die "checksum mismatch for $(basename "$1")"
}
installed() {
  local path
  path="$(command -v "$1" || true)"
  case "$path" in /*) ;; *) return 1 ;; esac
  [ -f "$path" ] && [ -r "$path" ] || return 1
  [ "$(hash "$path")" = "$2" ] || return 1
  cp "$path" "$work/bin/$1"
}
if ! installed kubectl "$kubectl_sha"; then
  download "https://dl.k8s.io/release/v1.36.2/bin/$os/$arch/kubectl" "$work/bin/kubectl"
fi
verify "$work/bin/kubectl" "$kubectl_sha"
chmod 0700 "$work/bin/kubectl"
version="$(KUBECONFIG=/dev/null "$work/bin/kubectl" version --client -o json)"
[ "$(jq -er '.clientVersion.gitVersion' <<<"$version")" = v1.36.2 ] || die 'kubectl version differs'
[ "$(jq -er '.kustomizeVersion' <<<"$version")" = v5.8.1 ] || die 'embedded Kustomize version differs'
if [ "$kubectl_only" -eq 0 ]; then
  if ! installed kyverno "$kyverno_binary_sha"; then
    archive="kyverno-cli_v1.19.2_${os}_${kyverno_arch}.tar.gz"
    download "https://github.com/kyverno/kyverno/releases/download/v1.19.2/$archive" "$work/kyverno.tar.gz"
    verify "$work/kyverno.tar.gz" "$kyverno_archive_sha"
    tar -xzf "$work/kyverno.tar.gz" -C "$work/bin" kyverno || die 'verified Kyverno extraction failed'
  fi
  verify "$work/bin/kyverno" "$kyverno_binary_sha"
  chmod 0700 "$work/bin/kyverno"
  version="$("$work/bin/kyverno" version)"
  [ "${version%%$'\n'*}" = 'Version: 1.19.2' ] || die 'Kyverno version differs'
fi
status=0
PATH="$work/bin:$PATH" "$@" || status=$?
exit "$status"
