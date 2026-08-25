#!/usr/bin/env bash
# Enumerate every image a Talos ImageVerificationConfig rule would match, and record whether it
# carries a signature that rule would ACCEPT — PASS, FAIL or UNKNOWN, never a silent pass.
#
# WHY THIS EXISTS (#3334, under #3101)
# Talos verifies first-party images itself, and an image that MATCHES a rule but fails verification
# is refused at pull. Every such image is already cached on the fleet, so no rule-matching pull
# happens today and the rules have never produced a decision. The moment one does, every latent
# violation becomes a simultaneous ImagePullBackOff. That blast radius is measurable now, without
# touching a node — and until it is measured, enabling the control is a guess.
#
# 🔴 THE ENUMERATION IS THE HARD HALF, AND A HAND-WRITTEN LIST CANNOT DO IT.
# The apps that run here are not declared in this repository: they arrive as Flux OCIRepository
# artifacts published by each product's own repo, so `kubectl kustomize k8s/clusters/prod/` renders
# four Kustomizations and not one app image. The only source that cannot silently miss a workload
# added later is the fleet itself, so images are read from the live pod set and resolved to the
# digest the node ACTUALLY pulled (`.status.containerStatuses[].imageID`), not the spec's tag.
#
# 🔴 A cosign failure DOES NOT MEAN UNSIGNED, and treating it as one is the fail-open this guards.
# GHCR answers `DENIED` both for a signature that is absent and for a package the credential may not
# read. Those are opposite conclusions: absent is the outage this issue exists to prevent, while
# unreadable says nothing at all. They are separated by probing the IMAGE MANIFEST itself, and ONLY
# a manifest actually read (2xx) lets the verdict stand as FAIL — every other status, 401/403 and
# 404 and 429 and 5xx alike, establishes nothing and is UNKNOWN. Measured 2026-08-25:
# `ascoachingogvaner` and `wedding-app` are private and answer 401 to an ANONYMOUS read, while
# `doggy-countdown` answers 404 on its `.sig` TAG yet verifies fine, because its signature is an OCI
# referrer rather than a tag. A probe that only looked at `.sig` would have called that one unsigned.
#
# 🔴 401 IS NOT THE ONLY UNREADABLE STATUS, WHICH IS WHY THE CATCH-ALL IS THE LOAD-BEARING BRANCH.
# An anonymous read of a private package answers 401, but an identity that AUTHENTICATES and simply
# lacks read on that package gets 404 — GHCR masks existence rather than admitting the package is
# there. Measured 2026-08-26 against prod with an under-privileged GHCR identity: both private
# packages returned 404 and were correctly reported UNKNOWN with exit 1, not FAIL. An
# under-privileged credential is the likelier real-world failure than an absent one, so a rule that
# named only 401/403 as unreadable would call those two images REFUSED-at-pull on the strength of a
# permission problem.
#
# EXIT STATUS
#   0  every matched image PASSED
#   1  at least one matched image is FAIL or UNKNOWN — the blast radius is non-empty or unproven
#   2  usage, or the measurement could not be made at all (never reported as "nothing to fix")
set -euo pipefail

