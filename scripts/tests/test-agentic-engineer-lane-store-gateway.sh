#!/usr/bin/env bash
# Prove the kagent lane reaches its store's WebDAV methods through Substrate's egress gateway.
#
# A run reaches the lane store only through Substrate's egress gateway, agentgateway in AUTO mode.
# It decides whether a plaintext connection is HTTP from the connection's first bytes, and knows
# only nine standard methods there (crates/agentgateway/src/proxy/gateway.rs, `METHODS`). A
# PROPFIND, MKCOL, LOCK or UNLOCK that opens a connection is taken for opaque TCP and dropped, so the
# lane's first full run stopped at its first store request with an empty reply. After that first
# request the gateway parses every request on the connection as HTTP, whatever its method. The run
# message therefore sends each WebDAV method through a `dav` helper that opens its connection with
# an OPTIONS.
#
# This runs the gateway image Substrate deploys, with the AUTO bind Substrate configures for its
# plaintext egress, in front of the store's own pinned rclone, and proves:
# - a WebDAV method that opens a connection gets no reply at all, which is why the helper exists;
# - the run message's own helper reaches PROPFIND, MKCOL, LOCK and UNLOCK through the gateway, with
#   their real statuses, headers and bodies, and a standard PUT that carries the lease's token lands.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="${root}/k8s/providers/hetzner/apps/agentic-engineer"
# The gateway image Substrate's control-plane chart runs as atenet-egress. The chart, not this
# repository, chooses it, so the pin below is the one chart 1.0.0 deploys; moving the chart fails
# this test until the pin follows it.
gateway_image='cr.agentgateway.dev/agentgateway:v1.7.0@sha256:60216aafb03d37f94e952ff3d51d2f630a65b10ecaebdc0a864882f92610c45b'
gateway_chart='1.0.0'
work="$(mktemp -d)"
network=''
containers=()
completed=0
# Remove the containers, their network and the work directory on exit, and fail any exit that did
# not reach the PASS.
cleanup() {
  local name
  for name in "${containers[@]}"; do
    docker rm -f "${name}" >/dev/null 2>&1 || true
  done
  if [[ -n "${network}" ]]; then
    docker network rm "${network}" >/dev/null 2>&1 || true
  fi
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer lane store gateway: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer lane store gateway: %s\n' "$1" >&2
  exit 1
}
for tool in curl docker yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

chart="$(yq '.spec.ref.tag' "${root}/k8s/bases/infrastructure/substrate/oci-repository-control-plane.yaml")"
[[ "${chart}" == "${gateway_chart}" ]] ||
  fail "Substrate's chart is ${chart}, not ${gateway_chart}: pin the gateway image that chart deploys as atenet-egress here"

# The run message's store address, header and helper, exactly as a run defines them.
prompt="$(yq '.spec.template.spec.containers[] | select(.name == "schedule") | .env[] |
  select(.name == "PROMPT") | .value' "${dir}/job-schedule.yaml")"
helper="$(grep -E '^ *(S=|A=|dav\(\) )' <<<"${prompt}" || true)"
[[ "$(grep -c . <<<"${helper}")" == 3 ]] || fail "the run message does not define S, A and dav exactly once: ${helper}"
[[ "${helper}" == *'S=http://lane-store.agentic-engineer.svc.cluster.local:8080'* ]] ||
  fail 'the helper does not address the lane store'
[[ "${helper}" == *"A='Authorization: Basic kagent-credential-injected'"* ]] ||
  fail "the helper does not send the placeholder the store's egress policy replaces"
[[ "${helper}" == *'-X OPTIONS'*'--next'* ]] || fail 'the helper does not open its connection with an OPTIONS'
pattern='(<[?]xml [^`]*</D:lockinfo>)'
[[ "${prompt}" =~ ${pattern} ]] || fail 'the run message gives no lock body'
lockinfo="${BASH_REMATCH[1]}"

# The store's image and arguments, without the credential: the gateway's egress policy adds that,
# and it has no part in which methods pass.
deployment="${dir}/deployment-lane-store.yaml"
store_image="$(yq '.spec.template.spec.containers[] | select(.name == "webdav") | .image' "${deployment}")"
[[ "${store_image}" =~ ^docker\.io/rclone/rclone:[^@]+@sha256:[0-9a-f]{64}$ ]] ||
  fail "the store does not run a digest-pinned rclone: ${store_image}"
args=()
while IFS= read -r arg; do
  case "${arg}" in
    --htpasswd=*) ;;
    *) args+=("${arg}") ;;
  esac
done < <(yq '.spec.template.spec.containers[] | select(.name == "webdav") | .args[]' "${deployment}")
[[ "${args[*]:0:4}" == 'serve webdav /data --addr=:8080' ]] ||
  fail "the store no longer serves WebDAV from /data on :8080: ${args[*]}"

network="lane-store-gateway-$$-${RANDOM}"
docker network create "${network}" >/dev/null
store="${network}-store"
containers+=("${store}")
mkdir -p "${work}/data" "${work}/gateway"
docker run -d --name "${store}" --network "${network}" --user "$(id -u):$(id -g)" --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges --mount "type=bind,src=${work}/data,dst=/data" \
  "${store_image}" "${args[@]}" >/dev/null
