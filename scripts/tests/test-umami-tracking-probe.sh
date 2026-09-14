#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly probe="${root_dir}/scripts/verify-umami-tracking.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${probe}" ]] || fail 'the executable Umami tracking probe must be present'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
trap 'rm -rf "${tmp_dir}"' EXIT

mkdir -p "${tmp_dir}/bin"
cp "${root_dir}/scripts/tests/fixtures/umami-curl-stub.sh" "${tmp_dir}/bin/curl"
chmod +x "${tmp_dir}/bin/curl"

readonly website_id='2f8d150e-c6f0-4a90-ab77-431c9ef9dc59'
readonly session_id='ab0fbc94-de33-51c4-9347-f74f81120104'
readonly visit_id='9bb30335-586a-5708-8cfb-51ce0153c2d5'
readonly test_path='/__verification/platform-3134-unit'

success_output="$({
  PATH="${tmp_dir}/bin:${PATH}" \
    UMAMI_CURL_PAYLOAD_FILE="${tmp_dir}/payload.json" \
    UMAMI_CURL_RESPONSE="{\"cache\":\"signed-cache\",\"sessionId\":\"${session_id}\",\"visitId\":\"${visit_id}\"}" \
    bash "${probe}" \
      --website-id "${website_id}" \
      --hostname devantler.tech \
      --path "${test_path}"
})" || fail 'the probe must accept Umami recording confirmation identifiers'

[[ "${success_output}" == "PASS: Umami recorded synthetic pageview session=${session_id} visit=${visit_id}" ]] ||
  fail 'the probe must report the confirmed Umami session and visit identifiers'

jq -e \
  --arg website "${website_id}" \
  --arg path "${test_path}" \
  '.type == "event" and .payload.website == $website and .payload.hostname == "devantler.tech" and .payload.url == $path' \
  "${tmp_dir}/payload.json" >/dev/null ||
  fail 'the probe must send a correctly scoped Umami pageview payload'

if PATH="${tmp_dir}/bin:${PATH}" \
  UMAMI_CURL_PAYLOAD_FILE="${tmp_dir}/discarded-payload.json" \
  UMAMI_CURL_RESPONSE='{"beep":"boop"}' \
  bash "${probe}" --website-id "${website_id}" --hostname devantler.tech --path "${test_path}" \
  >"${tmp_dir}/discarded.out" 2>"${tmp_dir}/discarded.err"; then
  fail 'the probe must reject Umami bot-discard responses'
fi

grep -Fq 'the collector response did not confirm a recorded event' "${tmp_dir}/discarded.err" ||
  fail 'the probe must explain when Umami did not record the event'

if bash "${probe}" --website-id >"${tmp_dir}/missing.out" 2>"${tmp_dir}/missing.err"; then
  fail 'the probe must reject a missing option value'
fi
grep -Fq -- '--website-id requires a value' "${tmp_dir}/missing.err" ||
  fail 'the probe must explain a missing website-id value'

if bash "${probe}" --website-id '------------------------------------' \
  >"${tmp_dir}/malformed.out" 2>"${tmp_dir}/malformed.err"; then
  fail 'the probe must reject a malformed UUID'
fi
grep -Fq -- '--website-id must be a UUID' "${tmp_dir}/malformed.err" ||
  fail 'the probe must explain a malformed website ID'

printf 'PASS: Umami tracking probe distinguishes recorded events from bot discards\n'
