#!/usr/bin/env bash
#
# Fail when a kustomization under the given root pulls content over the NETWORK at
# render time.
#
# THE RULE THIS ENFORCES: every `resources:` / `components:` / `bases:` entry must
# resolve to a path inside this repository, and no `helmCharts:` entry may name a
# remote `repo:`. A prod render must resolve entirely from reviewed, committed bytes.
#
# Why this matters (#3196). Two entries in the prod controllers render fetch
# third-party manifests at render time, one of them from Cloudflare's `trunk`
# BRANCH. Three consequences, none of which any existing check sees:
#
#   1. The render is not reproducible. `trunk` can move between two renders of the
#      same commit, so one commit yields two different rendered surfaces with
#      nothing in this repository having changed.
#   2. Unreviewed third-party content reaches the prod AUTHORIZATION surface. The
#      cert-approver bundle contributes ServiceAccount / ClusterRole /
#      (Cluster)RoleBinding documents to the very surface the EKS CI role policy
#      validator exists to pin. A `trunk` move lands with no PR and no digest.
#   3. A required security gate becomes hostage to a third-party CDN. This was
#      found because `raw.githubusercontent.com` returned HTTP 429 and the render
#      aborted, taking four authorization tests down with it on clean `main`.
#
# The repository already has the mechanism for doing this properly:
# `scripts/update-vendored-operators.sh` vendors pinned upstream bundles with
# SHA-256 verification and CI re-validates the committed bytes. These entries are
# the outliers, not a new problem. This guard stops a THIRD one appearing while
# they are being vendored.
#
# 🔴 WHY THIS SCANS THE TREE RATHER THAN WALKING FROM THE RENDER ROOTS.
#
# The natural reading of "reachable from a prod render path" is a graph walk from
# each render root. That form has a fail-open this one does not: the guard is only
# as complete as its list of roots, so a render root added later — or renamed — is
# simply not walked, and its remote entry is reported as clean. The failure is
# silent and looks exactly like a healthy tree.
#
# Scanning every kustomization under the root is a strict SUPERSET of reachability,
# so it cannot miss an offender for that reason. Its cost is the opposite error: it
# would refuse a kustomization that is genuinely unreachable from any render and
# deliberately remote. Measured over this repository (2026-08-29): 116
# kustomizations under `k8s`, exactly 3 remote entries, all of them in the two
# files #3196 names — so the superset costs nothing today. If a legitimately
# unreachable remote ever appears, narrowing THIS guard is a reviewed change with
# the reasoning recorded, which is the outcome we want; a missed root is not.
#
# 🔴 CLASSIFY LOCAL-FIRST, AND NEVER BY SCHEME ALONE.
#
# A `*://*` test is the obvious detector and it is a fail-open, because kustomize
# accepts SCHEMELESS remote shorthand: `github.com/org/repo//path?ref=v1` is a
# remote base and contains no `://` at all. So a scheme test reports it clean.
#
# But the repair cannot simply be "also match a bare hostname", because this is a
# Kubernetes repository where DIRECTORIES ARE ROUTINELY NAMED LIKE HOSTS — an API
# group such as `cert-manager.k8s.cloudflare.com` is a perfectly ordinary local
# directory name, and a host-shaped pattern refuses it. That is a false refusal on
# a correct file.
#
# Both are resolved by asking the FILESYSTEM first and the pattern second:
#
#   resolves to an existing path  -> LOCAL. Correct regardless of how it is spelled,
#                                    so a host-named directory can never be refused.
#   else, a recognised remote form -> REMOTE. The violation.
#   else                           -> UNDECIDABLE, exit 2.
#
# ⚠️ ANTI-VACUITY, TWICE. Finding NO kustomization under the root is exit 2, and so
# is examining ZERO entries. A selector that matched nothing is indistinguishable
# from a clean tree, and reporting it as clean is the same class of failure this
# guard exists to prevent one level down.
#
# ⚠️ AN UNDECIDABLE ENTRY IS cannot-check, NEVER clean. An entry that resolves to
# nothing on disk and matches no remote form is one this guard cannot classify —
# a dangling path, or a remote spelling not listed here. Skipping it is precisely
# how a new remote spelling would enter unnoticed, so it stops the build and asks.
#
# 🔴 A REMOTE ENTRY IS ALLOWED ONLY BY A REVIEWED, ISSUE-REFERENCED EXCEPTION ROW.
#
# Wiring this guard while the two entries #3196 names are still remote would turn
# `main` red, and the usual escape — ship the guard unwired until they are vendored
# — is exactly the defect that keeps a working detector from ever running (#2757).
# So the two known entries carry a row in
# `scripts/render-remote-resource-exceptions.tsv`, and a THIRD one fails the build
# today, which is child (1)'s whole point.
#
# A row is a tracked disposition, never an approval: it MUST name an issue, and a
# row whose URL is no longer referenced is itself a VIOLATION. That is what stops
# the exceptions list becoming the quiet place remote fetches accumulate — a
# growing exception set is a smell, not progress.
#
# Exit codes:
#   0  every entry resolves inside the repository
#   1  at least one entry is remote and unexcepted, or an exception row is stale
#   2  cannot check: bad usage, missing root, missing yq, unparseable YAML,
#      an entry that cannot be classified, or an anti-vacuity failure

