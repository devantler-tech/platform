#!/usr/bin/env bash
# #4693: point a hosted runner's Docker daemon at Google's documented Docker Hub
# cache without changing any pinned image. Runtime fixtures call this before
# their first pull; local and self-hosted Docker daemons are never reconfigured.
set -euo pipefail

if [ "$#" != 0 ]; then
  printf 'FAIL: unexpected registry cache argument\n' >&2; exit 1;
fi
if [ "${GITHUB_ACTIONS:-}" != true ] ||
  [ "${RUNNER_ENVIRONMENT:-}" != github-hosted ] ||
  [ "${RUNNER_OS:-}" != Linux ]; then
    printf 'FAIL: registry cache setup requires a hosted Linux runner\n' >&2; exit 1;
fi
umask 077
cache_dir="$(mktemp -d)"
trap 'rm -rf "${cache_dir}"' EXIT
docker ps -aq >"${cache_dir}/containers"
[ ! -s "${cache_dir}/containers" ] || {
  printf 'FAIL: registry cache setup requires an unused Docker daemon\n' >&2; exit 1;
}
if sudo test -e /etc/docker/daemon.json; then
  sudo cat /etc/docker/daemon.json | tee "${cache_dir}/original.json" >/dev/null
else
  sudo test ! -e /etc/docker/daemon.json
  printf '{}\n' >"${cache_dir}/original.json"
fi
# Slurp rejects empty or multiple documents rather than treating either as
# an empty daemon configuration. No configuration values reach the job log.
jq -e -s '
  if length != 1 or (.[0] | type) != "object" then error("invalid daemon configuration")
  else .[0] end |
  if has("registry-mirrors") and
    (.["registry-mirrors"] | type != "array" or
      any(.[]; type != "string" or length == 0))
  then error("invalid registry mirrors") else . end |
  .["registry-mirrors"] = (["https://mirror.gcr.io"] +
    [(.["registry-mirrors"] // [])[] | select(. != "https://mirror.gcr.io")])
' "${cache_dir}/original.json" >"${cache_dir}/daemon.json" 2>/dev/null || {
  printf 'FAIL: invalid daemon or registry cache configuration\n' >&2; exit 1;
}
sudo dockerd --validate --config-file="${cache_dir}/daemon.json" >/dev/null
sudo mkdir -p /etc/docker
sudo install -m 600 "${cache_dir}/daemon.json" /etc/docker/daemon.json
sudo systemctl restart docker
docker info --format '{{json .RegistryConfig.Mirrors}}' |
  jq -e 'type == "array" and any(.[]; . == "https://mirror.gcr.io/" or . == "https://mirror.gcr.io")' >/dev/null
printf 'PASS: hosted Docker registry cache configured and read back\n'
