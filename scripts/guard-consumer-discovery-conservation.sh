#!/usr/bin/env bash
# Reconcile the publish-revision report's consumer discovery against what production
# actually renders (#3332).
#
# WHY THIS EXISTS
# `report-publish-workflow-signing-revisions.sh` finds its consumers with a raw FILE SCAN:
# every `*.yaml` in the repository that carries a shared-publish-workflow subject. Production
# is what Flux RENDERS. The two agreed exactly when #3332 measured them, but nothing kept them
# agreeing. A base that production excludes, an overlay that patches a consumer's `spec.ref`
# or `spec.url`, or a consumer in a file the scan never opens (a `.yml` resource, a subject
# added by a kustomize patch) would each make the report name a selector that is not deployed,
# or miss one that is, while it exits clean.
#
# WHY A CONSERVATION CHECK AND NOT A RENDER-BASED DISCOVERY
# Discovery stays a file scan on purpose: its test suite keeps it REAL against these
# manifests, and a render-based discovery would have to reproduce Flux's path resolution to
# find anything at all. This asserts that the two sets are IDENTICAL and fails when they stop
# being, so a change on either side is caught without the report changing how it discovers.
#
# WHAT "THE PRODUCTION RENDER" IS — MEASURED, NOT ASSUMED
# Rendering k8s/clusters/prod yields NO OCIRepository documents. It renders only the Flux
# Kustomizations whose `spec.path` names the layers Flux applies; every `kind: OCIRepository`
# string in it is a `sourceRef` REFERENCE. Taking that overlay literally would discover no
# consumer at all and pass vacuously. So the roots are read from that render (never listed
# here, so a new layer is covered the day it is wired) and each one is rendered in turn. The
# consumer rows are then extracted from those renders by the SAME `consumer_rows` the scan
# uses, so the only things that can differ are WHICH documents each side sees and what their
# fields say once kustomize has applied every patch.
#
# THE COMPARED IDENTITY is exactly what `--list-consumers` emits and the report acts on: the
# source repository and the artifact (both from `spec.url`), the shared workflow (from the
# cosign subject), and the effective `spec.ref` (digest > semver > tag, `unpinned` when
# omitted). `metadata.name` is not part of it: the report never reads it, so a rename changes
# nothing the report says.
#
# WHAT A STATIC RENDER CANNOT SEE IS REFUSED, NEVER ASSUMED EQUAL
#   - a Flux-side transform on a production root (`spec.patches`, `spec.components`,
#     `spec.namePrefix`, `spec.nameSuffix`): kustomize-controller applies it after the build,
#     so `kubectl kustomize` does not show what Flux applies;
#   - production roots drawn from more than one source: each path is relative to its own
#     source, and this tree is only one of them;
#   - a nested Flux Kustomization that applies another path from that same source: only the
#     overlay's roots are rendered, so that layer would go unseen;
#   - an OCIRepository whose URL, ref or subject carries `${`: Flux post-build substitution
#     decides it at apply time;
#   - an instance of a ResourceGraphDefinition kind whose template is a shared-workflow
#     consumer: kro creates that OCIRepository inside the cluster, so neither the scan nor any
#     render has a document for it;
#   - a root that is missing or does not render, and an EMPTY consumer set on both sides —
#     an empty set compared to an empty set is agreement about nothing.
#
# USAGE
#   guard-consumer-discovery-conservation.sh [repo-root]
# The repository root defaults to this repository; the test suite points it at fixtures.
# Renders only manifests: no cluster, no network, no credentials.

set -euo pipefail

GUARD_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly GUARD_DIR
# shellcheck source=scripts/report-publish-workflow-signing-revisions.sh
source "$GUARD_DIR/report-publish-workflow-signing-revisions.sh"

