#!/usr/bin/env bash

set -euo pipefail

while (($# > 0)); do
  if [[ "$1" == '--data-binary' ]]; then
    printf '%s' "$2" >"${UMAMI_CURL_PAYLOAD_FILE:?}"
    shift 2
    continue
  fi
  shift
done

printf '%s' "${UMAMI_CURL_RESPONSE:?}"