# Resolved with parameter expansion alone. `dirname` is an EXTERNAL command, and the cosign
# gate below is exercised by a test that strips PATH — so calling one here turns a clean
# "cosign is required" exit 2 into a crash, which is the fabricated verdict this script
# exists to never produce.
SCRIPT_DIR="${BASH_SOURCE[0]}"
case "$SCRIPT_DIR" in
  */*) SCRIPT_DIR="${SCRIPT_DIR%/*}" ;;
  *) SCRIPT_DIR="." ;;
esac
readonly SCRIPT_DIR
# shellcheck source=scripts/registry-auth-lib.sh
source "${SCRIPT_DIR}/registry-auth-lib.sh"

RULES_FILE="talos/cluster/verify-first-party-images.yaml"
KUBE_CONTEXT="admin@prod"
IMAGES_FILE=""

usage() {
  cat >&2 <<'EOF'
usage:
  inventory-first-party-image-signatures.sh [--rules PATH] [--context NAME]
  inventory-first-party-image-signatures.sh [--rules PATH] --images PATH

  --rules    ImageVerificationConfig to read rules from (default talos/cluster/verify-first-party-images.yaml)
  --context  kubectl context to enumerate the live fleet from (default admin@prod)
  --images   read newline-separated image references from PATH instead of the cluster (offline/fixture)

Test seams (override for hermetic tests; each must exit non-zero on failure):
  INVENTORY_ENUMERATE_CMD  prints one image reference per line
  INVENTORY_VERIFY_CMD     <ref> <issuer> <subjectRegex>  -> exit 0 when the signature is accepted
  INVENTORY_PROBE_CMD      <ref>  -> prints the HTTP status of the IMAGE manifest read
EOF
}

die() {
  echo "inventory: $*" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --rules)
      [ $# -ge 2 ] || die "--rules needs a value"
      RULES_FILE="$2"
      shift 2
      ;;
    --context)
      [ $# -ge 2 ] || die "--context needs a value"
      KUBE_CONTEXT="$2"
      shift 2
      ;;
    --images)
      [ $# -ge 2 ] || die "--images needs a value"
      IMAGES_FILE="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage
      die "unknown argument: $1"
      ;;
  esac
done

command -v yq >/dev/null 2>&1 || die "yq (Mike Farah v4) is required to read the rules"
[ -r "$RULES_FILE" ] || die "rules file not readable: $RULES_FILE"

# ---------------------------------------------------------------------------
# Rules — read from the file, in order. Never restated here: a second copy of a
# subjectRegex is a copy that can drift from the one the fleet enforces.
# ---------------------------------------------------------------------------
yq -e '.rules | tag == "!!seq"' "$RULES_FILE" >/dev/null 2>&1 ||
  die "$RULES_FILE has no .rules sequence"

# 🔴 Completeness is asserted on the YAML, NOT on the rendered TSV. `yq ... | @tsv` renders a
# MISSING field as the four-character string `null`, which is not empty — so a string test over the
# TSV waves an incomplete rule straight through and hands `null` to cosign as the identity regex.
# Verified against a fixture with no subjectRegex: the row reads `<glob>\t<issuer>\tnull`.
incomplete="$(yq -r '
  [ .rules[]
    | select( ((.image // "") | tostring | length) == 0
           or ((.keyless.issuer // "") | tostring | length) == 0
           or ((.keyless.subjectRegex // "") | tostring | length) == 0 )
  ] | length' "$RULES_FILE")" || die "could not validate rules in $RULES_FILE"
[ "$incomplete" = "0" ] ||
  die "incomplete rule in $RULES_FILE — ${incomplete} rule(s) missing image, issuer or subjectRegex"

rules_tsv="$(yq -r '.rules[] | [.image, .keyless.issuer, .keyless.subjectRegex] | @tsv' "$RULES_FILE")" ||
  die "could not parse rules from $RULES_FILE"
[ -n "$rules_tsv" ] || die "no rules found in $RULES_FILE — refusing to report an empty blast radius"

rule_count=$(printf '%s\n' "$rules_tsv" | grep -c .)

# cosign is what decides PASS. Absent, every verification returns non-zero and each image whose
# manifest reads 2xx would be classified FAIL — a blast radius produced entirely by a missing tool
# rather than by the fleet. Checked after the rules are validated, so a malformed or unreadable
# rules file still reports its own error first, and before enumeration, so it fails before doing
# work whose result could not mean anything.
if [ -z "${INVENTORY_VERIFY_CMD:-}" ]; then
  command -v cosign >/dev/null 2>&1 || die "cosign is required to verify signatures"
fi

# ---------------------------------------------------------------------------
# Enumeration
# ---------------------------------------------------------------------------
enumerate_cluster() {
  kubectl --context "$KUBE_CONTEXT" get pods --all-namespaces -o json |
    jq -r '
      .items[]
      | ( (.status.containerStatuses // []) + (.status.initContainerStatuses // [])
          + (.status.ephemeralContainerStatuses // []) ) as $st
      | (.spec.containers // []) + (.spec.initContainers // []) + (.spec.ephemeralContainers // [])
      | .[]
      | . as $c
      | ($st | map(select(.name == $c.name)) | first | .imageID // "") as $resolved
      | if ($resolved | test("^[a-z0-9.-]+/")) then $resolved else $c.image end
    '
}

if [ -n "$IMAGES_FILE" ]; then
  [ -r "$IMAGES_FILE" ] || die "images file not readable: $IMAGES_FILE"
  all_images="$(cat "$IMAGES_FILE")"
elif [ -n "${INVENTORY_ENUMERATE_CMD:-}" ]; then
  all_images="$(eval "$INVENTORY_ENUMERATE_CMD")" || die "enumeration command failed"
else
  command -v kubectl >/dev/null 2>&1 || die "kubectl is required to enumerate the fleet"
  command -v jq >/dev/null 2>&1 || die "jq is required to enumerate the fleet"
  all_images="$(enumerate_cluster)" || die "could not enumerate pods from context $KUBE_CONTEXT"
fi

all_images="$(printf '%s\n' "$all_images" | grep . | sort -u || true)"
total=$(printf '%s\n' "$all_images" | grep -c . || true)

# An empty enumeration is a claim about the ENUMERATOR, never about the cluster. Reporting
# "0 failures" from it would be the exact fail-open this whole script exists to close.
[ "$total" -gt 0 ] || die "enumeration produced no images at all — the measurement did not run"

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
verify_signature() { # ref issuer subjectRegex
  if [ -n "${INVENTORY_VERIFY_CMD:-}" ]; then
    "$INVENTORY_VERIFY_CMD" "$1" "$2" "$3" >/dev/null 2>&1
    return $?
  fi
  cosign verify --certificate-oidc-issuer "$2" --certificate-identity-regexp "$3" "$1" >/dev/null 2>&1
}

# Prints the HTTP status of a read of the IMAGE manifest — the discriminator between
# "no signature" and "cannot look". Prints 000 when the probe itself could not run.
probe_manifest_status() { # ref
  if [ -n "${INVENTORY_PROBE_CMD:-}" ]; then
    "$INVENTORY_PROBE_CMD" "$1" 2>/dev/null || echo 000
    return 0
  fi
  local ref="$1" registry repo reference token
  registry="${ref%%/*}"
  case "$registry" in *.* | *:* | localhost) ;; *)
    echo 000
    return 0
    ;;
  esac
  local rest="${ref#*/}"
  case "$rest" in
    *@*)
      repo="${rest%@*}"
      reference="${rest##*@}"
      ;;
    *:*)
      repo="${rest%:*}"
      reference="${rest##*:}"
      ;;
    *)
      repo="$rest"
      reference="latest"
      ;;
  esac
  command -v curl >/dev/null 2>&1 || {
    echo 000
    return 0
  }
  # The token exchange is what must carry the credential: an anonymous token for a PRIVATE
  # repository is issued happily and then buys a 401 on the manifest, which is exactly the
  # "unreadable" answer this probe exists to distinguish from "unsigned". Basic auth is
  # written to a curl config file rather than argv, so the credential is not visible in the
  # process table of a shared runner and cannot be echoed by a traced shell.
  local auth_b64 auth_conf="" curl_conf_args=()
  auth_b64="$(registry_credential_b64 "$registry" || true)"
  if [ -n "$auth_b64" ]; then
    # This function runs inside a command substitution, so the EXIT trap below belongs to THAT
    # subshell and fires the moment this probe returns — exactly the lifetime the credential
    # file should have. A plain `rm` after the call would not survive a signal; the trap does.
    # `mktemp` creates the file 0600, so the credential is never briefly world-readable.
    if auth_conf="$(mktemp 2>/dev/null)"; then
      # shellcheck disable=SC2064  # expand now: the path must be named at trap time
      trap "rm -f '${auth_conf}'" EXIT
    fi
    if [ -n "$auth_conf" ]; then
      printf 'header = "Authorization: Basic %s"\n' "$auth_b64" >"$auth_conf"
      curl_conf_args=(--config "$auth_conf")
    fi
  fi
  token="$(curl -sSf --max-time 20 ${curl_conf_args[@]+"${curl_conf_args[@]}"} \
    "https://${registry}/token?scope=repository:${repo}:pull&service=${registry}" 2>/dev/null |
    jq -r '.token // .access_token // empty' 2>/dev/null || true)"
  [ -z "$auth_conf" ] || : >"$auth_conf"
  curl -sS -o /dev/null -w '%{http_code}' --max-time 20 \
    ${token:+-H "Authorization: Bearer ${token}"} \
    -H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
    "https://${registry}/v2/${repo}/manifests/${reference}" 2>/dev/null || echo 000
}