refuse() {
  printf 'guard-consumer-discovery-conservation: %s\n' "$*" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || refuse 'yq is required and was not found on PATH'
command -v kubectl >/dev/null 2>&1 || refuse 'kubectl is required to render the production roots'

SCAN_ROOT="${1:-$REPO_ROOT}"
[ -d "$SCAN_ROOT" ] || refuse "repository root $SCAN_ROOT does not exist"
SCAN_ROOT="$(cd "$SCAN_ROOT" && pwd)"
readonly SCAN_ROOT
readonly K8S_DIR="$SCAN_ROOT/k8s"
readonly OVERLAY="$K8S_DIR/clusters/prod"
[ -d "$OVERLAY" ] || refuse "no production overlay at ${OVERLAY#"$SCAN_ROOT"/}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Every field is spelled `-` when empty, so consecutive tabs never collapse under `read`'s
# IFS-whitespace splitting and shift the later fields left.
readonly FLUX_KUSTOMIZATION='select(.kind == "Kustomization" and ((.apiVersion // "") | test("^kustomize\\.toolkit\\.fluxcd\\.io/")))'

# ── 1. The production roots, read from the overlay's render ───────────────────────────
if ! kubectl kustomize "$OVERLAY" >"$work/overlay.yaml" 2>"$work/overlay.err"; then
  refuse "could not render k8s/clusters/prod, so the production roots are UNKNOWN: $(tr '\n' ' ' <"$work/overlay.err")"
fi
if ! yq -N -r "$FLUX_KUSTOMIZATION"' | [
    (.metadata.name // ""),
    (.spec.path // ""),
    (.spec.sourceRef.kind // ""),
    (.spec.sourceRef.namespace // .metadata.namespace // ""),
    (.spec.sourceRef.name // ""),
    ((((.spec.patches // []) | length) + ((.spec.components // []) | length)
      + ((.spec.namePrefix // "") | length) + ((.spec.nameSuffix // "") | length)) | tostring)
  ] | map(sub("^$", "-")) | join("	")' "$work/overlay.yaml" >"$work/overlay.rows" 2>"$work/overlay.rows.err"; then
  refuse "could not read the production overlay's render: $(tr '\n' ' ' <"$work/overlay.rows.err")"
fi

roots=''
source_id=''
while IFS=$'\t' read -r name path src_kind src_ns src_name transforms; do
  [ -n "$name" ] || continue
  [ "$path" != '-' ] || refuse "production Flux Kustomization $name names no spec.path"
  if [ "$transforms" != '0' ]; then
    refuse "production Flux Kustomization $name carries spec.patches, spec.components, spec.namePrefix or spec.nameSuffix; Flux applies those after the build, so a static render does not show what it applies"
  fi
  # Each path is relative to the root of ITS source. This tree is one source; a second one
  # would make some path relative to an artifact nobody here can render.
  this_source="$src_kind/$src_ns/$src_name"
  if [ -z "$source_id" ]; then
    source_id="$this_source"
  elif [ "$this_source" != "$source_id" ]; then
    refuse "production Flux Kustomizations draw on more than one source ($source_id and $this_source); each path is relative to its own source, so this tree cannot render them all"
  fi
  # `./` is the source root itself. Stripping the prefix must not leave an EMPTY root, which
  # the loop below would skip as a blank line, dropping that layer from the comparison.
  path="${path#./}"
  roots="$roots${path:-.}
"
done <"$work/overlay.rows"
[ -n "$roots" ] || refuse 'k8s/clusters/prod renders no Flux Kustomization, so there is no production root to compare against'
src_kind="${source_id%%/*}"
src_name="${source_id##*/}"
src_ns="${source_id#*/}"
src_ns="${src_ns%/*}"

# ── 2. Render every root, refusing what a static render cannot see ───────────────────
i=0
root_files=''
root_labels=''
while IFS= read -r root; do
  [ -n "$root" ] || continue
  i=$((i + 1))
  dir="$K8S_DIR/$root"
  [ -d "$dir" ] || refuse "production root $root does not exist under k8s/, so what it applies is UNKNOWN"
  file="$work/root-$i.yaml"
  if ! kubectl kustomize "$dir" >"$file" 2>"$work/root.err"; then
    refuse "could not render production root $root, so its consumers are UNKNOWN: $(tr '\n' ' ' <"$work/root.err")"
  fi
  root_files="$root_files$file
"
  root_labels="${root_labels:+$root_labels, }$root"

  # A nested Flux Kustomization applying another path from the SAME source is a layer this
  # guard never renders. A missing namespace is treated as a match: it cannot be ruled out.
  if ! nested="$(yq -N -r "$FLUX_KUSTOMIZATION"' | [
      (.metadata.name // ""), (.spec.sourceRef.kind // ""),
      (.spec.sourceRef.namespace // .metadata.namespace // ""), (.spec.sourceRef.name // "")
    ] | map(sub("^$", "-")) | join("	")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $root: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  while IFS=$'\t' read -r n_name n_kind n_ns n_src; do
    [ -n "$n_name" ] || continue
    if [ "$n_kind" = "$src_kind" ] && [ "$n_src" = "$src_name" ] &&
      { [ "$n_ns" = "$src_ns" ] || [ "$n_ns" = '-' ]; }; then
      refuse "production root $root renders Flux Kustomization $n_name, which applies another path from the platform's own source ($source_id); that layer is not rendered here, so its consumers are UNKNOWN"
    fi
  done <<<"$nested"

  # The literal characters `${` mark a Flux post-build substitution, deliberately not
  # expanded by the shell.
  if ! substituted="$(yq -N -r 'select(.kind == "OCIRepository")
      | select(([(.spec.url // ""), (.spec.ref.tag // ""), (.spec.ref.semver // ""), (.spec.ref.digest // ""),
                 ((.spec.verify.matchOIDCIdentity // []) | map(.subject // "") | join(" "))]
                | join(" ")) | test("\\$\\{"))
      | (.metadata.namespace // "-") + "/" + (.metadata.name // "-")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $root: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  if [ -n "$substituted" ]; then
    refuse "production root $root renders OCIRepository $(printf '%s\n' "$substituted" | paste -sd ' ' -) whose URL, ref or subject is decided by Flux substitution, which a static render cannot resolve; spell those fields literally"
  fi
done <<<"$roots"

# kro creates a templated OCIRepository for every instance of its RGD's kind, inside the
# cluster. The RGD may sit in one root and its instances in another, so collect the kinds
# from every root before looking for instances in any of them.
consumer_kinds=''
while IFS= read -r file; do
  [ -n "$file" ] || continue
  if ! kinds="$(yq -N -r 'select(.kind == "ResourceGraphDefinition")
      | select([(.spec.resources // [])[] | .template | select(.kind == "OCIRepository")
                | ((.spec.verify.matchOIDCIdentity // []) | map(.subject // "") | join(" "))]
               | join(" ") | test("workflows/publish-(app|manifests)"))
      | (.spec.schema.kind // "")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read a production render for ResourceGraphDefinitions: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  consumer_kinds="$consumer_kinds$kinds
"
done <<<"$root_files"
while IFS= read -r kind; do
  [ -n "$kind" ] || continue
  instances=0
  while IFS= read -r file; do
    [ -n "$file" ] || continue
    if ! count="$(KIND="$kind" yq -N -r '[select(.kind == strenv(KIND))] | length' "$file" 2>"$work/yq.err")"; then
      refuse "could not count $kind instances in a production render: $(tr '\n' ' ' <"$work/yq.err")"
    fi
    while IFS= read -r c; do
      [ -n "$c" ] || continue
      instances=$((instances + c))
    done <<<"$count"
  done <<<"$root_files"
  if [ "$instances" -gt 0 ]; then
    refuse "production renders $instances $kind instance(s); kro turns each into a shared-publish-workflow consumer OCIRepository inside the cluster, which neither the file scan nor this render has a document for"
  fi
done <<<"$consumer_kinds"

# ── 3. Compare the two consumer sets ───────────────────────────────────────────────────
# `consumer_rows` reads a file it cannot parse as having no consumers. Every render has
# already been parsed by the checks above, each of which refuses on a parse failure, so an
# unreadable render never reaches this point as an empty set.
while IFS= read -r file; do
  [ -n "$file" ] || continue
  consumer_rows "$file"
done <<<"$root_files" | LC_ALL=C sort -u >"$work/rendered"
discover_consumers "$SCAN_ROOT" | LC_ALL=C sort -u >"$work/scanned"

if [ ! -s "$work/rendered" ] && [ ! -s "$work/scanned" ]; then
  refuse "neither the file scan nor the production render found a single consumer; an empty set agreeing with an empty set compares nothing (roots: $root_labels)"
fi

LC_ALL=C comm -23 "$work/scanned" "$work/rendered" >"$work/only-scanned"
LC_ALL=C comm -13 "$work/scanned" "$work/rendered" >"$work/only-rendered"

describe() {
  local repo workflow version artifact
  while IFS=$'\t' read -r repo workflow version artifact; do
    [ -n "$repo" ] || continue
    printf '    artifact=%s repo=%s workflow=%s ref=%s\n' "$artifact" "$repo" "$workflow" "${version:-<none>}"
  done <"$1"
}

if [ -s "$work/only-scanned" ] || [ -s "$work/only-rendered" ]; then
  {
    printf 'guard-consumer-discovery-conservation: consumer discovery and the production render DISAGREE (roots: %s).\n' "$root_labels"
    if [ -s "$work/only-scanned" ]; then
      printf '  Discovered by the file scan but NOT rendered by production (a base production excludes, or a field an overlay changes):\n'
      describe "$work/only-scanned"
    fi
    if [ -s "$work/only-rendered" ]; then
      printf '  Rendered by production but NOT discovered by the file scan (a patched ref or URL, or a consumer in a file the scan never opens):\n'
      describe "$work/only-rendered"
    fi
    printf 'The same artifact on both sides means an overlay changed one of its fields. Until the two agree, the\n'
    printf 'publish-revision report names a selector production does not deploy, or misses one it does.\n'
  } >&2
  exit 1
fi

printf 'guard-consumer-discovery-conservation: %d consumer(s) found by the file scan match the production render exactly (roots: %s).\n' \
  "$(grep -c . "$work/rendered")" "$root_labels"
