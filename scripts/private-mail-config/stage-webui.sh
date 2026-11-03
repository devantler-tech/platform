#!/usr/bin/env bash
set -euo pipefail
umask 077
[ "$#" -ge 1 ] && [ "$#" -le 2 ] && [ -d "$1" ] && [ ! -L "$1" ] || { echo 'An existing application build directory is required.' >&2; exit 2; }
destination=$1
case "$destination" in /*) ;; *) echo 'Application build directory must be absolute.' >&2; exit 2 ;; esac
for name in webui.zip webui.LICENSE webui.SOURCE; do
  if [ -e "$destination/$name" ] || [ -L "$destination/$name" ]; then
    echo 'Administration build artifacts already exist; refusing to replace them.' >&2
    exit 2
  fi
done
root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "${2:-0.16.27}" in
  0.16.27) pins="$root/webui-pins.json" ;;
  1.0.2) pins="$root/webui-v1-pins.json" ;;
  *) echo 'Unsupported Email runtime version.' >&2; exit 2 ;;
esac
version="$(jq -er '.version | select(type == "string" and test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))' "$pins")"
url="$(jq -er '.url | select(type == "string")' "$pins")"
digest="$(jq -er '.sha256 | select(type == "string" and test("^[0-9a-f]{64}$"))' "$pins")"
license_digest="$(jq -er '.license_sha256 | select(type == "string" and test("^[0-9a-f]{64}$"))' "$pins")"
[ "$url" = "https://github.com/stalwartlabs/webui/releases/download/v${version}/webui.zip" ] || {
  echo 'Application release metadata is invalid.' >&2; exit 2;
}
work="$(mktemp -d "$destination/.webui-stage.XXXXXX")"
trap 'rm -rf "$work"' EXIT
curl --disable --fail --silent --show-error --location --max-time 60 --max-filesize 1048576 --proto '=https' --proto-redir '=https' \
  --output "$work/webui.zip" "$url"
curl --disable --fail --silent --show-error --location --max-time 60 --max-filesize 65536 --proto '=https' --proto-redir '=https' \
  --output "$work/webui.LICENSE" "https://raw.githubusercontent.com/stalwartlabs/webui/v${version}/LICENSES/AGPL-3.0-only.txt"
if ! printf '%s  %s\n%s  %s\n' "$digest" "$work/webui.zip" "$license_digest" "$work/webui.LICENSE" | shasum -a 256 --check >/dev/null 2>&1; then
  echo 'Administration asset integrity verification failed.' >&2
  exit 2
fi
printf 'Unmodified upstream source: https://github.com/stalwartlabs/webui/tree/v%s\n' "$version" >"$work/webui.SOURCE"
for name in webui.zip webui.LICENSE webui.SOURCE; do
  (set -C; cat "$work/$name" >"$destination/$name")
  chmod 0644 "$destination/$name"
done
echo 'Verified pinned administration assets staged.'