repository_of() { # strip the tag or digest, so a rule glob matches the NAME
  local ref="$1" registry="${1%%/*}" rest="${1#*/}"
  case "$rest" in
    *@*) rest="${rest%@*}" ;;
    *:*) rest="${rest%:*}" ;;
  esac
  printf '%s/%s' "$registry" "$rest"
}

matched=0 pass=0 fail=0 unknown=0
results=""

while IFS= read -r ref; do
  [ -n "$ref" ] || continue
  repo_name="$(repository_of "$ref")"
  hit_glob="" hit_issuer="" hit_subject=""
  # First match wins — the order Talos evaluates these in. Reordering the file changes
  # which identity an image is held to, so the order is read, never re-sorted.
  while IFS="$(printf '\t')" read -r glob issuer subject; do
    [ -n "$glob" ] || continue
    # shellcheck disable=SC2254  # the glob is data from the rules file, and must stay a pattern
    case "$repo_name" in
      $glob)
        hit_glob="$glob"
        hit_issuer="$issuer"
        hit_subject="$subject"
        break
        ;;
    esac
  done <<EOF
$rules_tsv
EOF
  [ -n "$hit_glob" ] || continue

  matched=$((matched + 1))
  if verify_signature "$ref" "$hit_issuer" "$hit_subject"; then
    pass=$((pass + 1))
    results="${results}PASS	${ref}	${hit_glob}	signature accepted by the rule identity