store_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${store}")"
[[ -n "${store_ip}" ]] || fail 'the store got no address on the test network'
# Substrate's egress bind is AUTO; its HTTP listener carries every plaintext request a run sends.
# The HTTPS and TLS listeners beside it take only TLS, so they play no part in what follows.
cat >"${work}/gateway/config.yaml" <<YAML
binds:
- port: 3000
  protocol: AUTO
  listeners:
  - protocol: HTTP
    routes:
    - backends:
      - host: ${store_ip}:8080
YAML
chmod -R a+rX "${work}/gateway"
gateway="${network}-gateway"
containers+=("${gateway}")
docker run -d --name "${gateway}" --network "${network}" --read-only --tmpfs /tmp --cap-drop ALL \
  --security-opt no-new-privileges --mount "type=bind,src=${work}/gateway,dst=/etc/agentgateway,readonly" \
  -p 127.0.0.1::3000 "${gateway_image}" -f /etc/agentgateway/config.yaml >/dev/null

# Wait until a standard request crosses the gateway to the store.
url=''
for attempt in $(seq 1 30); do
  ports="$(docker port "${gateway}" 3000/tcp 2>/dev/null || true)"
  address="${ports%%$'\n'*}"
  if [[ -n "${address}" ]] &&
    [[ "$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -X OPTIONS "http://${address}/" 2>/dev/null || true)" == 200 ]]; then
    url="http://${address}"
    break
  fi
  sleep 1
done
[[ -n "${url}" ]] || fail "the gateway did not reach the store after ${attempt} tries: $(docker logs "${gateway}" 2>&1 | tail -c 800)"

# A WebDAV method that opens a connection is taken for opaque TCP and dropped: curl gets no reply.
status=0
curl -sS --max-time 20 -o /dev/null -X PROPFIND -H 'Depth: 0' "${url}/" 2>"${work}/bare.err" || status=$?
[[ "${status}" == 52 ]] ||
  fail "a PROPFIND that opens a connection now gets a reply (curl ${status}): the gateway may pass WebDAV methods, so revisit the helper"

# The run message's helper, pointed at this gateway in place of the cluster's. The helper reads S.
eval "${helper}"
# shellcheck disable=SC2034
S="${url}"
# reply <name> <dav arguments...>: runs the helper, keeps its output as <name>, prints the status.
reply() {
  local name="$1"
  shift
  dav "$@" | tr -d '\r' >"${work}/${name}" || fail "the helper failed for $*: $(cat "${work}/${name}")"
  sed -n '1s/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "${work}/${name}"
}
# header <name> <header>: one header of a kept reply.
header() { awk -v h="$2" 'BEGIN {h = tolower(h) ":"} tolower(substr($0, 1, length(h))) == h {
  sub(/^[^:]*:[[:space:]]*/, ""); print; exit}' "${work}/$1"; }
owner='agentic-engineer 2026-11-11T05:15:00Z 0123456789abcdef'

[[ "$(reply missing PROPFIND memory/ -H 'Depth: 0')" == 404 ]] || fail "PROPFIND of a missing memory/ did not answer 404: $(head -c 300 "${work}/missing")"
[[ "$(reply created MKCOL memory/)" == 201 ]] || fail "MKCOL did not create memory/: $(head -c 300 "${work}/created")"
[[ "$(reply listed PROPFIND memory/ -H 'Depth: 1')" == 207 ]] || fail "PROPFIND did not list memory/: $(head -c 300 "${work}/listed")"
grep -q 'multistatus' "${work}/listed" || fail 'the listing has no multistatus body'
[[ "$(reply locked LOCK memory/ -H 'Timeout: Infinite' -H 'Content-Type: application/xml' --data-binary "${lockinfo/OWNER/${owner}}")" == 200 ]] ||
  fail "LOCK did not take the lease: $(head -c 300 "${work}/locked")"
token="$(header locked Lock-Token)"
[[ "${token}" =~ ^\<[0-9]+\>$ ]] || fail "the lease reply carried no Lock-Token: ${token}"
grep -Fq ">${owner}<" "${work}/locked" || fail 'the lease reply does not name its holder'
[[ "$(reply refreshed LOCK memory/ -H 'Timeout: Infinite' -H "If: (${token})")" == 200 ]] || fail 'the refresh did not hold the lease'
grep -Fq ">${owner}<" "${work}/refreshed" || fail 'the refresh does not name the holder'
put="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT -H "${A}" -H "If: (${token})" --data-binary memory "${url}/memory/MEMORY.md")"
[[ "${put}" == 201 ]] || fail "a PUT with the lease's token did not land: ${put}"
[[ "$(reply folder MKCOL memory/archive/ -H "If: (${token})")" == 201 ]] || fail 'MKCOL with the lease token did not create a folder'
[[ "$(reply released UNLOCK memory/ -H "Lock-Token: ${token}")" == 204 ]] || fail "UNLOCK did not release the lease: $(head -c 300 "${work}/released")"

grep -Fq 'bash scripts/tests/test-agentic-engineer-lane-store-gateway.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer lane store gateway: PASS\n'
