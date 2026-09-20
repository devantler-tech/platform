#!/usr/bin/env bash
#
# Fail when a namespace is opted into the baseline-context mutation while sitting
# inside the namespace set Kubescape does not scan, without saying so at the opt-in.
#
# The per-namespace rollout in #3239 has a documented read-back step: opt the namespace
# in, then confirm against the posture signal that C-0211's residual dropped. For a
# namespace the scanner never looks at, that step silently cannot be performed — and it
# does not fail, it comes back clean. A rollout "validated" that way produces a
# confident reading that is an artifact of the namespace being out of scope.
#
# Today exactly one opted-in namespace is in that position. `kubescape` is both labelled
# `pod-security.devantler.tech/baseline-context: enabled` and named in the operator's own
# `excludeNamespaces`, and its opt-in comment reads exactly like the three scanned
# namespaces' comments. The caveat does exist — in `helm-release.yaml`, about a different
# change — so a reader following the established opt-in pattern never meets it.
#
# 🔴 THIS IS NOT HYPOTHETICAL DRIFT. #3919 was written on the belief that labelling
# `kube-system` would change what Kubescape reports about nine control-plane pods. It
# would not: `kube-system` is excluded, so there is no verdict to change. Settling that
# took a live-cluster measurement, because nothing in the tree said which namespaces are
# scanned. #3923 put the excluded set in the tree; this guard makes the consequence
# checkable at the place the decision is actually made.
#
# 🔴 FAIL CLOSED. Both inputs are required non-empty before any comparison. An empty
# opted-in set compared against an empty excluded set is the failure mode that makes a
# guard look green forever — and it is reachable here by a single typo in either yq
# path, since both would then simply return nothing. Anything unreadable is exit 2.
#
# The intersection itself may legitimately be empty: that means no opted-in namespace is
# unscanned, which is a real pass rather than a vacuous one, because both inputs were
# proven non-empty first.
#
# Exit codes:
#   0  no opted-in namespace is unscanned, or each that is declares the caveat
#   1  an unscanned namespace is opted in without the declaration — it is named
#   2  cannot check: bad usage, a missing or unparseable file, a moved expression, or
#      either input coming back empty

set -uo pipefail

readonly MARKER='kubescape-scan-scope: excluded'
readonly LABEL='pod-security.devantler.tech/baseline-context'

die() {
  printf 'guard-baseline-context-optin-scan-scope: %s\n' "$*" >&2
  exit 2
}

[ "$#" -le 2 ] || die "usage: $0 [<kubescape-helm-release> <k8s-dir>]"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" ||
  die "could not resolve the repository root"

helm_release="${1:-$repo_root/k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml}"
k8s_dir="${2:-$repo_root/k8s}"

[ -f "$helm_release" ] || die "kubescape HelmRelease '$helm_release' not found"
[ -d "$k8s_dir" ] || die "k8s directory '$k8s_dir' not found"

# --- input 1: the namespaces the operator does not scan -------------------------------
# A single comma-separated string, so a moved path yields `null` rather than an error;
# that is why the emptiness assertion below is not optional.
excluded_raw="$(yq -r '.spec.values.excludeNamespaces' "$helm_release" 2>/dev/null)" ||
  die "could not read .spec.values.excludeNamespaces from '$helm_release' — unparseable YAML"
[ -n "$excluded_raw" ] && [ "$excluded_raw" != "null" ] ||
  die "excludeNamespaces came back EMPTY in '$helm_release'; refusing to compare against an empty set, which would pass vacuously"

excluded="$(printf '%s\n' "$excluded_raw" | tr ',' '\n' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | sed '/^$/d' | sort -u)"
[ -n "$excluded" ] ||
  die "excludeNamespaces in '$helm_release' held no namespace names after splitting on commas"

# --- input 2: the namespaces opted into the baseline-context mutation -----------------
# Narrow with grep on the label KEY (robust to how the value is quoted), then let yq
# decide, so a file that merely mentions the label in prose is not counted. Both YAML
# suffixes are swept: k8s/ uses .yaml throughout today, but the repository does use .yml
# elsewhere, and a namespace manifest written that way would otherwise be invisible here
# — a silent miss rather than a failure.
candidates="$(grep -rl --include='*.yaml' --include='*.yml' -- "$LABEL" "$k8s_dir" 2>/dev/null | sort -u)"
[ -n "$candidates" ] ||
  die "no file under '$k8s_dir' mentions '$LABEL'; refusing to report a clean tree from an empty opted-in set"

optin_pairs=""
while IFS= read -r file; do
  [ -n "$file" ] || continue
  names="$(yq -r "select(.kind == \"Namespace\" and .metadata.labels.\"${LABEL}\" == \"enabled\") | .metadata.name" "$file" 2>/dev/null)" ||
    die "could not evaluate the opt-in label in '$file' — unparseable YAML, or the expression no longer matches"
  while IFS= read -r ns; do
    case "$ns" in '' | null | ---) continue ;; esac
    optin_pairs="${optin_pairs}${ns} ${file}"$'\n'
  done <<EOF
$names
EOF
done <<EOF
$candidates
EOF

[ -n "$optin_pairs" ] ||
  die "no Namespace under '$k8s_dir' carries '$LABEL: enabled'; the rollout has always had at least one, so this is treated as a broken read rather than a clean tree"

# --- the comparison -------------------------------------------------------------------
status=0
unscanned_count=0

while IFS=' ' read -r ns file; do
  [ -n "$ns" ] || continue
  printf '%s\n' "$excluded" | grep -qxF -- "$ns" || continue
  unscanned_count=$((unscanned_count + 1))
  if grep -qF -- "$MARKER" "$file"; then
    printf 'guard-baseline-context-optin-scan-scope: %s is opted in and unscanned, and declares it\n' "$ns"
    continue
  fi
  status=1
  printf 'guard-baseline-context-optin-scan-scope: %s is opted into the baseline-context mutation but is inside the operator'"'"'s excludeNamespaces, so no scan can validate that opt-in\n' "$ns" >&2
  printf '  opt-in declared at: %s\n' "$file" >&2
  printf '  excluded by       : %s (.spec.values.excludeNamespaces)\n' "$helm_release" >&2
  printf '  fix: state the caveat AT the opt-in — add a comment line containing exactly\n' >&2
  printf '         %s\n' "$MARKER" >&2
  printf '       to %s, next to the label, so a reader following the opt-in pattern meets it there.\n' "$file" >&2
  printf '       Do NOT exempt the namespace instead: the opt-in is fine, the silent read-back is not.\n' >&2
done <<EOF
$optin_pairs
EOF

if [ "$status" -ne 0 ]; then
  exit 1
fi

printf 'guard-baseline-context-optin-scan-scope: OK — %d opted-in namespace(s) inside the unscanned set, each declaring it\n' \
  "$unscanned_count"
