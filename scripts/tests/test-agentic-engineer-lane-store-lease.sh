#!/usr/bin/env bash
# Prove the kagent lane's single-writer lease holds on the store the lane runs against.
#
# kagent starts each day's run whether or not the last one has finished, and a run has no time
# limit, so two runs can overlap (devantler-tech/monorepo#4910). Before a run touches its memory it
# takes an exclusive WebDAV LOCK on the lane store's memory/, writes there only with that lock's
# token, and releases it last (job-schedule.yaml). This starts the store's own pinned rclone image
# with the Deployment's own arguments, minus the credential the egress gateway adds, and proves:
# - a held lease refuses every other run, including many at once;
# - the store fences memory/: it refuses a write there without the lease's token, and lets the
#   holder's writes, and every write outside memory/, through;
# - a refresh succeeds only with the holder's token and names the holder;
# - only the holder's token releases the lease;
# - a store restart frees the lease and refuses the former holder's writes and release;
# - the store numbers its tokens up from the second it started, so a token at least 60 below a
#   reply's Date is never handed out again after a restart. A token above it can be, and an
#   UNLOCK of it could then release a later run's lease: that is why a run that takes the lease
#   on a fresh store waits until its token is safe, and never releases one that is not.
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

# The run message checks for memory/, takes the lease, checks its token against the clock,
# downloads, refreshes, uploads one file at a time with the token, and releases the lease last.
# The body this test locks with is the one the run message gives.
prompt="$(yq '.spec.template.spec.containers[] | select(.name == "schedule") | .env[] |
  select(.name == "PROMPT") | .value' "${dir}/job-schedule.yaml")"
# at <text>: how many characters of the run message come before text, or -1 when it is missing.
at() {
  local before="${prompt%%"$1"*}"
  if [[ "${before}" == "${prompt}" ]]; then printf '%s' -1; else printf '%s' "${#before}"; fi
}
previous=-1
for step in 'Send a PROPFIND to memory/' 'Send a LOCK to memory/' 'at least 60 below a reply' \
  'download every file' 'however the run ended, refresh the lease' 'one file at a time' 'UNLOCK to memory/'; do
  position="$(at "${step}")"
  [[ "${position}" -gt "${previous}" ]] || fail "the run message does not say, in order: ${step}"
  previous="${position}"
