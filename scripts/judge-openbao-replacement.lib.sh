#!/usr/bin/env bash
# Shared by both steps of the Replace OpenBao Server workflow (#4970): one
# fresh capture of every server, judged by the health and quorum judges for
# the server being replaced. Sourced, not executed.
#
# Reads OPENBAO_SERVERS, OPENBAO_VERSION, PREVIOUS_VERSION and
# REPLACEMENT_SERVER from the environment, and the built tools from
# OPENBAO_BIN (default "$RUNNER_TEMP/bin"). Prints only the tools' own closed
# status lines; the capture stays in the caller's private directory.

# judge_openbao_replacement <private-dir> <servers-on-the-new-release>
# Returns 0 PROVEN, 1 HOLD or 2 UNKNOWN; UNKNOWN outranks HOLD.
judge_openbao_replacement() {
  local private="$1" current="$2"
  local bin="${OPENBAO_BIN:-$RUNNER_TEMP/bin}"
  local health=0 quorum=0 status=0 code
  # Without a declared upgrade every server must already run the reviewed
  # release, so the list of servers on it adds nothing to judge.
  local rolling=()
  if [[ -n "$PREVIOUS_VERSION" ]]; then
    rolling=(--previous-version "$PREVIOUS_VERSION")
    if [[ -n "$current" ]]; then rolling+=(--require-current "$current"); fi
  fi
  # A failed capture writes nothing, so clear the previous one first: an old
  # file must never be judged as this capture.
  rm -f "$private/health.json" "$private/quorum.json"
  "$bin/capture-openbao-replacement" --expect-servers "$OPENBAO_SERVERS" --out "$private" || return 2
  "$bin/judge-openbao-health" --input "$private/health.json" \
    --expect-servers "$OPENBAO_SERVERS" --expect-version "$OPENBAO_VERSION" ${rolling[@]+"${rolling[@]}"} --max-age 5m || health=$?
  "$bin/judge-openbao-quorum" --input "$private/quorum.json" \
    --expect-servers "$OPENBAO_SERVERS" --replace "$REPLACEMENT_SERVER" --max-age 5m || quorum=$?
  for code in "$health" "$quorum"; do
    if ((code > 2)); then code=2; fi
    if ((code > status)); then status=$code; fi
  done
  return "$status"
}

# openbao_verdict <status> — the word for a judge_openbao_replacement status.
openbao_verdict() {
  case "$1" in
    0) echo PROVEN ;;
    1) echo HOLD ;;
    *) echo UNKNOWN ;;
  esac
}

# openbao_pod_uid <private-dir> <server> — the pod UID the last capture bound
# that server's evidence to. Never printed by the workflow; it is passed to
# the replacement step as its delete precondition.
openbao_pod_uid() {
  jq -er --arg name "$2" '[.servers[] | select(.name == $name) | .podUID] | if length == 1 then .[0] else error("not exactly one") end' "$1/health.json"
}