"
    continue
  fi

  status="$(probe_manifest_status "$ref")"
  # FAIL is reserved for a manifest we actually READ. Any other status — 404, 429, 5xx — says the
  # read did not succeed, so it establishes nothing about the signature and must not be counted as
  # an image that would be refused. Overstating the blast radius is the same class of confident-but-
  # wrong verdict as #3108, just pointing the other way.
  case "$status" in
    2??)
      fail=$((fail + 1))
      results="${results}FAIL	${ref}	${hit_glob}	image manifest readable (HTTP ${status}) but no signature the rule accepts — this image would be REFUSED at pull
"
      ;;
    401 | 403)
      unknown=$((unknown + 1))
      results="${results}UNKNOWN	${ref}	${hit_glob}	image manifest unreadable (HTTP ${status}) — credential lacks access, signature state not established
"
      ;;
    000)
      unknown=$((unknown + 1))
      results="${results}UNKNOWN	${ref}	${hit_glob}	manifest probe could not run — signature state not established
"
      ;;
    *)
      unknown=$((unknown + 1))
      results="${results}UNKNOWN	${ref}	${hit_glob}	image manifest read failed (HTTP ${status}) — signature state not established
"
      ;;
  esac
done <<EOF
$all_images
EOF

printf 'verdict\timage\trule\tdetail\n'
printf '%s' "$results"
printf '\n'
printf 'rules=%s images_enumerated=%s matched=%s PASS=%s FAIL=%s UNKNOWN=%s\n' \
  "$rule_count" "$total" "$matched" "$pass" "$fail" "$unknown"

# Zero matches out of a non-empty enumeration cannot be distinguished from a matcher that stopped
# working, and AC1's whole point is that a workload must not be silently missed. Fail closed.
if [ "$matched" -eq 0 ]; then
  echo "inventory: no image matched any rule — the matcher or the enumeration is broken" >&2
  exit 2
fi

if [ "$fail" -gt 0 ] || [ "$unknown" -gt 0 ]; then
  echo "inventory: blast radius is non-empty or unproven — enabling enforcement is not yet a no-op" >&2
  exit 1
fi
echo "inventory: every matched image verifies — enabling enforcement refuses nothing that runs today"
