#!/usr/bin/env bash
# Prove the kagent lane's single-writer lease holds on the store the lane runs against.
#
# kagent starts each day's run whether or not the last one has finished, and a run has no time
# limit, so two runs can overlap (devantler-tech/monorepo#4910). Before a run touches its memory it
# takes an exclusive WebDAV LOCK on the lane store's lease, refreshes it before it uploads, and
# releases it afterwards (job-schedule.yaml). This starts the store's own pinned rclone image with
# the Deployment's own arguments, minus the credential the egress gateway adds, and proves on it:
# - a held lease refuses every other run, including many at once;
# - the lease never blocks the holder's own uploads;
# - a refresh succeeds only with the holder's token and names the holder;
# - only the holder's token releases it;
# - a store restart frees it and can hand an earlier run's token to a later run's lease, so only a
#   refresh that names the run as the holder shows the lease is still its own.
# It also shows why the lease is not a MKCOL: the store accepts a MKCOL of a folder that exists.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="${root}/k8s/providers/hetzner/apps/agentic-engineer"
deployment="${dir}/deployment-lane-store.yaml"
work="$(mktemp -d)"
container=''
completed=0
# Remove the store and the work directory on exit, and fail any exit that did not reach the PASS.
cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm -f "${container}" >/dev/null 2>&1 || true
  fi
  rm -rf "${work}"
  if [[ "${completed}" != 1 ]]; then
    printf 'FAIL: agentic-engineer lane store lease: aborted before completion\n' >&2
    exit 1
  fi
}
trap cleanup EXIT
# Print one failure reason and stop.
fail() {
  printf 'FAIL: agentic-engineer lane store lease: %s\n' "$1" >&2
  exit 1
}
for tool in curl docker yq; do
  command -v "${tool}" >/dev/null || fail "${tool} is required"
done

# The run message takes the lease before it reads memory, refreshes it before it uploads, and
# releases it last. The body this test locks with is the one the run message gives.
prompt="$(yq '.spec.template.spec.containers[] | select(.name == "schedule") | .env[] |
  select(.name == "PROMPT") | .value' "${dir}/job-schedule.yaml")"
# at <text>: how many characters of the run message come before text, or -1 when it is missing.
at() {
  local before="${prompt%%"$1"*}"
  if [[ "${before}" == "${prompt}" ]]; then printf '%s' -1; else printf '%s' "${#before}"; fi
}
previous=-1
for step in 'Send a WebDAV LOCK to lease' 'PROPFIND' 'refresh the lease' 'upload ~/lane-memory/' \
  'UNLOCK to lease'; do
  position="$(at "${step}")"
  [[ "${position}" -gt "${previous}" ]] || fail "the run message does not say, in order: ${step}"
  previous="${position}"
done
for rule in 'Timeout: Infinite' 'A 423 means another run holds it' 'Never remove a lease you did not take' \
  'its owner is yours' 'leave the lease alone'; do
  [[ "$(at "${rule}")" -ge 0 ]] || fail "the run message no longer says: ${rule}"
done
pattern='(<[?]xml [^`]*</D:lockinfo>)'
[[ "${prompt}" =~ ${pattern} ]] || fail 'the run message gives no lock body'
lockinfo="${BASH_REMATCH[1]}"
for part in '<D:exclusive/>' '<D:write/>' '<D:owner>OWNER</D:owner>'; do
  [[ "${lockinfo}" == *"${part}"* ]] || fail "the run message's lock body has no ${part}: ${lockinfo}"
done

# The store's image and arguments, without the credential: the egress gateway adds that, and it
# has no part in the lease.
image="$(yq '.spec.template.spec.containers[] | select(.name == "webdav") | .image' "${deployment}")"
[[ "${image}" =~ ^docker\.io/rclone/rclone:[^@]+@sha256:[0-9a-f]{64}$ ]] ||
  fail "the store does not run a digest-pinned rclone: ${image}"
args=()
dropped=0
while IFS= read -r arg; do
  case "${arg}" in
    --htpasswd=*) dropped=$((dropped + 1)) ;;
    *) args+=("${arg}") ;;
  esac
done < <(yq '.spec.template.spec.containers[] | select(.name == "webdav") | .args[]' "${deployment}")
[[ "${dropped}" == 1 ]] || fail "the store has ${dropped} --htpasswd arguments, not one"
[[ "${args[*]:0:4}" == 'serve webdav /data --addr=:8080' ]] ||
  fail "the store no longer serves WebDAV from /data on :8080: ${args[*]}"