done
for rule in 'Timeout: Infinite' 'A 423 means another run holds it' 'Never remove a lease you did not take' \
  'its owner is yours' 'If header on every PUT' 'on the MKCOL of any folder inside memory/' \
  'each folder it shows the same way' 'leave the lease alone' 'A 412' \
  'never UNLOCK it' 'wait out the difference'; do
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
# restart: restarts the store, which drops every lease, and waits for it.
restart() {
  docker restart -t 5 "${container}" >/dev/null
  connect
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
# lock <name> <owner>: takes the lease on memory/ with the run message's body, as owner.
lock() {
  dav "$1" LOCK /memory/ -H 'Timeout: Infinite' -H 'Content-Type: application/xml' \
    --data-binary "${lockinfo/OWNER/$2}"
}
# refresh <name> <token>: refreshes the lease as the run message does, with no body.
refresh() { dav "$1" LOCK /memory/ -H 'Timeout: Infinite' -H "If: ($2)"; }
# unlock <name> <token>: releases the lease.
unlock() { dav "$1" UNLOCK /memory/ -H "Lock-Token: $2"; }
# put <name> <path> [token]: writes a file, with the lease's token when one is given.
put() {
  if [[ -n "${3:-}" ]]; then
    dav "$1" PUT "$2" --data-binary "$1" -H "If: ($3)"
  else
    dav "$1" PUT "$2" --data-binary "$1"
  fi
}
# header <name> <header>: one header of a reply, or nothing.
header() {
  tr -d '\r' <"${work}/$1.headers" | awk -v h="$2" 'BEGIN {h = tolower(h) ":"}
    tolower(substr($0, 1, length(h))) == h {sub(/^[^:]*:[[:space:]]*/, ""); print; exit}'
}
# token <name>: the Lock-Token a reply carried, angle brackets included, or nothing.
token() {
  local value
  value="$(header "$1" Lock-Token)"
  if [[ "${value}" =~ ^\<[0-9]+\>$ ]]; then printf '%s' "${value}"; fi
}
# number <token>: the number inside a token.
number() { printf '%s' "${1//[<>]/}"; }
# clock <name>: a reply's Date header in Unix seconds.
clock() {
  local value
  value="$(header "$1" Date)"
  date -u -d "${value}" +%s 2>/dev/null || date -u -j -f '%a, %d %b %Y %H:%M:%S GMT' "${value}" +%s
}
# names <name> <owner>: whether a lock reply names owner as the lease's holder.
names() { grep -Fq ">$2<" "${work}/$1.body"; }
# body <name>: the start of a reply's body, for a failure message.
body() { tr -d '\r\n' <"${work}/$1.body" | head -c 400; }
# owner <n>: a run's owner in the shape the run message gives.
owner() { printf 'agentic-engineer 2026-11-11T05:15:%02dZ %016x' "$(($1 % 60))" "$1"; }

# A MKCOL is no lease: the store answers 201 for a folder that already exists.
[[ "$(dav mkcol-new MKCOL /runs/)" == 201 ]] || fail "the store refused a MKCOL: $(body mkcol-new)"
status="$(dav mkcol-again MKCOL /runs/)"
[[ "${status}" == 201 ]] ||
  fail "the store now refuses a MKCOL of an existing folder (${status}): it could serve as a lease, so revisit why the lease is a LOCK"

# A first run finds no memory/, creates it, and takes the lease, which names it as the holder.
[[ "$(dav first-check PROPFIND /memory/ -H 'Depth: 0')" == 404 ]] || fail 'a fresh store already has memory/'
[[ "$(dav first-mkcol MKCOL /memory/)" == 201 ]] || fail "a first run could not create memory/: $(body first-mkcol)"
status="$(lock first "$(owner 1)")"
[[ "${status}" == 200 ]] || fail "the first run did not take the lease: ${status} $(body first)"
first="$(token first)"
[[ -n "${first}" ]] || fail "the lease reply carried no numbered Lock-Token: $(header first Lock-Token)"
names first "$(owner 1)" || fail "the lease reply does not name its holder: $(body first)"
[[ "$(dav first-list PROPFIND /memory/ -H 'Depth: 1')" == 207 ]] || fail 'the lease blocked a listing of memory/'

# While it holds the lease, another run is refused it.
status="$(lock second "$(owner 2)")"
[[ "${status}" == 423 ]] || fail "a second run was not refused the held lease: ${status} $(body second)"

# The store fences memory/: the holder's writes with its token land, a write without it or with
# another token is refused, and writes outside memory/ are not held up.
status="$(put fenced /memory/MEMORY.md "${first}")"
[[ "${status}" == 201 ]] || fail "the holder could not write memory/ with its token: ${status} $(body fenced)"
status="$(put unfenced /memory/topic.md)"
[[ "${status}" == 423 ]] || fail "the store let a write without the lease's token into memory/: ${status}"
status="$(put forged-write /memory/topic.md '<999>')"
[[ "${status}" == 412 ]] || fail "the store let a write with another token into memory/: ${status}"
# A folder inside memory/ is a write too: its MKCOL needs the token, and a file in it then lands.
status="$(dav unfenced-folder MKCOL /memory/archive/)"
[[ "${status}" == 423 ]] || fail "the store let a MKCOL without the lease's token into memory/: ${status}"
status="$(dav fenced-folder MKCOL /memory/archive/ -H "If: (${first})")"
[[ "${status}" == 201 ]] || fail "the holder could not create a folder in memory/ with its token: ${status} $(body fenced-folder)"
status="$(put fenced-nested /memory/archive/old.md "${first}")"
[[ "${status}" == 201 ]] || fail "the holder could not write inside a folder of memory/: ${status} $(body fenced-nested)"
status="$(put transcript /runs/transcript.jsonl)"
[[ "${status}" == 201 ]] || fail "the lease held up a write outside memory/: ${status} $(body transcript)"

# Only the holder's token refreshes the lease, and the refresh names the holder.
status="$(refresh refreshed "${first}")"
[[ "${status}" == 200 ]] || fail "the holder could not refresh the lease: ${status} $(body refreshed)"
names refreshed "$(owner 1)" || fail "the refresh does not name the holder: $(body refreshed)"
status="$(refresh forged '<999>')"
[[ "${status}" != 200 ]] || fail 'a refresh with a token nobody holds succeeded'

# Only the holder's token releases it, and a release opens memory/ again.
status="$(unlock forged-unlock '<999>')"
[[ "${status}" != 204 ]] || fail 'an UNLOCK with a token nobody holds succeeded'
[[ "$(lock still-held "$(owner 3)")" == 423 ]] || fail 'an UNLOCK with a wrong token released the lease'
status="$(unlock released "${first}")"
[[ "${status}" == 204 ]] || fail "the holder could not release the lease: ${status} $(body released)"
[[ "$(put after-release /memory/topic.md)" == 201 ]] || fail 'memory/ stayed fenced after the release'

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
    200) winners+=("${racer}") ;;
    423) refused=$((refused + 1)) ;;
    *) fail "racer ${racer} got neither the lease nor a 423: $(cat "${work}/race-${racer}.status")" ;;
  esac
done
[[ "${#winners[@]}" == 1 && "${refused}" == $((racers - 1)) ]] ||
  fail "${#winners[@]} of ${racers} simultaneous runs took the lease, not exactly one"
