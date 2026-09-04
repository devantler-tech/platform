#!/usr/bin/env bash
# Require cosign verification on every devantler-tech OCIRepository, discovered by KIND
# and URL rather than by the text of a cosign subject (#3558, under #3308).
#
# WHY THIS EXISTS
# Both cosign-subject guards (scripts/guard-shared-publish-workflow-pin.sh and
# scripts/guard-publish-workflow-approved-revisions.sh) examine a file only when it
# already carries a shared-publish-workflow subject. An OCIRepository pointed at a
# devantler-tech artifact with NO `spec.verify` at all therefore carries no subject, is
# examined by neither guard, and is refused by nothing else: validate-flux-verify covers
# the root source only, and the Kyverno cluster policies do not touch OCIRepositories.
# Flux would apply an unsigned or foreign-signed artifact with every check green. This
# guard closes that shape by asking the question the other way round: every
# OCIRepository whose URL is a devantler-tech GHCR artifact must verify.
#
# WHAT IT REQUIRES, PER OCIRepository UNDER oci://ghcr.io/devantler-tech/
#   - `spec.verify` present
#   - `spec.verify.provider: cosign`
#   - `spec.verify.matchOIDCIdentity` a sequence of EXACTLY ONE entry. Flux ORs the
#     entries, so a second entry widens the trusted signer set while the first still
#     reads as narrowed; alternation belongs inside the one subject regex, where the
#     subject guards judge it.
#   - that entry names a non-empty `issuer` and a non-empty `subject`.
#
# DISCOVERY IS BY KIND AT ANY DEPTH. A YAML document is walked with yq and every mapping
# carrying `kind: OCIRepository` is examined — a top-level manifest, a later document in
# a multi-document file, or a template nested inside a ResourceGraphDefinition. A
# line-based scan would see none of the nested forms, and a top-level-only read would
# miss the tenant template that every new tenant inherits.
#
# WHAT IT DOES NOT COVER, BY NAME
#   - The ROOT source `flux-system/flux-system` is not a document in this tree: the
#     FluxInstance creates it, and its `spec.verify` (a branch identity, legitimately)
#     is supplied by a kustomize patch in flux-instance.yaml. That verification is
#     asserted by scripts/validate-flux-verify against both halves of its config.
#   - OCIRepositories outside oci://ghcr.io/devantler-tech/ are out of scope; this guard
#     is about the suite's own artifacts.
#
# EXEMPTIONS ARE EXPLICIT, URL-KEYED, AND FAIL CLOSED. An OCIRepository that cannot verify
# yet is admitted only by an entry in EXEMPTIONS below, carrying its reason and the
# issue that retires it. Every exemption must still match an OCIRepository in the tree
# (otherwise it is STALE and the run fails), and an exempt OCIRepository that gains
# `spec.verify` fails too, so the list can only shrink as artifacts start signing.
#
# THE FLOOR. An empty result from a filtered read is a claim about the filter: if the
# manifests move, the URL scheme changes, or yq stops matching, the scan returns nothing
# and — without this — the guard would report a clean tree while checking nothing. The
# in-scope count must reach EXPECTED_MIN_IN_SCOPE, raised when a consumer is genuinely
# added and lowered only after verifying by hand that the scan still finds the rest.
#
# SEAMS (for scripts/tests/test-guard-oci-repository-verify.sh)
#   OCI_VERIFY_SCAN_ROOT         directory to scan (default: <repo>/k8s)
#   OCI_VERIFY_EXEMPTIONS_FILE   exemption rows `<url>\t<reason>` (default: the list below)
#   OCI_VERIFY_EXPECTED_MIN      in-scope floor (default: EXPECTED_MIN_IN_SCOPE)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly SCAN_ROOT="${OCI_VERIFY_SCAN_ROOT:-$REPO_ROOT/k8s}"
readonly REQUIRED_URL_PREFIX='oci://ghcr.io/devantler-tech/'

# 6 on 2026-09-04: the aws, github-config, wedding-app and ascoachingogvaner manifests
# consumers, the data-product-controller chart, and the tenant RGD template.
readonly EXPECTED_MIN_IN_SCOPE=6
readonly EXPECTED_MIN="${OCI_VERIFY_EXPECTED_MIN:-$EXPECTED_MIN_IN_SCOPE}"

# <url><TAB><reason>, one per line. The reason names what retires the entry.
DEFAULT_EXEMPTIONS="$(
  cat <<'EOF'
oci://ghcr.io/devantler-tech/charts/data-product-controller	the chart is published unsigned — a bare `helm push` with no cosign step (devantler-tech/data-product-controller#27 adds the signing); the OCIRepository is digest-pinned and the app is staged off (platform#3476). Remove this row when #27 ships and the manifest gains spec.verify.
EOF
)"
readonly DEFAULT_EXEMPTIONS

refuse() {
  printf 'guard: %s\n' "$*" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || refuse 'yq is required and was not found on PATH'
[ -d "$SCAN_ROOT" ] || refuse "scan root $SCAN_ROOT is not a directory"

case "$EXPECTED_MIN" in
  '' | *[!0-9]*) refuse "OCI_VERIFY_EXPECTED_MIN must be a non-negative integer, got '$EXPECTED_MIN'" ;;
esac

exemptions="$DEFAULT_EXEMPTIONS"
if [ -n "${OCI_VERIFY_EXEMPTIONS_FILE:-}" ]; then
  [ -f "$OCI_VERIFY_EXEMPTIONS_FILE" ] || refuse "OCI_VERIFY_EXEMPTIONS_FILE $OCI_VERIFY_EXEMPTIONS_FILE does not exist"
  exemptions="$(cat "$OCI_VERIFY_EXEMPTIONS_FILE")"
