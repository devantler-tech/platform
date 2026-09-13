#!/usr/bin/env bash
# Measures whether the gateway's Gateway API ExternalAuth subrequest reaches the
# auth backend (platform#3784, platform#2284).
#
# It sends N unauthenticated HTTPS requests to the probe hostname served by the
# default-off externalauth-probe component. Each one makes the gateway Envoy call
# oauth2-proxy before anything else:
#   * delivered  -> oauth2-proxy answers at once: 302/303 (sign-in redirect) or 401
#   * lost       -> 403 (ext_authz failure), a 5xx, or a client timeout (000)
#
# Verdicts:
#   0  DELIVERED            every request reached oauth2-proxy
#   1  SUBREQUESTS-LOST     some reached it and some did not: the cross-node black-hole
#   3  INCONCLUSIVE         nothing reached it, the filter is not applied (a 200), or
#                           another status (e.g. 404 because the component is off)
#   2  usage error
#
# It needs no credentials and prints only counts, never an address. CURL may point
# at another curl-compatible binary (the test uses a fake).

set -euo pipefail

usage() {
  printf 'usage: %s --host <probe-hostname> [--requests N] [--timeout SECONDS]\n' "${0##*/}" >&2
  exit 2
}

host=''
requests=120
timeout=10
curl_bin="${CURL:-curl}"

while (($# > 0)); do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || usage
      host="$2"
      shift 2
      ;;
    --requests)
      [[ $# -ge 2 ]] || usage
      requests="$2"
      shift 2
      ;;
    --timeout)
      [[ $# -ge 2 ]] || usage
      timeout="$2"
      shift 2
      ;;
    *) usage ;;
  esac
done

# The hostname reaches a URL, so it is restricted to DNS characters: no scheme,
# path, port, credentials or shell metacharacters can ride in on the input.
if [[ ! "${host}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; then
  printf 'ERROR: --host must be a lowercase DNS hostname (got %q)\n' "${host}" >&2
  exit 2
fi
if [[ ! "${requests}" =~ ^[0-9]+$ ]] || ((requests < 1 || requests > 1000)); then
  printf 'ERROR: --requests must be an integer from 1 to 1000\n' >&2
  exit 2
fi
if [[ ! "${timeout}" =~ ^[0-9]+$ ]] || ((timeout < 1 || timeout > 60)); then
  printf 'ERROR: --timeout must be an integer from 1 to 60\n' >&2
  exit 2
fi

delivered=0
denied=0
server_error=0
timed_out=0
unfiltered=0
other=0

for ((i = 1; i <= requests; i++)); do
  # curl prints 000 for a timeout or connection failure and exits non-zero, which
  # is an outcome to count here, not an error to stop on.
  code="$("${curl_bin}" --silent --output /dev/null --write-out '%{http_code}' \
    --proto '=https' --max-time "${timeout}" "https://${host}/" 2>/dev/null)" || true
  case "${code}" in
    302 | 303 | 401) delivered=$((delivered + 1)) ;;
    403) denied=$((denied + 1)) ;;
    5[0-9][0-9]) server_error=$((server_error + 1)) ;;
    000 | '') timed_out=$((timed_out + 1)) ;;
    200) unfiltered=$((unfiltered + 1)) ;;
    *) other=$((other + 1)) ;;
  esac
done

lost=$((denied + server_error + timed_out))

printf 'requests sent: %d\n' "${requests}"
printf 'delivered to the auth backend (302/303/401): %d\n' "${delivered}"
printf 'lost: %d (403: %d, 5xx: %d, timeout: %d)\n' "${lost}" "${denied}" "${server_error}" "${timed_out}"
printf 'not filtered (200): %d\n' "${unfiltered}"
printf 'other status: %d\n' "${other}"

verdict='INCONCLUSIVE'
reason=''
rc=3
if ((unfiltered > 0)); then
  reason='some requests reached the backend without an auth check, so the ExternalAuth filter is not applied on this route'
elif ((other > 0)); then
  reason='unexpected statuses (for example 404 when the probe component is not referenced), so the route is not serving the probe'
elif ((delivered == 0)); then
  reason='no request reached the auth backend at all, which is a broken route or backend rather than the partial cross-node loss this probe looks for'
elif ((lost == 0)); then
  verdict='DELIVERED'
  reason='every ext_authz subrequest reached oauth2-proxy'
  rc=0
else
  verdict='SUBREQUESTS-LOST'
  reason='some ext_authz subrequests reached oauth2-proxy and some did not, the partial loss #2284 reproduced'
  rc=1
fi

printf 'VERDICT: %s\n' "${verdict}"
printf 'Reason: %s\n' "${reason}"

if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  {
    printf '### ExternalAuth datapath probe\n\n'
    printf '| outcome | count |\n| --- | --- |\n'
    printf '| delivered (302/303/401) | %d |\n' "${delivered}"
    printf '| lost: 403 | %d |\n| lost: 5xx | %d |\n| lost: timeout | %d |\n' "${denied}" "${server_error}" "${timed_out}"
    printf '| not filtered (200) | %d |\n| other | %d |\n\n' "${unfiltered}" "${other}"
    printf '**VERDICT: %s**: %s\n' "${verdict}" "${reason}"
  } >>"${GITHUB_STEP_SUMMARY}"
fi

exit "${rc}"
