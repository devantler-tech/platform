#!/usr/bin/env bash

set -euo pipefail

endpoint='https://analytics.platform.devantler.tech/api/send'
hostname='devantler.tech'
event_path="/__verification/umami-$(date -u '+%Y%m%dT%H%M%SZ')"
website_id=''

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

usage() {
  printf 'Usage: %s --website-id UUID [--endpoint URL] [--hostname HOST] [--path PATH]\n' "$0" >&2
}

while (($# > 0)); do
  case "$1" in
    --website-id)
      (($# >= 2)) && [[ -n "${2:-}" ]] || {
        usage
        fail '--website-id requires a value'
      }
      website_id="$2"
      shift 2
      ;;
    --endpoint)
      (($# >= 2)) && [[ -n "${2:-}" ]] || {
        usage
        fail '--endpoint requires a value'
      }
      endpoint="$2"
      shift 2
      ;;
    --hostname)
      (($# >= 2)) && [[ -n "${2:-}" ]] || {
        usage
        fail '--hostname requires a value'
      }
      hostname="$2"
      shift 2
      ;;
    --path)
      (($# >= 2)) && [[ -n "${2:-}" ]] || {
        usage
        fail '--path requires a value'
      }
      event_path="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage
      fail "unknown argument: $1"
      ;;
  esac
done

[[ "${website_id}" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || {
  usage
  fail '--website-id must be a UUID'
}
[[ -n "${hostname}" ]] || fail '--hostname must not be empty'
[[ "${event_path}" == /* ]] || fail '--path must start with /'

payload="$(
  jq -cn \
    --arg website "${website_id}" \
    --arg hostname "${hostname}" \
    --arg path "${event_path}" \
    '{
      type: "event",
      payload: {
        website: $website,
        hostname: $hostname,
        screen: "1280x720",
        language: "en-US",
        title: "Platform Umami end-to-end verification",
        url: $path,
        referrer: ""
      }
    }'
)" || fail 'could not build the Umami pageview payload'

# Umami 3.2.0 awaits saveEvent before returning its signed cache plus session
# and visit IDs. Bot-filtered requests instead return {"beep":"boop"} with 200,
# so the response body—not HTTP success alone—is the persistence assertion.
response="$(
  curl --fail --silent --show-error \
    -H 'Content-Type: application/json' \
    -H "Origin: https://${hostname}" \
    -H "Referer: https://${hostname}/" \
    -H 'User-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36' \
    --data-binary "${payload}" \
    "${endpoint}"
)" || fail 'the Umami collector request failed'

session_id="$(jq -er '.sessionId | select(type == "string" and length > 0)' <<<"${response}" 2>/dev/null)" ||
  fail 'the collector response did not confirm a recorded event'
visit_id="$(jq -er '.visitId | select(type == "string" and length > 0)' <<<"${response}" 2>/dev/null)" ||
  fail 'the collector response did not confirm a recorded event'
jq -e '.cache | select(type == "string" and length > 0)' <<<"${response}" >/dev/null 2>&1 ||
  fail 'the collector response did not confirm a recorded event'

[[ "${session_id}" =~ ^[0-9a-fA-F-]{36}$ && "${visit_id}" =~ ^[0-9a-fA-F-]{36}$ ]] ||
  fail 'the collector response returned malformed recording identifiers'

printf 'PASS: Umami recorded synthetic pageview session=%s visit=%s\n' "${session_id}" "${visit_id}"