fi

# exempt_reason <url> — prints the reason and returns 0 when the URL is exempt.
exempt_reason() {
  local url="$1" row row_url
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    case "$row" in '#'*) continue ;; esac
    row_url="${row%%	*}"
    if [ "$row_url" = "$url" ]; then
      case "$row" in
        *"	"*) printf '%s' "${row#*	}" ;;
        *) refuse "exemption for $url carries no reason; every exemption names what retires it" ;;
      esac
      return 0
    fi
  done <<EOF
$exemptions
EOF
  return 1
}

# One row per OCIRepository mapping found anywhere in the file, tab-separated:
#   url  name  has_verify  provider  identities_type  identities_count  incomplete_entries
# `..` walks every node of every document, so a nested template counts exactly like a
# top-level manifest. A mapping is examined only when its `kind` is OCIRepository.
readonly YQ_ROWS='[.. | select(type == "!!map" and .kind == "OCIRepository")]
  | .[]
  | [
      (.spec.url // ""),
      (.metadata.name // ""),
      ((.spec // {}) | has("verify")),
      (.spec.verify.provider // ""),
      (.spec.verify.matchOIDCIdentity | type),
      ((.spec.verify.matchOIDCIdentity | select(type == "!!seq") | length) // 0),
      (([.spec.verify.matchOIDCIdentity | select(type == "!!seq") | .[]
          | select(((.issuer // "") == "") or ((.subject // "") == ""))] | length))
    ]
  | @tsv'

status=0
in_scope=0
seen_exempt=""
fail() {
  printf '%s\n' "$*" >&2
  status=1
}

while IFS= read -r file; do
  [ -n "$file" ] || continue
  # FAIL CLOSED on a file yq cannot read: an unparseable manifest is not "no
  # OCIRepository here", it is "unknown", and unknown must not read as clean.
  if ! rows="$(yq -r "$YQ_ROWS" "$file" 2>&1)"; then
    fail "$file: yq could not read this file, so its OCIRepositories are UNKNOWN: $rows"
    continue
  fi
  while IFS=$'\t' read -r url name has_verify provider ids_type ids_count incomplete; do
    [ -n "$url$name" ] || continue
    case "$url" in
      "$REQUIRED_URL_PREFIX"*) ;;
      *) continue ;;
    esac
    in_scope=$((in_scope + 1))
    if reason="$(exempt_reason "$url")"; then
      seen_exempt="$seen_exempt$url
"
      if [ "$has_verify" = "true" ]; then
        fail "$file: OCIRepository $name ($url) is exempt from verification but carries spec.verify; the exemption is stale — remove it ($reason)"
      fi
      continue
    fi
    if [ "$has_verify" != "true" ]; then
      fail "$file: OCIRepository $name ($url) has NO spec.verify — Flux would apply an unsigned or foreign-signed artifact. Add provider: cosign with exactly one matchOIDCIdentity entry."
      continue
    fi
    if [ "$provider" != "cosign" ]; then
      fail "$file: OCIRepository $name ($url) verifies with provider '${provider:-<none>}', not cosign"
      continue
    fi
    if [ "$ids_type" != "!!seq" ]; then
      fail "$file: OCIRepository $name ($url) has no matchOIDCIdentity sequence (found ${ids_type:-nothing}); a verify block that names no identity admits no signer and pins none"
      continue
    fi
    if [ "$ids_count" -eq 0 ]; then
      fail "$file: OCIRepository $name ($url) has ZERO matchOIDCIdentity entries; verification with no identity is not a matcher"
      continue
    fi
    if [ "$ids_count" -ne 1 ]; then
      fail "$file: OCIRepository $name ($url) has $ids_count matchOIDCIdentity entries; Flux ORs them, so a second entry WIDENS the trusted signer set. Put any alternation inside the one subject regex."
      continue
    fi
    if [ "$incomplete" -ne 0 ]; then
      fail "$file: OCIRepository $name ($url) has a matchOIDCIdentity entry missing a non-empty issuer or subject"
      continue
    fi
  done <<EOF
$rows
EOF
done < <(find "$SCAN_ROOT" -type f \( -name '*.yaml' -o -name '*.yml' \) | LC_ALL=C sort)

# Every exemption must still name an OCIRepository that exists, or it is a hole waiting
# for a manifest to fall into it.
while IFS= read -r row; do
  [ -n "$row" ] || continue
  case "$row" in '#'*) continue ;; esac
  row_url="${row%%	*}"
  case "$seen_exempt" in
    *"$row_url
"*) ;;
    *) fail "exemption for $row_url is STALE: no OCIRepository in $SCAN_ROOT carries that URL. Remove the row." ;;
  esac
done <<EOF
$exemptions
EOF

if [ "$in_scope" -lt "$EXPECTED_MIN" ]; then
  fail "found $in_scope in-scope OCIRepositor(y/ies) under $REQUIRED_URL_PREFIX, expected at least $EXPECTED_MIN. The scan, not the tree, is the likely cause: verify by hand, then fix the discovery or lower the floor with the reason."
fi

if [ "$status" -eq 0 ]; then
  printf 'guard: %d devantler-tech OCIRepositor(y/ies) all verify with cosign and exactly one identity (exemptions honoured: %d).\n' \
    "$in_scope" "$(printf '%s' "$seen_exempt" | grep -c . || true)"
fi
exit "$status"
