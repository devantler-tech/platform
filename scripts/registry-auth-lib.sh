#!/usr/bin/env bash
# Resolve an OCI registry credential from a Docker config, for callers that must speak the registry
# v2 token flow themselves.
#
# WHY THIS EXISTS (#3372, under #3334)
# `cosign` reads a Docker config natively through go-containerregistry, so pointing DOCKER_CONFIG at
# the cluster's own pull credential is enough to make it verify a private first-party package. The
# signature inventory's manifest PROBE is not cosign: it performs the token exchange itself, because
# what it needs is the HTTP status of the image manifest — the one signal that separates "this image
# carries no signature" from "this credential may not look". That probe therefore needs the same
# credential cosign is already using, read from the same place, or it keeps answering 401 for a
# package that is merely private and the inventory reports UNKNOWN forever.
#
# 🔴 RETURNING A CREDENTIAL WE DO NOT ACTUALLY HAVE IS THE FAIL-OPEN HERE.
# Every malformed, partial, absent or helper-backed entry yields the empty string, and the caller
# then probes anonymously — which is honest, and still produces UNKNOWN rather than a verdict. The
# dangerous direction is the opposite one: a lookup that returns a half-formed credential makes the
# probe believe it had authority it lacked, so a private package's 401 gets read as a statement
# about its signature instead of about our access. A `credsStore` entry is empty for the same
# reason — the secret lives in a helper binary this deliberately does not execute.

# Print the base64 "user:password" for a registry, or nothing at all.
registry_credential_b64() { # registry
  local registry="${1:-}" config candidate decoded
  [ -n "$registry" ] || return 0

  config="${DOCKER_CONFIG:-${HOME}/.docker}/config.json"
  [ -r "$config" ] || return 0
  command -v jq >/dev/null 2>&1 || return 0

  # The key is matched on its HOST, so the legacy `https://ghcr.io/v1/` form and a bare `ghcr.io`
  # resolve to the same registry rather than one of them silently missing.
  candidate="$(
    jq -r --arg reg "$registry" '
      (.auths // {})
      | to_entries
      | map(select((.key | sub("^https?://"; "") | sub("/.*$"; "")) == $reg))
      | .[0].value // {}
      | if ((.auth // "") | length) > 0 then .auth
        elif (((.username // "") | length) > 0 and ((.password // "") | length) > 0)
        then ((.username + ":" + .password) | @base64)
        else empty end
    ' "$config" 2>/dev/null
  )" || return 0
  [ -n "$candidate" ] || return 0

  # A pre-encoded `auth` is attacker-adjacent only in the sense that it is unvalidated input from a
  # file: decode it and require a non-empty user and secret, exactly as the SOPS bridge does, so a
  # truncated or padded entry cannot be handed to a registry as though it were a credential.
  decoded="$(printf '%s' "$candidate" | base64 -d 2>/dev/null)" || return 0
  case "$decoded" in
    *:*) ;;
    *) return 0 ;;
  esac
  [ -n "${decoded%%:*}" ] && [ -n "${decoded#*:}" ] || return 0

  printf '%s' "$candidate"
}