mkdir -p "${work}/data"
container="lane-store-lease-$$-${RANDOM}"
docker run -d --name "${container}" --user "$(id -u):$(id -g)" --read-only --tmpfs /tmp \
  --cap-drop ALL --security-opt no-new-privileges \
  --mount "type=bind,src=${work}/data,dst=/data" \
  -p 127.0.0.1::8080 "${image}" "${args[@]}" >/dev/null
url=''
# connect: waits until the store answers, then points url at it. A restart can move its port.
connect() {
  local ports address attempt
  for attempt in $(seq 1 30); do
    ports="$(docker port "${container}" 8080/tcp 2>/dev/null || true)"
    address="${ports%%$'\n'*}"
    if [[ -n "${address}" ]] &&
      curl -fsS -o /dev/null -X PROPFIND -H 'Depth: 0' "http://${address}/" 2>/dev/null; then
      url="http://${address}"
      return 0
    fi
    sleep 1
  done
  fail "the store did not answer after ${attempt} tries: $(docker logs "${container}" 2>&1 | tail -c 600)"
}
connect

# dav <name> <method> <path> [curl arguments...]: sends one request, keeps its headers and body
# as <name>.headers and <name>.body, and prints its status.
dav() {
  local name="$1" method="$2" path="$3"
  shift 3
  curl -sS -o "${work}/${name}.body" -D "${work}/${name}.headers" -w '%{http_code}' \
    -X "${method}" "$@" "${url}${path}"
}
# lock <name> <owner>: takes the lease with the run message's body, as owner.
lock() {
  dav "$1" LOCK /lease -H 'Timeout: Infinite' -H 'Content-Type: application/xml' \
    --data-binary "${lockinfo/OWNER/$2}"
}
# refresh <name> <token>: refreshes the lease as the run message does, with no body.
refresh() { dav "$1" LOCK /lease -H 'Timeout: Infinite' -H "If: ($2)"; }
# unlock <name> <token>: releases the lease.
unlock() { dav "$1" UNLOCK /lease -H "Lock-Token: $2"; }
# token <name>: the Lock-Token a reply carried, angle brackets included, or nothing.
token() {
  local value
  value="$(tr -d '\r' <"${work}/$1.headers" | sed -n 's/^[Ll][Oo][Cc][Kk]-[Tt][Oo][Kk][Ee][Nn]:[[:space:]]*//p')"
  if [[ "${value}" =~ ^\<[^\<\>]+\>$ ]]; then printf '%s' "${value}"; fi
}
# names <name> <owner>: whether a lock reply names owner as the lease's holder.
names() { grep -Fq ">$2<" "${work}/$1.body"; }
# body <name>: the start of a reply's body, for a failure message.
body() { tr -d '\r\n' <"${work}/$1.body" | head -c 400; }
# owner <n>: a run's owner in the shape the run message gives.
owner() { printf 'agentic-engineer 2026-11-11T05:15:%02dZ %016x' "$(($1 % 60))" "$1"; }

# A MKCOL is no lease: the store answers 201 for a folder that already exists.
[[ "$(dav mkcol-new MKCOL /memory)" == 201 ]] || fail "the store refused a MKCOL: $(body mkcol-new)"
status="$(dav mkcol-again MKCOL /memory)"
[[ "${status}" == 201 ]] ||
  fail "the store now refuses a MKCOL of an existing folder (${status}): it could serve as a lease, so revisit why the lease is a LOCK"

# A first run takes the lease, and the reply names it as the holder.
status="$(lock first "$(owner 1)")"
[[ "${status}" == 201 ]] || fail "the first run did not take the lease: ${status} $(body first)"
first="$(token first)"
[[ -n "${first}" ]] || fail 'the lease reply carried no Lock-Token'
names first "$(owner 1)" || fail "the lease reply does not name its holder: $(body first)"

# While it holds the lease, another run is refused, and the holder's own uploads still land.
status="$(lock second "$(owner 2)")"
[[ "${status}" == 423 ]] || fail "a second run was not refused the held lease: ${status} $(body second)"
status="$(dav upload PUT /memory/MEMORY.md --data-binary 'memory')"
[[ "${status}" == 201 ]] || fail "the lease blocked an upload to memory/: ${status} $(body upload)"

