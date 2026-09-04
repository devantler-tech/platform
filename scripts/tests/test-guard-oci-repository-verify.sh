#!/usr/bin/env bash
# RED/GREEN coverage for scripts/guard-oci-repository-verify.sh (#3558).
#
# WHAT IS ACTUALLY BEING PROVED
# The guard's one harmful failure mode is passing: an OCIRepository with no
# `spec.verify` still reconciles, so no schema, kubeconform pass or deploy notices.
# Every RED case below isolates ONE conjunct and asserts the guard refuses it BY
# NAME — the OCIRepository and the specific defect appear in the message — rather
# than merely exiting non-zero for some other reason (a floor miss, a stale
# exemption, an unreadable file). The floor, the exemption list and discovery
# depth each get their own case, and the real tree is run last as the wiring
# check.
#
# THE SEAMS: OCI_VERIFY_SCAN_ROOT points the guard at a synthetic tree built here,
# OCI_VERIFY_EXEMPTIONS_FILE replaces the built-in exemption list, and
# OCI_VERIFY_EXPECTED_MIN lowers the floor so a one-file fixture can be judged on
# its own defect.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GUARD="$REPO_ROOT/scripts/guard-oci-repository-verify.sh"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

[ -x "$GUARD" ] || { fail "guard not executable at $GUARD"; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

readonly ISSUER="'^https://token\\.actions\\.githubusercontent\\.com\$'"
readonly SUBJECT="'^https://github\\.com/devantler-tech/actions/\\.github/workflows/publish-app\\.yaml@[0-9a-f]{40}\$'"

# write_repo <dir> <name> <url> <verify-block-or-empty> — one OCIRepository manifest.
# The verify block is appended verbatim under spec:, so a case controls exactly it.
write_repo() {
  local dir="$1" name="$2" url="$3" verify="$4"
  mkdir -p "$dir"
  {
    printf 'apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  interval: 10m\n  url: %s\n  ref:\n    semver: ">=1.0.0"\n' "$name" "$name" "$url"
    [ -z "$verify" ] || printf '%s\n' "$verify"
  } >"$dir/oci-repository.yaml"
}

good_verify() {
  printf '  verify:\n    provider: cosign\n    matchOIDCIdentity:\n      - issuer: %s\n        subject: %s' "$ISSUER" "$SUBJECT"
}

# fresh_tree — a new scan root holding one GOOD in-scope OCIRepository, so every case
# starts from a tree the guard accepts and varies exactly one thing.
fresh_tree() {
  local root="$WORK/tree-$1"
  rm -rf "$root"
  write_repo "$root/apps/good" good 'oci://ghcr.io/devantler-tech/good/manifests' "$(good_verify)"
  printf '%s' "$root"
}

# run_guard <root> [exemptions-file] — runs the guard with the floor lowered to 1 and an
# EMPTY exemption list unless one is supplied; stdout+stderr captured in $out.
out=""
run_guard() {
  local root="$1" exemptions="${2:-$WORK/no-exemptions.tsv}"
  : >"$WORK/no-exemptions.tsv"
  if out="$(OCI_VERIFY_SCAN_ROOT="$root" OCI_VERIFY_EXEMPTIONS_FILE="$exemptions" OCI_VERIFY_EXPECTED_MIN=1 "$GUARD" 2>&1)"; then
    return 0
  fi
  return 1
}

# expect_refused <case> <root> <must-name> [exemptions-file]
expect_refused() {
  local name="$1" root="$2" needle="$3" exemptions="${4:-}"
  if run_guard "$root" ${exemptions:+"$exemptions"}; then
    fail "$name: expected the guard to REFUSE, it passed: $out"
    return
  fi
  if ! printf '%s' "$out" | grep -qF -- "$needle"; then
    fail "$name: refused, but not for the right reason (wanted '$needle'): $out"
    return
  fi
  pass "$name"
}

expect_accepted() {
  local name="$1" root="$2" exemptions="${3:-}"
  if run_guard "$root" ${exemptions:+"$exemptions"}; then
    pass "$name"
  else
    fail "$name: expected the guard to ACCEPT, it refused: $out"
  fi
}

# --- GREEN: the baseline fixture is accepted, so every RED below is attributable ---
root="$(fresh_tree green)"
expect_accepted 'a verified in-scope OCIRepository with one identity is accepted' "$root"

# --- RED: no spec.verify at all (AC1) ---
root="$(fresh_tree noverify)"
write_repo "$root/apps/bare" bare 'oci://ghcr.io/devantler-tech/bare/manifests' ''
expect_refused 'no spec.verify is refused by name' "$root" 'OCIRepository bare (oci://ghcr.io/devantler-tech/bare/manifests) has NO spec.verify'

# --- RED: verify with zero identity entries (AC2) ---
root="$(fresh_tree zero)"
write_repo "$root/apps/zero" zero 'oci://ghcr.io/devantler-tech/zero/manifests' \
  "$(printf '  verify:\n    provider: cosign\n    matchOIDCIdentity: []')"
expect_refused 'zero matchOIDCIdentity entries is refused' "$root" 'OCIRepository zero (oci://ghcr.io/devantler-tech/zero/manifests) has ZERO matchOIDCIdentity entries'

# --- RED: verify with no matchOIDCIdentity key at all ---
root="$(fresh_tree nokey)"
write_repo "$root/apps/nokey" nokey 'oci://ghcr.io/devantler-tech/nokey/manifests' \
  "$(printf '  verify:\n    provider: cosign')"
expect_refused 'a verify block without matchOIDCIdentity is refused' "$root" 'OCIRepository nokey (oci://ghcr.io/devantler-tech/nokey/manifests) has no matchOIDCIdentity sequence'

# --- RED: two identity entries (Flux ORs them) ---
root="$(fresh_tree two)"
write_repo "$root/apps/two" two 'oci://ghcr.io/devantler-tech/two/manifests' \
  "$(good_verify; printf '\n      - issuer: %s\n        subject: %s' "'.*'" "'.*'")"
expect_refused 'a second identity entry is refused as a widening' "$root" 'OCIRepository two (oci://ghcr.io/devantler-tech/two/manifests) has 2 matchOIDCIdentity entries'

# --- RED: a provider other than cosign ---
root="$(fresh_tree notation)"
write_repo "$root/apps/notation" notation 'oci://ghcr.io/devantler-tech/notation/manifests' \
  "$(printf '  verify:\n    provider: notation\n    matchOIDCIdentity:\n      - issuer: %s\n        subject: %s' "$ISSUER" "$SUBJECT")"
expect_refused 'a non-cosign provider is refused' "$root" "OCIRepository notation (oci://ghcr.io/devantler-tech/notation/manifests) verifies with provider 'notation'"

# --- RED: an entry with an empty subject ---
root="$(fresh_tree nosubject)"
write_repo "$root/apps/nosubject" nosubject 'oci://ghcr.io/devantler-tech/nosubject/manifests' \
  "$(printf '  verify:\n    provider: cosign\n    matchOIDCIdentity:\n      - issuer: %s' "$ISSUER")"
expect_refused 'an identity entry without a subject is refused' "$root" 'OCIRepository nosubject (oci://ghcr.io/devantler-tech/nosubject/manifests) has a matchOIDCIdentity entry missing a non-empty issuer or subject'

# --- RED: discovery at depth — an unverified OCIRepository nested as a template ---
root="$(fresh_tree nested)"
mkdir -p "$root/rgd"
cat >"$root/rgd/resource-graph-definition.yaml" <<'EOF'
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: tenant
spec:
  resources:
    - id: ociRepository
      template:
        apiVersion: source.toolkit.fluxcd.io/v1
        kind: OCIRepository
        metadata:
          name: ${schema.spec.name}
        spec:
          url: oci://ghcr.io/devantler-tech/${schema.spec.name}/manifests
          ref:
            semver: ">=1.0.0"
EOF
# The `${schema.spec.name}` below is the kro template placeholder, quoted verbatim — it
# must NOT expand, which is exactly what single quotes guarantee.
# shellcheck disable=SC2016
expect_refused 'an unverified OCIRepository nested inside a template is discovered' "$root" 'OCIRepository ${schema.spec.name} (oci://ghcr.io/devantler-tech/${schema.spec.name}/manifests) has NO spec.verify'

# --- RED: discovery across documents — the second document of a multi-doc file ---
root="$(fresh_tree multidoc)"
{
  printf -- '---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: second\n---\n'
  printf 'apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nmetadata:\n  name: second\nspec:\n  url: oci://ghcr.io/devantler-tech/second/manifests\n'
} >"$root/apps/second.yaml"
expect_refused 'an unverified OCIRepository in a later YAML document is discovered' "$root" 'OCIRepository second (oci://ghcr.io/devantler-tech/second/manifests) has NO spec.verify'

# --- CONTROL: an OCIRepository outside the devantler-tech prefix is out of scope ---
root="$(fresh_tree thirdparty)"
write_repo "$root/apps/flux" flux 'oci://ghcr.io/fluxcd/flux-manifests' ''
expect_accepted 'a third-party OCIRepository without verify is out of scope' "$root"

# --- CONTROL: the scan root is honoured — the same defect outside it is not seen ---
root="$(fresh_tree outside)"
write_repo "$WORK/elsewhere-outside" outside 'oci://ghcr.io/devantler-tech/outside/manifests' ''
expect_accepted 'a defect outside the scan root is not attributed to the tree' "$root"

# --- EXEMPTIONS: an exempt URL without verify passes; the row must be reasoned ---
root="$(fresh_tree exempt)"
write_repo "$root/apps/unsigned" unsigned 'oci://ghcr.io/devantler-tech/charts/unsigned' ''
printf 'oci://ghcr.io/devantler-tech/charts/unsigned\tpublished unsigned; tracked by example#1\n' >"$WORK/exempt.tsv"
expect_accepted 'an exempt unverified OCIRepository is admitted' "$root" "$WORK/exempt.tsv"

# --- RED: the exemption must be LOAD-BEARING — the same tree without the row is refused ---
expect_refused 'the same tree with no exemption row is refused' "$root" 'OCIRepository unsigned (oci://ghcr.io/devantler-tech/charts/unsigned) has NO spec.verify'

# --- RED: an exemption row with no reason is refused ---
printf 'oci://ghcr.io/devantler-tech/charts/unsigned\n' >"$WORK/exempt-noreason.tsv"
expect_refused 'an exemption row without a reason is refused' "$root" 'carries no reason' "$WORK/exempt-noreason.tsv"

# --- RED: a STALE exemption (no OCIRepository carries the URL) is refused ---
root="$(fresh_tree stale)"
printf 'oci://ghcr.io/devantler-tech/charts/gone\tretired long ago\n' >"$WORK/stale.tsv"
expect_refused 'an exemption naming no OCIRepository in the tree is STALE' "$root" 'exemption for oci://ghcr.io/devantler-tech/charts/gone is STALE' "$WORK/stale.tsv"

# --- RED: an exempt OCIRepository that GAINED verify makes the row stale ---
root="$(fresh_tree gained)"
write_repo "$root/apps/unsigned" unsigned 'oci://ghcr.io/devantler-tech/charts/unsigned' "$(good_verify)"
expect_refused 'an exempt OCIRepository that now verifies reports its exemption as stale' "$root" 'is exempt from verification but carries spec.verify' "$WORK/exempt.tsv"

# --- RED: the floor — an empty in-scope result is a claim about the scan ---
root="$WORK/tree-floor"
rm -rf "$root"
mkdir -p "$root"
write_repo "$root/apps/flux" flux 'oci://ghcr.io/fluxcd/flux-manifests' "$(good_verify)"
expect_refused 'zero in-scope OCIRepositories fails the floor' "$root" 'found 0 in-scope OCIRepositor(y/ies)'

# --- RED: an unreadable file is UNKNOWN, never clean ---
root="$(fresh_tree unreadable)"
printf 'kind: OCIRepository\nspec: [unterminated\n' >"$root/apps/broken.yaml"
expect_refused 'a file yq cannot parse is refused as unknown' "$root" 'yq could not read this file'

# --- WIRING: the real tree passes with the built-in exemptions and floor ---
if out="$("$GUARD" 2>&1)"; then
  pass "the real tree passes: $out"
else
  fail "the real tree is refused: $out"
fi

# --- WIRING: ci.yaml runs the guard unconditionally in the validate job ---
if grep -qF './scripts/guard-oci-repository-verify.sh' "$REPO_ROOT/.github/workflows/ci.yaml"; then
  pass 'ci.yaml invokes the guard'
else
  fail 'ci.yaml does not invoke scripts/guard-oci-repository-verify.sh'
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d failure(s)\n' "$failures" >&2
  exit 1
fi
printf '\nall cases passed\n'
