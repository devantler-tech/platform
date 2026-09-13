#!/usr/bin/env bash
#
# Confirm that a registry image digest carries no cosign signature, attestation,
# SBOM or OCI referrer.
#
# Two callers need exactly this answer:
#   * .github/workflows/publish-unsigned-probe-image.yaml, right after it
#     publishes the probe's deliberately unsigned negative control;
#   * scripts/probe-image-signature-enforcement.sh, right before it pulls that
#     control on a node. Publishing and probing are separate runs, so a package
#     writer could sign the image in between. Re-checking just before the pull is
#     what keeps a now-signed control from being misread as broken enforcement.
#
# WHAT "UNSIGNED" MEANS. For <registry>/<repository>@sha256:<hex>:
#   * the digest's own manifest reads 200 with the same credential, so a later
#     404 means "absent" and not "no access";
#   * the tags cosign derives from the digest (sha256-<hex>, and its .sig, .att
#     and .sbom forms) each answer 404;
#   * the OCI referrers API lists no manifests, or is not offered (404), in which
#     case the tags above are where cosign stores signatures.
# Anything short of all three is UNKNOWN, never unsigned.
#
# ONLY A DIGEST IS ACCEPTED. A tag can be moved to a different image between
# this check and a pull, so a check against a tag proves nothing about what is
# pulled later.
#
# CREDENTIALS. REGISTRY_USERNAME and REGISTRY_PASSWORD are exchanged for a pull
# token. Neither the password nor the token is ever a curl argument: curl is an
# external process, and its arguments are visible in the process table to every
# other user on the host for as long as it runs. Both travel in a curl config
# file inside a private temporary directory that an EXIT trap removes, the same
# pattern scripts/inventory-first-party-image-signatures.sh uses.
#
# Exit status:
#   0  UNSIGNED  every signature location is confirmed empty
#   1  SIGNED    a signature, attestation or SBOM tag, or a referrer, exists
#   2  usage     the ref is not a digest ref, or credentials are missing
#   3  UNKNOWN   the answer could not be established: an auth failure, an
#                unexpected status, a transport error or a malformed response

set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: verify-image-unsigned.sh --image <registry>/<repository>@sha256:<64 hex>

Environment: REGISTRY_USERNAME and REGISTRY_PASSWORD (a credential that can read the image)

Exit status: 0 unsigned, 1 signed, 2 usage error, 3 unknown (never read as unsigned)
USAGE
}

fail_usage() {
  printf 'ERROR: %s\n\n' "$1" >&2
  usage
  exit 2
}

fail_signed() {
  printf 'SIGNED: %s\n' "$1" >&2
  exit 1
}

fail_unknown() {
  printf 'UNKNOWN: %s\n' "$1" >&2
  printf 'UNKNOWN is NOT unsigned: nothing about signatures was established.\n' >&2
  exit 3
}

image=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image)
      [[ $# -ge 2 ]] || fail_usage '--image requires a value'
      image="$2"
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) fail_usage "unknown argument: $1" ;;
  esac
  shift
done

[[ -n "${image}" ]] || fail_usage '--image is required'

ref_re='^([a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?)/([a-z0-9]+([._/-][a-z0-9]+)*)@sha256:([0-9a-f]{64})$'
[[ "${image}" =~ ${ref_re} ]] ||
  fail_usage "'${image}' is not <registry>/<repository>@sha256:<64 lowercase hex>; only a digest names one image that cannot be moved"
readonly registry="${BASH_REMATCH[1]}"
readonly repository="${BASH_REMATCH[4]}"
readonly hex="${BASH_REMATCH[6]}"
readonly digest="sha256:${hex}"

[[ -n "${REGISTRY_USERNAME:-}" && -n "${REGISTRY_PASSWORD:-}" ]] ||
  fail_usage 'REGISTRY_USERNAME and REGISTRY_PASSWORD must both be set'
case "${REGISTRY_USERNAME}${REGISTRY_PASSWORD}" in
  *$'\n'* | *$'\r'*) fail_usage 'the registry credential contains a line break' ;;
esac

command -v curl >/dev/null 2>&1 || fail_unknown 'curl is not installed'
command -v jq >/dev/null 2>&1 || fail_unknown 'jq is not installed'

work="$(mktemp -d)" || fail_unknown 'could not create a private working directory'
readonly work
trap 'rm -rf "${work}"' EXIT
chmod 700 "${work}"
readonly conf="${work}/curl.conf"
readonly body="${work}/body"
(
  umask 077
  : >"${conf}"
  : >"${body}"
)

# A value inside a double-quoted curl config string, with `\` and `"` escaped.
conf_string() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  printf '"%s"' "${value}"
}

# http <url> <accept> <output file> — prints the HTTP status, or 000 when no
# response was received. Authentication comes only from the config file.
http() {
  local code
  code="$(curl -sS --max-time 20 --config "${conf}" -H "Accept: $2" -o "$3" -w '%{http_code}' "$1" 2>/dev/null)" || true
  [[ "${code}" =~ ^[0-9]{3}$ ]] || code='000'
  printf '%s' "${code}"
}

# Exchange the credential for a pull token. `printf` is a shell builtin, so
# writing the file starts no process whose arguments could expose the secret.
printf 'user = %s\n' "$(conf_string "${REGISTRY_USERNAME}:${REGISTRY_PASSWORD}")" >"${conf}"
code="$(http "https://${registry}/token?service=${registry}&scope=repository:${repository}:pull" 'application/json' "${body}")"
[[ "${code}" == '200' ]] ||
  fail_unknown "the registry refused the credential for ${registry}/${repository} (token endpoint HTTP ${code})"
token="$(jq -er '(.token // .access_token) | select(type == "string" and length > 0)' "${body}" 2>/dev/null)" ||
  fail_unknown "the token endpoint for ${registry}/${repository} returned no token"
case "${token}" in
  *$'\n'* | *$'\r'*) fail_unknown 'the token endpoint returned a malformed token' ;;
esac
printf 'header = %s\n' "$(conf_string "Authorization: Bearer ${token}")" >"${conf}"
unset token

readonly manifest_accept='application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json'
readonly base="https://${registry}/v2/${repository}"

code="$(http "${base}/manifests/${digest}" "${manifest_accept}" /dev/null)"
[[ "${code}" == '200' ]] ||
  fail_unknown "${image} is not readable with the supplied credential (HTTP ${code}), so an absent signature could not be told apart from no access"

for suffix in '' '.sig' '.att' '.sbom'; do
  tag="sha256-${hex}${suffix}"
  code="$(http "${base}/manifests/${tag}" "${manifest_accept}" /dev/null)"
  case "${code}" in
    404) ;;
    200) fail_signed "${image} has a ${tag} artifact" ;;
    *) fail_unknown "could not establish whether ${tag} exists for ${registry}/${repository} (HTTP ${code})" ;;
  esac
done

: >"${body}"
code="$(http "${base}/referrers/${digest}" 'application/vnd.oci.image.index.v1+json' "${body}")"
case "${code}" in
  200)
    count="$(jq -er '.manifests | if type == "array" then length else error("no manifests array") end' "${body}" 2>/dev/null)" ||
      fail_unknown "the referrers response for ${image} is not a valid index"
    [[ "${count}" == '0' ]] || fail_signed "${image} has ${count} OCI referrer(s) attached"
    ;;
  404) ;;
  *) fail_unknown "could not read referrers for ${image} (HTTP ${code})" ;;
esac

printf 'UNSIGNED: %s has no signature, attestation or SBOM tag and no referrers\n' "${image}"
