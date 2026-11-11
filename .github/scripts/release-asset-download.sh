# shellcheck shell=bash
# Download one GitHub release asset, preferring the authenticated asset API (#5942).
#
# WHY THIS EXISTS
# The setup helpers used to fetch every release asset from the public
# `https://github.com/<owner>/<repo>/releases/download/...` URL with no credentials.
# Hosted runners share their outbound addresses, so those requests count as
# anonymous traffic. On 2026-11-09 a GitHub incident that slowed anonymous traffic
# made every one of those downloads return 504 until curl gave up, and the whole
# platform delivery path stopped before any check of our own ran.
#
# When GITHUB_TOKEN is set, this asks the release-asset API for the same bytes
# with the job's token first, and falls back to the anonymous URL if that fails.
# Without a token it goes straight to the anonymous URL, exactly as before.
#
# 🔴 THIS CHOOSES WHERE THE BYTES COME FROM, NEVER WHETHER THEY ARE TRUSTED.
# Every caller still checks the downloaded file against its reviewed SHA-256 pin
# (or its published digests) before installing or running anything. Neither path
# is more trusted than the other: both serve the same release asset.
#
# Each call prints ONE line to stderr per attempt, with fixed fields only:
#   release-asset asset=<name> path=<authenticated|anonymous> status=<http code> result=<served|failed|unlisted>
# The status is curl's final HTTP code (000 when no response arrived); `unlisted`
# means the release metadata was read but did not list the asset exactly once. The token,
# request headers and response bodies are never printed. curl does not forward the
# Authorization header when the API redirects to the storage host.
#
# Every local is `rad_`-prefixed: this file is SOURCED, and a caller's readonly
# variable of the same name would otherwise abort it under `set -e`.

# Print a validated three-digit HTTP code, or 000 for anything else.
release_asset_status() { # raw
  if [[ "${1-}" =~ ^[0-9]{3}$ ]]; then
    printf '%s' "$1"
  else
    printf '000'
  fi
}

release_asset_log() { # asset path status result
  printf 'release-asset asset=%s path=%s status=%s result=%s\n' "$1" "$2" "$(release_asset_status "$3")" "$4" >&2
}

# release_asset_download OWNER/REPO TAG ASSET OUTPUT
# Write ASSET from release TAG of OWNER/REPO to OUTPUT. Returns non-zero, with
# OUTPUT removed, when neither path served it.
release_asset_download() {
  local rad_repo="${1:?owner/repo required}" rad_tag="${2:?tag required}"
  local rad_asset="${3:?asset name required}" rad_output="${4:?output path required}"
  local rad_status rad_metadata rad_id
  # Transient failures (timeouts, 408, 429 and 5xx) are retried a few times on each
  # path (#4549), capped so a long server-requested wait cannot hold the job.
  local -a rad_curl=(curl --fail --silent --show-error --location
    --proto '=https' --proto-redir '=https'
    --retry 5 --retry-delay 3 --retry-max-time 60)

  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    local -a rad_auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}" -H "X-GitHub-Api-Version: 2022-11-28")
    rad_metadata="$(mktemp "${RUNNER_TEMP:-/tmp}/release-asset.XXXXXX")"
    rad_id=''
    if rad_status="$("${rad_curl[@]}" "${rad_auth[@]}" -H 'Accept: application/vnd.github+json' \
      --write-out '%{http_code}' --output "${rad_metadata}" \
      "https://api.github.com/repos/${rad_repo}/releases/tags/${rad_tag}")"; then
      rad_id="$(jq -r --arg asset "${rad_asset}" \
        '[.assets[]? | select(.name == $asset) | .id | select(type == "number")] | if length == 1 then .[0] else empty end' \
        "${rad_metadata}" 2>/dev/null)" || rad_id=''
    fi
    rm -f -- "${rad_metadata}"
    if [[ "${rad_id}" =~ ^[0-9]+$ ]]; then
      if rad_status="$("${rad_curl[@]}" "${rad_auth[@]}" -H 'Accept: application/octet-stream' \
        --write-out '%{http_code}' --output "${rad_output}" \
        "https://api.github.com/repos/${rad_repo}/releases/assets/${rad_id}")"; then
        release_asset_log "${rad_asset}" authenticated "${rad_status}" served
        return 0
      fi
      release_asset_log "${rad_asset}" authenticated "${rad_status:-000}" failed
    elif [[ "$(release_asset_status "${rad_status:-}")" == 2* ]]; then
      # The release was read but does not list this asset exactly once.
      release_asset_log "${rad_asset}" authenticated "${rad_status}" unlisted
    else
      release_asset_log "${rad_asset}" authenticated "${rad_status:-000}" failed
    fi
    rm -f -- "${rad_output}"
  fi

  if rad_status="$("${rad_curl[@]}" --write-out '%{http_code}' --output "${rad_output}" \
    "https://github.com/${rad_repo}/releases/download/${rad_tag}/${rad_asset}")"; then
    release_asset_log "${rad_asset}" anonymous "${rad_status}" served
    return 0
  fi
  release_asset_log "${rad_asset}" anonymous "${rad_status:-000}" failed
  rm -f -- "${rad_output}"
  return 1
}
