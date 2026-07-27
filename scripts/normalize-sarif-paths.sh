#!/usr/bin/env bash
# Make a SARIF file's result paths repo-root-relative, so Code Scanning alerts
# anchor to real files.
#
# WHY THIS EXISTS. Kubescape reports paths relative to the directory it scanned,
# not to the repository root: a finding in k8s/bases/apps/umami/cron-job.yaml
# arrives as bases/apps/umami/cron-job.yaml. Code Scanning resolves an alert's
# location against the repository root, so an un-prefixed uri produces an alert
# pointing at a path that does not exist — the alert is created, looks correct in
# the API, and is unclickable. Measured on devantler-tech/platform#2830: the
# upload step was green, four alerts existed, and every one of them was dead.
#
# The same scan run locally emitted the k8s/ prefix, so the shape is not stable
# across ksail versions. This script therefore does not assume either shape: it
# resolves each uri against the working tree and only rewrites the ones that
# need it, which makes it correct before and after any upstream change.
#
# CONTRACT
#   - A uri that already resolves to a file is left ALONE (idempotent, and a
#     re-run or a future ksail that emits root-relative paths is a no-op).
#   - A uri that does not resolve, but does resolve under <prefix>/, is
#     rewritten to <prefix>/<uri>.
#   - A uri that resolves under NEITHER is a hard error naming the uri, because
#     silently uploading it recreates exactly the dead-alert bug this prevents.
#   - An EMPTY uri is dropped with a warning, not an error: Kubescape emits one
#     for cluster-RBAC controls that have no backing manifest (C-0002), and there
#     is no file for such a finding to anchor to.
#
# Usage: normalize-sarif-paths.sh <sarif-file> <prefix> [repo-root]
set -euo pipefail

SARIF="${1:?usage: normalize-sarif-paths.sh <sarif-file> <prefix> [repo-root]}"
PREFIX="${2:?usage: normalize-sarif-paths.sh <sarif-file> <prefix> [repo-root]}"
ROOT="${3:-.}"

[ -f "$SARIF" ] || { echo "normalize-sarif-paths: no such file: $SARIF" >&2; exit 2; }
[ -d "$ROOT/$PREFIX" ] || { echo "normalize-sarif-paths: prefix dir not found: $ROOT/$PREFIX" >&2; exit 2; }

PREFIX="${PREFIX%/}"

# `while read` rather than `mapfile`, which is bash 4+ and so unavailable on the
# macOS bash 3.2 a maintainer may run this under locally.
uris=()
while IFS= read -r line; do uris+=("$line"); done < <(jq -r '
  [.runs[]?.results[]?.locations[]?.physicalLocation.artifactLocation.uri // empty] | unique[]
' "$SARIF")

rewrite_args=()
unresolved=()
kept=0
prefixed=0

for uri in "${uris[@]}"; do
  if [ -z "$uri" ]; then
    continue
  elif [ -e "$ROOT/$uri" ]; then
    kept=$((kept + 1))
  elif [ -e "$ROOT/$PREFIX/$uri" ]; then
    prefixed=$((prefixed + 1))
    rewrite_args+=("$uri")
  else
    unresolved+=("$uri")
  fi
done

if [ ${#unresolved[@]} -gt 0 ]; then
  echo "::error::normalize-sarif-paths: these SARIF paths resolve neither at the repo root nor under '$PREFIX/':" >&2
  printf '  %s\n' "${unresolved[@]}" >&2
  echo "Uploading them would create Code Scanning alerts that point at files which do not exist." >&2
  echo "Check what path base the scanner is emitting before changing this script's prefix." >&2
  exit 1
fi

# Rewrite in one pass. Empty-uri results are dropped here too — see CONTRACT.
tmp="$(mktemp)"
jq --arg prefix "$PREFIX" --argjson rewrite "$(printf '%s\n' "${rewrite_args[@]+"${rewrite_args[@]}"}" | jq -R . | jq -s 'map(select(. != ""))')" '
  def fix(u): if (u == "") then u
              elif (u as $x | $rewrite | index($x)) then ($prefix + "/" + u)
              else u end;
  .runs |= map(
    (.results //= [])
    | .results |= map(select((.locations[0].physicalLocation.artifactLocation.uri // "") != ""))
    | .results |= map(
        .locations |= map(
          .physicalLocation.artifactLocation.uri |= fix(.)
        )
      )
  )
' "$SARIF" > "$tmp"
mv "$tmp" "$SARIF"

dropped=$(printf '%s\n' "${uris[@]+"${uris[@]}"}" | grep -c '^$' || true)
echo "normalize-sarif-paths: ${kept} already root-relative, ${prefixed} prefixed with '${PREFIX}/', ${dropped} empty-uri group(s) dropped."

# Fail-closed re-check: every surviving path must now resolve.
after=()
while IFS= read -r line; do after+=("$line"); done < <(jq -r '
  [.runs[]?.results[]?.locations[]?.physicalLocation.artifactLocation.uri // empty] | unique[]
' "$SARIF")
bad=0
for uri in "${after[@]}"; do
  [ -n "$uri" ] || continue
  [ -e "$ROOT/$uri" ] || { echo "::error::normalize-sarif-paths: still unresolved after rewrite: $uri" >&2; bad=1; }
done
[ "$bad" -eq 0 ] || exit 1