set -uo pipefail

die() {
  printf 'guard-render-remote-resources: %s\n' "$*" >&2
  exit 2
}

[ "$#" -eq 1 ] || die "usage: $0 <root-directory>"
root="$1"
[ -d "$root" ] || die "root '$root' is not a directory"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"

exceptions_file="${RENDER_REMOTE_EXCEPTIONS:-$(dirname "$0")/render-remote-resource-exceptions.tsv}"
[ -f "$exceptions_file" ] ||
  die "exceptions file '$exceptions_file' not found — refusing to run without the reviewed disposition list"

# Parsed into two parallel newline-delimited lists. A malformed row is exit 2: a row
# this guard cannot read is a disposition nobody can audit, and silently ignoring it
# would let a remote entry pass on a row that says nothing.
excepted_urls=""
exception_lineno=0
while IFS= read -r row || [ -n "$row" ]; do
  exception_lineno=$((exception_lineno + 1))
  case $row in '' | '#'*) continue ;; esac
  url=$(printf '%s' "$row" | cut -f1)
  issue=$(printf '%s' "$row" | cut -f2)
  reason=$(printf '%s' "$row" | cut -f3)
  [ -n "$url" ] ||
    die "$exceptions_file:$exception_lineno: row has no URL"
  printf '%s' "$issue" | grep -Eq '^#[0-9]+$' ||
    die "$exceptions_file:$exception_lineno: '$url' names no tracking issue (column 2 must be #<number>, got '$issue') — an exception without an owner is not reviewable"
  [ -n "$reason" ] ||
    die "$exceptions_file:$exception_lineno: '$url' carries no reason (column 3) — an exception without a stated reason is not reviewable"
  excepted_urls="$excepted_urls$url"$'\n'
done < "$exceptions_file"

# ⚠️ AN EMPTY EXCEPTIONS FILE IS THE GOAL STATE, NOT A FAILURE.
#
# The first draft of this guard died here on a file with no rows, by analogy with
# the anti-vacuity checks above. That analogy is wrong and the check was a latent
# outage: once #3196's children vendor the two entries, the correct file has zero
# rows — so the guard would have failed CI at the exact moment the defect it exists
# for was fixed. Anti-vacuity protects a SELECTOR that silently matched nothing; an
# empty disposition list is a measured fact about the tree, and a good one.

# 🔴 THE QUOTES AROUND "$1" ARE LOAD-BEARING, NOT STYLE.
#
# The value tested here comes from a kustomization, and it sits on the PATTERN side
# of this `case`. Quoted, it is matched LITERALLY, which is what makes this safe.
# Unquoted — the obvious "simplification" — a metacharacter in the entry becomes an
# active glob, so `resources: [ "https://*" ]` would match the body of the first
# excepted row, be counted as a tracked exception, and pass. That is a one-line
# bypass of the entire guard, and it would also mark that row used, silencing the
# stale-row check that would otherwise fire.
#
# Probed directly (2026-08-29): quoted → no match; the same expansion unquoted →
# match. The test suite pins the behaviour so the quotes cannot be dropped silently.
is_excepted() { # <url>
  case $'\n'"$excepted_urls" in *$'\n'"$1"$'\n'*) return 0 ;; esac
  return 1
}

used_urls=""

# `find` exits non-zero when it cannot traverse part of the tree. Reporting on a
# partially-read tree is the same "could not read it" case as a parse failure, so it
# is cannot-check rather than a verdict on what happened to be readable.
candidates="$(find "$root" -name kustomization.yaml -type f -print 2>/dev/null)"
find_status=$?
[ "$find_status" -eq 0 ] ||
  die "could not enumerate '$root' (find exit $find_status) — refusing to report a tree it could not fully read"