winner="$(token "race-${winners[0]}")"
names "race-${winners[0]}" "$(owner "$((100 + winners[0]))")" || fail 'the winning reply does not name the winner'
[[ "$(unlock race-released "${winner}")" == 204 ]] || fail 'the winner could not release the lease'

# A run that takes the lease moments after the store started holds a token that is not yet safe.
# As the run message says, it never releases that token: it waits out the difference and
# refreshes, and the token is then at least 60 below the Date. A safe token outlives no restart
# and is never handed out again: the restart frees the lease, the store refuses the former
# holder's writes and release, and the next run's lease stands.
restart
[[ "$(lock fresh "$(owner 4)")" == 200 ]] || fail 'a run could not take the lease on a fresh store'
held="$(token fresh)"
wait_for=$(($(number "${held}") + 60 - $(clock fresh)))
if ((wait_for > 0)); then
  sleep "$((wait_for + 1))"
fi
status="$(refresh settled "${held}")"
if [[ "${status}" != 200 ]] || ! names settled "$(owner 4)"; then
  fail "the run's refresh after waiting did not hold the lease: ${status} $(body settled)"
fi
(($(number "${held}") <= $(clock settled) - 60)) ||
  fail "after the wait, the token ${held} is still not 60 below the Date $(clock settled)"
restart
status="$(put stale-write /memory/MEMORY.md "${held}")"
[[ "${status}" == 412 ]] || fail "after a restart, the store took the former holder's write: ${status}"
[[ "$(lock next "$(owner 5)")" == 200 ]] || fail 'the restart did not free the lease'
next="$(token next)"
(($(number "${next}") > $(number "${held}"))) ||
  fail "the restarted store handed out ${next}, not above the earlier ${held}"
[[ "$(put stale-write-again /memory/MEMORY.md "${held}")" == 412 ]] ||
  fail "the store took the former holder's write under the next run's lease"
[[ "$(unlock stale-unlock "${held}")" != 204 ]] || fail "the former holder's UNLOCK succeeded after a restart"
[[ "$(lock still-next "$(owner 6)")" == 423 ]] || fail "the former holder's UNLOCK released the next run's lease"
[[ "$(unlock next-released "${next}")" == 204 ]] || fail 'the next run could not release the lease'

# Why a token that is not yet safe is never used or released: a burst of writes after a start runs
# the store's count ahead of the clock, and a restarted store then counts through the same numbers
# again. The later run's lease gets the earlier token, the earlier run's writes pass the fence
# under it, only the owner in a refresh still tells the two apart, and the earlier run's UNLOCK
# would release the later run's lease.
restart
for burst in $(seq 1 200); do
  [[ "$(lock burst "$(owner 7)")" == 200 ]] || fail "burst ${burst} could not take the lease: $(body burst)"
  [[ "$(unlock burst-released "$(token burst)")" == 204 ]] || fail "burst ${burst} could not release the lease"
done
[[ "$(lock ahead "$(owner 8)")" == 200 ]] || fail 'a run could not take the lease after the burst'
ahead="$(token ahead)"
(($(number "${ahead}") > $(clock ahead) - 60)) ||
  fail "after the burst the token ${ahead} is still 60 below the Date $(clock ahead), so this case shows nothing"
restart
reached=''
for attempt in $(seq 1 400); do
  [[ "$(lock reissued "$(owner 9)")" == 200 ]] || fail "the restart did not free the lease: $(body reissued)"
  reached="$(token reissued)"
  if (($(number "${reached}") >= $(number "${ahead}"))); then
    break
  fi
  [[ "$(unlock reissued-released "${reached}")" == 204 ]] || fail 'a later run could not release the lease'
done
[[ "${reached}" == "${ahead}" ]] ||
  fail "the restarted store did not reissue ${ahead} (reached ${reached} after ${attempt} leases), so the Date check guards nothing"
[[ "$(put reused-write /memory/MEMORY.md "${ahead}")" == 201 ]] ||
  fail "the earlier run's reissued token no longer passes the fence, so this no longer shows why the Date check matters"
status="$(refresh reused "${ahead}")"
if [[ "${status}" != 200 ]] || ! names reused "$(owner 9)" || names reused "$(owner 8)"; then
  fail "a refresh on a reissued token does not name the later run: ${status} $(body reused)"
fi
[[ "$(unlock reused-unlock "${ahead}")" == 204 ]] ||
  fail "the earlier run's UNLOCK no longer releases a lease that reissued its token, so this no longer shows why the run never releases a token that is not yet safe"
[[ "$(lock taken-over "$(owner 10)")" == 200 ]] ||
  fail "a third run could not take the lease the earlier run's UNLOCK released"

grep -Fq 'bash scripts/tests/test-agentic-engineer-lane-store-lease.sh' "${root}/.github/workflows/ci.yaml" ||
  fail 'CI does not run this test'

completed=1
printf 'agentic-engineer lane store lease: PASS\n'
