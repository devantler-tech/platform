#!/usr/bin/env bash
#
# Fail when the namespaces Kubescape does not scan differ from the reviewed list in
# scripts/kubescape-unscanned-namespaces.tsv (#3215).
#
# A namespace in the operator's `excludeNamespaces` produces no scan result at all, and the
# posture score cannot tell that apart from a namespace with nothing wrong. So adding one
# silently narrows every posture claim the platform makes. This guard makes that change
# visible: the exclusion list and the reviewed list must match exactly, and every reviewed
# row must say why the namespace is not scanned.
#
# Both directions fail. A namespace excluded without a row is the silent narrowing this guard
# exists to catch. A row whose namespace is no longer excluded is a stale claim that a scanned
# namespace is unscanned, which would mislead the next reader in the opposite direction.
#
# FAIL CLOSED. Both inputs must be non-empty before they are compared: a moved yq path or an
# emptied list would otherwise compare two empty sets and pass forever.
#
# Exit codes:
#   0  the exclusion list and the reviewed list match, and every row has a reason
#   1  they differ, or a row has no reason; each problem is named
#   2  cannot check: bad usage, a missing or unparseable file, or an empty input

set -uo pipefail

die() {
  printf 'guard-kubescape-scan-scope: %s\n' "$*" >&2
  exit 2
}

[ "$#" -le 2 ] || die "usage: $0 [<kubescape-helm-release> <reviewed-list>]"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" ||
  die "could not resolve the repository root"

helm_release="${1:-$repo_root/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml}"
reviewed="${2:-$repo_root/scripts/kubescape-unscanned-namespaces.tsv}"

[ -f "$helm_release" ] || die "kubescape HelmRelease '$helm_release' not found"
[ -f "$reviewed" ] || die "reviewed list '$reviewed' not found"

# --- input 1: what the operator is told not to scan -------------------------------------
excluded_raw="$(yq -r '.spec.values.excludeNamespaces' "$helm_release" 2>/dev/null)" ||
  die "could not read .spec.values.excludeNamespaces from '$helm_release' — unparseable YAML"
[ -n "$excluded_raw" ] && [ "$excluded_raw" != "null" ] ||
  die "excludeNamespaces came back EMPTY in '$helm_release'; refusing to compare against an empty set"

excluded="$(printf '%s\n' "$excluded_raw" | tr ',' '\n' |
  sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | sed '/^$/d' | sort -u)"
[ -n "$excluded" ] ||
  die "excludeNamespaces in '$helm_release' held no namespace names after splitting on commas"

# --- input 2: the reviewed rows ---------------------------------------------------------
failures=0
listed=""
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | '#'*) continue ;; esac
  ns="${line%%$'\t'*}"
  if [ "$ns" = "$line" ]; then
    reason=""
  else
    reason="${line#*$'\t'}"
  fi
  reason="$(printf '%s' "$reason" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
  if [ -z "$ns" ]; then
    printf 'guard-kubescape-scan-scope: a row in %s has an empty namespace\n' "$reviewed" >&2
    failures=$((failures + 1))
    continue
  fi
  if [ -z "$reason" ]; then
    printf 'guard-kubescape-scan-scope: %s is listed without a reason; say why it is not scanned\n' "$ns" >&2
    failures=$((failures + 1))
  fi
  listed="${listed}${ns}"$'\n'
done <"$reviewed"

listed_sorted="$(printf '%s' "$listed" | sed '/^$/d' | sort)"
[ -n "$listed_sorted" ] ||
  die "no rows in '$reviewed'; refusing to compare against an empty reviewed list"

duplicates="$(printf '%s\n' "$listed_sorted" | uniq -d)"
if [ -n "$duplicates" ]; then
  while IFS= read -r ns; do
    printf 'guard-kubescape-scan-scope: %s is listed more than once in %s\n' "$ns" "$reviewed" >&2
    failures=$((failures + 1))
  done <<EOF
$duplicates
EOF
fi
listed_unique="$(printf '%s\n' "$listed_sorted" | sort -u)"

# --- compare, both directions -------------------------------------------------------------
unreviewed="$(comm -23 <(printf '%s\n' "$excluded") <(printf '%s\n' "$listed_unique"))"
stale="$(comm -13 <(printf '%s\n' "$excluded") <(printf '%s\n' "$listed_unique"))"

if [ -n "$unreviewed" ]; then
  while IFS= read -r ns; do
    printf 'guard-kubescape-scan-scope: %s is excluded from scanning but has no row in %s; add one saying why, or scan it\n' \
      "$ns" "$reviewed" >&2
    failures=$((failures + 1))
  done <<EOF
$unreviewed
EOF
fi
if [ -n "$stale" ]; then
  while IFS= read -r ns; do
    printf 'guard-kubescape-scan-scope: %s is listed as unscanned but is not in excludeNamespaces; remove its row\n' \
      "$ns" >&2
    failures=$((failures + 1))
  done <<EOF
$stale
EOF
fi

[ "$failures" -eq 0 ] || exit 1

printf 'guard-kubescape-scan-scope: OK — %d unscanned namespace(s), each reviewed with a reason\n' \
  "$(printf '%s\n' "$excluded" | wc -l | tr -d ' ')"