# Collapse `.`/`..` textually rather than depending on realpath, so an entry that
# names nothing still yields a stable path to test and report.
canonicalize() { # <dir> <entry>
  local base=$1 entry=$2 combined
  case $entry in
    /*) combined=$entry ;;
    *) combined="$base/$entry" ;;
  esac
  printf '%s' "$combined" | awk -F/ -v abs="${combined:0:1}" '{
    n = 0
    for (i = 1; i <= NF; i++) {
      if ($i == "" || $i == ".") continue
      if ($i == "..") { if (n > 0) n--; continue }
      parts[++n] = $i
    }
    out = ""
    for (i = 1; i <= n; i++) out = out "/" parts[i]
    if (abs == "/") print out; else print substr(out, 2)
  }'
}

# The remote spellings kustomize accepts. Consulted ONLY after the filesystem has
# said the entry does not resolve locally, so a host-named local directory is never
# tested against it.
is_remote_form() { # <entry>
  case $1 in
    *"://"*) return 0 ;;      # https://, http://, git::https://, ssh://
    git@*) return 0 ;;        # scp-style SSH
    git::*) return 0 ;;       # kustomize's explicit git prefix
    gh:*) return 0 ;;         # kustomize's github shorthand
  esac
  # Schemeless host shorthand: `github.com/org/repo//path?ref=v1`. Requires a dot
  # in the first segment and a following slash, which no relative path in this tree
  # produces once the filesystem check above has already failed.
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+/'
}

examined=0
kustomizations=0
violations=0
excepted=0

# A here-string, not a pipe: a piped `while` runs in a subshell, so every counter it
# increments is discarded and the guard reports 0 violations over a dirty tree.
while IFS= read -r file; do
  [ -n "$file" ] || continue
  kustomizations=$((kustomizations + 1))
  dir=$(dirname "$file")

  # `bases:` is deprecated but still honoured by kustomize, so omitting it would
  # leave a supported field through which a remote could enter unseen.
  if ! entries="$(yq eval -r '(.resources // []) + (.components // []) + (.bases // []) | .[]' "$file" 2>/dev/null)"; then
    die "could not parse '$file' — refusing to report a tree it could not read"
  fi

  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    examined=$((examined + 1))
    target=$(canonicalize "$dir" "$entry")
    # LOCAL FIRST. A path that exists is correct however it is spelled.
    if [ -e "$target" ]; then
      continue
    fi
    if is_remote_form "$entry"; then
      if is_excepted "$entry"; then
        used_urls="$used_urls$entry"$'\n'
        excepted=$((excepted + 1))
        continue
      fi
      violations=$((violations + 1))
      printf 'VIOLATION %s: remote resource entry %s\n' "$file" "$entry" >&2
      continue
    fi
    die "'$file': entry '$entry' resolves to nothing under this repository and matches no known remote form — cannot classify it, so this tree is unverified"
  done <<<"$entries"

  # A kustomize-native Helm fetch is the same render-time network dependency wearing
  # a different field name, so closing `resources:` alone would leave the obvious
  # bypass open.
  if ! charts="$(yq eval -r '(.helmCharts // []) | .[] | (.repo // "")' "$file" 2>/dev/null)"; then
    die "could not parse '$file' — refusing to report a tree it could not read"
  fi
  while IFS= read -r repo; do
    [ -n "$repo" ] || continue
    examined=$((examined + 1))
    if is_excepted "$repo"; then
      used_urls="$used_urls$repo"$'\n'
      excepted=$((excepted + 1))
      continue
    fi
    violations=$((violations + 1))
    printf 'VIOLATION %s: helmCharts entry fetches from remote repo %s\n' "$file" "$repo" >&2
  done <<<"$charts"
done <<<"$candidates"

if [ "$kustomizations" -eq 0 ]; then
  die "no kustomization.yaml found under '$root' — selector matched nothing, so nothing was verified"
fi

if [ "$examined" -eq 0 ]; then
  die "found $kustomizations kustomization(s) under '$root' but not one resource entry — refusing to report a tree it did not actually read"
fi

# A row whose URL is no longer referenced has outlived its subject. Leaving it would
# let the exception list drift into a standing permission for URLs nobody uses — and
# on the day the same URL returns, it would be pre-approved with no review.
while IFS= read -r url; do
  [ -n "$url" ] || continue
  case $'\n'"$used_urls" in
    *$'\n'"$url"$'\n'*) ;;
    *)
      violations=$((violations + 1))
      printf 'VIOLATION %s: exception row for %s is stale — no kustomization under %s references it any more; delete the row\n' \
        "$exceptions_file" "$url" "$root" >&2
      ;;
  esac
done <<<"$excepted_urls"

if [ "$violations" -gt 0 ]; then
  cat >&2 <<'FIX'

A render must resolve entirely from bytes committed to this repository.

Vendor the upstream content instead of fetching it, using the mechanism this
repository already has:

  scripts/update-vendored-operators.sh          # pin + SHA-256 verify + commit
  scripts/update-vendored-operators.sh --validate-committed   # what CI re-checks

Then replace the URL with the vendored path. If a remote is genuinely required,
that is a reviewed decision and this guard is where it gets recorded (#3196).
FIX
  printf 'guard-render-remote-resources: %d remote entr(y/ies) across %d kustomization(s)\n' \
    "$violations" "$kustomizations" >&2
  exit 1
fi

printf 'guard-render-remote-resources: OK — %d entr(y/ies) across %d kustomization(s); %d in-repo, %d tracked exception(s)\n' \
  "$examined" "$kustomizations" "$((examined - excepted))" "$excepted"
exit 0