# Only the holder's token refreshes the lease, and the refresh names the holder.
status="$(refresh refreshed "${first}")"
[[ "${status}" == 200 ]] || fail "the holder could not refresh the lease: ${status} $(body refreshed)"
names refreshed "$(owner 1)" || fail "the refresh does not name the holder: $(body refreshed)"
status="$(refresh forged '<opaquelocktoken:not-a-lease>')"
[[ "${status}" != 200 ]] || fail 'a refresh with a token nobody holds succeeded'

# Only the holder's token releases it.
status="$(unlock forged-unlock '<opaquelocktoken:not-a-lease>')"
[[ "${status}" != 204 ]] || fail 'an UNLOCK with a token nobody holds succeeded'
[[ "$(lock still-held "$(owner 3)")" == 423 ]] || fail 'an UNLOCK with a wrong token released the lease'
status="$(unlock released "${first}")"
[[ "${status}" == 204 ]] || fail "the holder could not release the lease: ${status} $(body released)"

# Of many runs that try at once, exactly one takes the lease.
racers=20
for racer in $(seq 1 "${racers}"); do
  (lock "race-${racer}" "$(owner "$((100 + racer))")" >"${work}/race-${racer}.status" || true) &
done
wait
winners=()
refused=0
for racer in $(seq 1 "${racers}"); do
  case "$(cat "${work}/race-${racer}.status")" in
    200 | 201) winners+=("${racer}") ;;
    423) refused=$((refused + 1)) ;;
    *) fail "racer ${racer} got neither the lease nor a 423: $(cat "${work}/race-${racer}.status")" ;;
  esac
done
[[ "${#winners[@]}" == 1 && "${refused}" == $((racers - 1)) ]] ||
  fail "${#winners[@]} of ${racers} simultaneous runs took the lease, not exactly one"
winner="$(token "race-${winners[0]}")"
names "race-${winners[0]}" "$(owner "$((100 + winners[0]))")" || fail 'the winning reply does not name the winner'
[[ "$(unlock race-released "${winner}")" == 204 ]] || fail 'the winner could not release the lease'

# A store restart frees every lease, and can hand a token a run took before it to a later run's
# lease: the store numbers its tokens up from the clock at its start, so a burst of writes after a
# start runs the count ahead of the clock, and the restarted store counts through the same
# numbers again. This builds exactly that case. The earlier run's refresh then succeeds on the
# later run's lease, and only the owner shows that the lease is no longer its own.
docker restart -t 5 "${container}" >/dev/null
connect
for burst in $(seq 1 200); do
  [[ "$(lock burst "$(owner 7)")" == 200 ]] || fail "burst ${burst} could not take the lease: $(body burst)"
  [[ "$(unlock burst-released "$(token burst)")" == 204 ]] || fail "burst ${burst} could not release the lease"
done
[[ "$(lock before-restart "$(owner 4)")" == 200 ]] || fail 'a run could not take the lease before the restart'
before="$(token before-restart)"
[[ "${before}" =~ ^\<[0-9]+\>$ ]] ||
  fail "the store's lock tokens are no longer numbers (${before}): revisit whether a restart can reissue one"
docker restart -t 5 "${container}" >/dev/null
connect
# Later runs take and release the lease until the store hands out the earlier run's token.
after=''
for attempt in $(seq 1 400); do
  status="$(lock after-restart "$(owner 5)")"
  [[ "${status}" == 200 ]] || fail "the restart did not free the lease: ${status} $(body after-restart)"
  after="$(token after-restart)"
  [[ "${after}" =~ ^\<[0-9]+\>$ ]] || fail "the restarted store's token is not a number: ${after}"
  reached="${after//[<>]/}"
  target="${before//[<>]/}"
  if ((reached >= target)); then
    break
  fi
  [[ "$(unlock after-released "${after}")" == 204 ]] || fail 'a later run could not release the lease'
done
[[ "${after}" == "${before}" ]] ||
  fail "the restarted store did not reissue the earlier token ${before} (reached ${after} after ${attempt} leases)"
status="$(refresh stale "${before}")"
[[ "${status}" == 200 ]] ||
  fail "the earlier run's token no longer refreshes the later run's lease (${status}), so this no longer shows why the owner matters"
if names stale "$(owner 4)" || ! names stale "$(owner 5)"; then
  fail "the refresh on a reissued token does not name the later run as the holder: $(body stale)"
fi
[[ "$(lock after-stale "$(owner 6)")" == 423 ]] || fail "the earlier run's refresh released the later run's lease"

grep -Fq 'bash scripts/tests/test-agentic-engineer-lane-store-lease.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer lane store lease: PASS\n'
