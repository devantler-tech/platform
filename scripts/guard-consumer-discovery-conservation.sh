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
# Rendering k8s/clusters/prod yields NO OCIRepository documents today. It renders the Flux
# Kustomizations whose `spec.path` names the layers Flux applies; every `kind: OCIRepository`
# string in it is a `sourceRef` REFERENCE. Taking that overlay alone would discover no
# consumer at all and pass vacuously. So the roots are read from that render (never listed
# here, so a new layer is covered the day it is wired) and each one is rendered in turn. The
# overlay's own render is a consumer source too, since the root sync applies it. The consumer
# rows are then extracted from those renders by the SAME `consumer_rows` the scan uses, so
# the only things that can differ are WHICH documents each side sees and what their fields say
# once kustomize has applied every patch.
#
# THE COMPARED IDENTITY is every field the report acts on — the source repository and the
# artifact (both from `spec.url`), the shared workflow, and the effective `spec.ref` (digest >
# semver > tag, `unpinned` when omitted) — plus the cosign subjects themselves, so an overlay
# that narrows or widens a consumer's signer constraint is a divergence too. `metadata.name` is
# not part of it: nothing reads it, so a rename changes nothing the report says.
#
# WHAT A STATIC RENDER CANNOT SEE IS REFUSED, NEVER ASSUMED EQUAL
#   - a Flux-side transform on a production root (`spec.patches`, `spec.components`,
#     `spec.namePrefix`, `spec.nameSuffix`, `spec.targetNamespace`, and the v1beta2 `spec.patchesStrategicMerge` and
#     `spec.patchesJson6902`): kustomize-controller applies it after the build, so
#     `kubectl kustomize` does not show what Flux applies;
#   - production roots whose sole source is not the KSail-generated platform artifact:
#     agreeing source references do not prove this checkout holds their manifests;
#   - a rendered declaration or FluxInstance sync/patch that could redirect that generated
#     source to another artifact or change its content selection (`ignore`/`layerSelector`);
#   - a nested Flux Kustomization that applies another path from that same source: only the
#     overlay's roots are rendered, so that layer would go unseen;
#   - an OCIRepository whose URL, ref or subject carries `${`: Flux post-build substitution
#     decides it at apply time;
#   - a document or object template whose kind or apiVersion carries `${`: substitution can turn it into
#     an OCIRepository (or another production root) after discovery has already omitted it;
#   - a top-level OCIRepository or FluxInstance whose name/namespace carries `${`: it can
#     become the generated platform source identity only after the override check;
#   - an OCIRepository TEMPLATE (a nested mapping with a `spec`, not a `sourceRef`) inside any
#     rendered document: a controller creates that object inside the cluster, so neither the
#     scan nor any render has a document for it. A kro ResourceGraphDefinition is admitted
#     only while it names its schema kind and production renders no instance of that kind;
#     any other carrier (a Flux Operator ResourceSet, say) is refused outright;
#   - a root that is missing or does not render, a file either side selected but cannot read,
#     parse or attribute (two partial sets can still agree), and an EMPTY consumer set on both
#     sides — an empty set compared to an empty set is agreement about nothing.
#
# WHAT IT DOES NOT COVER, BY NAME
#   - Consumers declared INSIDE another repository's published artifact, which a production
#     layer reconciles through a nested Flux Kustomization on that artifact's own source (each
#     tenant's manifests). Neither this repository's file scan nor any render of it can hold
#     them, and reading the artifact would need network access. "Match exactly" is a claim
#     about what this repository declares.
#   - Objects a HelmRelease chart renders: the chart is not part of the kustomize render.
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
readonly OVERLAY_LABEL='clusters/prod'
[ -d "$OVERLAY" ] || refuse "no production overlay at ${OVERLAY#"$SCAN_ROOT"/}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# KSail publishes k8s/ to this artifact and initializes Flux from clusters/prod. The platform's
# generated source identity is recorded by validate-flux-verify/instance.go and the production
# FluxInstance's root verification patch. Comparing an arbitrary source's paths
# against this directory would be agreement about a different artifact. Credentials may use
# variables, but neither their bytes nor their scope belong in these diagnostics.
readonly PLATFORM_SOURCE='OCIRepository/flux-system/flux-system'
if ! artifact_contract="$(yq -N -r '[
    (.apiVersion == "ksail.io/v1alpha1"), (.kind == "Cluster"),
    (.spec.cluster.gitOpsEngine == "Flux"),
    (.spec.workload.sourceDirectory == "k8s"),
    (.spec.workload.kustomizationFile == "clusters/prod"),
    (((.spec.cluster.localRegistry.registry // "") | sub("^.*@", "")) == "ghcr.io/devantler-tech/platform/manifests")
  ] | map(tostring) | join("\t")' "$SCAN_ROOT/ksail.prod.yaml" 2>"$work/contract.err")"; then
  refuse 'could not read the KSail production artifact contract, so the platform source is UNKNOWN'
fi
contract_rows=0
while IFS= read -r contract_row; do
  [ -n "$contract_row" ] || continue
  contract_rows=$((contract_rows + 1))
done <<<"$artifact_contract"
[ "$contract_rows" -eq 1 ] || refuse 'the KSail production artifact contract must name exactly one KSail Cluster, so the platform source is UNKNOWN'
[ "$artifact_contract" = $'true\ttrue\ttrue\ttrue\ttrue\ttrue' ] ||
  refuse 'the KSail production artifact contract does not bind k8s/clusters/prod to the platform OCI artifact through Flux, so the platform source is UNKNOWN'

# Flux substitutes the final YAML after kustomize build, including kind and apiVersion. Selectors for
# literal OCIRepository/Kustomization kinds cannot see a document whose type is decided
# later. Refuse that uncertainty before selecting either roots or consumers. Ordinary
# variables in ConfigMap data, workload fields and template names are still allowed.
refuse_substituted_object_types() {
  local file="$1" label="$2" kinds api_versions
  if ! kinds="$(yq -N -r '.. | select(type == "!!map" and (has("apiVersion") or has("spec")))
      | (.kind // "" | tostring) | select(test("\\$\\{"))' "$file" 2>"$work/yq.err")"; then
    refuse "could not read object kinds in the render of $label, so its consumers are UNKNOWN"
  fi
  [ -z "$kinds" ] || refuse "production render $label holds a document or object template whose kind is decided by substitution, so its consumers are UNKNOWN; spell object kinds literally"
  if ! api_versions="$(yq -N -r '.. | select(type == "!!map" and (has("apiVersion") or has("spec")))
      | (.apiVersion // "" | tostring) | select(test("\\$\\{"))' "$file" 2>"$work/yq.err")"; then
    refuse "could not read object API versions in the render of $label, so its consumers are UNKNOWN"
  fi
  [ -z "$api_versions" ] || refuse "production render $label holds a document or object template whose apiVersion is decided by substitution, so its consumers are UNKNOWN; spell object API versions literally"
}

refuse_source_overrides() {
  local file="$1" label="$2" checks check
  # These top-level identities can become the generated source or its owning instance
  # after Flux substitution. Uninstantiated RGD template names remain handled below.
  if ! checks="$(yq -N -r 'select(.kind == "OCIRepository" or .kind == "FluxInstance")
      | ([(.metadata.name // ""), (.metadata.namespace // "")] | join(" ") | test("\\$\\{"))' "$file" 2>"$work/yq.err")"; then
    refuse "could not read source identities in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = false ] ||
      refuse "production render $label holds an OCIRepository or FluxInstance whose source identity is decided by Flux substitution, so its consumers are UNKNOWN; spell source names and namespaces literally"
  done <<<"$checks"
  # An omitted/empty namespace cannot establish that this same-named source is
  # outside flux-system. Check every potentially canonical identity conservatively.
  if ! checks="$(yq -N -r 'select(.kind == "OCIRepository" and .metadata.name == "flux-system"
      and ((.metadata.namespace // "") == "" or .metadata.namespace == "flux-system"))
      | (.spec.url == "oci://ghcr.io/devantler-tech/platform/manifests")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read platform source declarations in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = true ] ||
      refuse "production render $label holds a platform source override outside the configured artifact, so its consumers are UNKNOWN"
  done <<<"$checks"
  # The generated source extracts its default layer with the default file exclusions.
  # URL equality does not attest that tree when a declaration changes source contents.
  # Presence is refused even for empty/null fields rather than inferring equivalence.
  if ! checks="$(yq -N -r 'select(.kind == "OCIRepository" and .metadata.name == "flux-system"
      and ((.metadata.namespace // "") == "" or .metadata.namespace == "flux-system"))
      | ((.spec | has("ignore")) or (.spec | has("layerSelector")))' "$file" 2>"$work/yq.err")"; then
    refuse "could not read platform source content selection in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = false ] ||
      refuse "production render $label changes platform source content selection with spec.ignore or spec.layerSelector, so its consumers are UNKNOWN"
  done <<<"$checks"
  if ! checks="$(yq -N -r 'select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system" and .spec.sync != null)
      | (.spec.sync.kind == "OCIRepository" and .spec.sync.url == "oci://ghcr.io/devantler-tech/platform/manifests"
          and (.spec.sync.name // "flux-system") == "flux-system"
          and ((.spec.sync.path // "") | sub("^\\./", "")) == "clusters/prod")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read FluxInstance sync in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = true ] ||
      refuse "production render $label holds a FluxInstance platform source override, so its consumers are UNKNOWN"
  done <<<"$checks"
  # Controller Deployment patches cannot target the source. All other patch targets could
  # select it; only JSON operations confined to verify/ref are established as preserving its
  # identity and artifact. A strategic patch or broader operation is UNKNOWN, never ignored.
  if ! checks="$(yq -N -r 'select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system")
      | .spec.kustomize.patches[] | select(.target.kind != "Deployment") | (.patch | from_yaml) | .[]
      | (.op == "test" or ((.op == "add" or .op == "replace" or .op == "remove")
          and ((.path // "") | test("^/spec/(verify|ref)(/|$)"))))' "$file" 2>"$work/yq.err")"; then
    refuse "could not rule out a FluxInstance platform source override in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = true ] ||
      refuse "production render $label holds a FluxInstance patch that could be a platform source override, so its consumers are UNKNOWN"
  done <<<"$checks"
}

# Every field is spelled `-` when empty, so consecutive tabs never collapse under `read`'s
# IFS-whitespace splitting and shift the later fields left.
readonly FLUX_KUSTOMIZATION='select(.kind == "Kustomization" and ((.apiVersion // "") | test("^kustomize\\.toolkit\\.fluxcd\\.io/")))'

# ── 1. The production roots, read from the overlay's render ───────────────────────────
if ! kubectl kustomize "$OVERLAY" >"$work/overlay.yaml" 2>"$work/overlay.err"; then
  refuse "could not render k8s/$OVERLAY_LABEL, so the production roots are UNKNOWN: $(tr '\n' ' ' <"$work/overlay.err")"
fi
refuse_substituted_object_types "$work/overlay.yaml" "$OVERLAY_LABEL"
if ! yq -N -r "$FLUX_KUSTOMIZATION"' | [
    (.metadata.name // ""),
    (.spec.path // ""),
    (.spec.sourceRef.kind // ""),
    (.spec.sourceRef.namespace // .metadata.namespace // ""),
    (.spec.sourceRef.name // ""),
    ((((.spec.patches // []) | length) + ((.spec.components // []) | length)
      + ((.spec.patchesStrategicMerge // []) | length) + ((.spec.patchesJson6902 // []) | length)
      + ((.spec.namePrefix // "") | length) + ((.spec.nameSuffix // "") | length)
      + ((.spec.targetNamespace // "") | length)) | tostring)
  ] | map(sub("^$", "-")) | join("	")' "$work/overlay.yaml" >"$work/overlay.rows" 2>"$work/overlay.rows.err"; then
  refuse "could not read the production overlay's render: $(tr '\n' ' ' <"$work/overlay.rows.err")"
fi

roots=''
src_kind=''
src_ns=''
src_name=''
while IFS=$'\t' read -r name path kind ns source transforms; do
  [ -n "$name" ] || continue
  [ "$path" != '-' ] || refuse "production Flux Kustomization $name names no spec.path"
  if [ "$transforms" != '0' ]; then
    refuse "production Flux Kustomization $name carries spec.patches, spec.components, spec.patchesStrategicMerge, spec.patchesJson6902, spec.namePrefix, spec.nameSuffix or spec.targetNamespace; Flux applies those after the build, so a static render does not show what it applies"
  fi
  # Each path is relative to the root of ITS source. This tree is one source; a second one
  # would make some path relative to an artifact nobody here can render.
  if [ -z "$src_kind" ]; then
    src_kind="$kind" src_ns="$ns" src_name="$source"
  elif [ "$kind/$ns/$source" != "$src_kind/$src_ns/$src_name" ]; then
    refuse "production Flux Kustomizations draw on more than one source ($src_kind/$src_ns/$src_name and $kind/$ns/$source); each path is relative to its own source, so this tree cannot render them all"
  fi
  # `./` is the source root itself. Stripping the prefix must not leave an EMPTY root, which
  # the loop below would skip as a blank line, dropping that layer from the comparison.
  path="${path#./}"
  roots="$roots${path:-.}
"
done <"$work/overlay.rows"
[ -n "$roots" ] || refuse "k8s/$OVERLAY_LABEL renders no Flux Kustomization, so there is no production root to compare against"
[ "$src_kind/$src_ns/$src_name" = "$PLATFORM_SOURCE" ] ||
  refuse "production roots' source $src_kind/$src_ns/$src_name is not the KSail-generated platform source $PLATFORM_SOURCE, so their consumers are UNKNOWN"

# ── 2. Render every root ──────────────────────────────────────────────────────────────
# `sources` lists every production render as `<file><TAB><label>`: the overlay first, then
# each root. The checks in step 3 and the comparison in step 4 read all of them.
sources="$work/overlay.yaml	$OVERLAY_LABEL
"
root_labels=''
i=0
while IFS= read -r root; do
  [ -n "$root" ] || continue
  i=$((i + 1))
  dir="$K8S_DIR/$root"
  [ -d "$dir" ] || refuse "production root $root does not exist under k8s/, so what it applies is UNKNOWN"
  file="$work/root-$i.yaml"
  if ! kubectl kustomize "$dir" >"$file" 2>"$work/root.err"; then
    refuse "could not render production root $root, so its consumers are UNKNOWN: $(tr '\n' ' ' <"$work/root.err")"
  fi
  refuse_substituted_object_types "$file" "$root"
  sources="$sources$file	$root
"
  root_labels="${root_labels:+$root_labels, }$root"

  # A nested Flux Kustomization applying another path from the SAME source is a layer this
  # guard never renders. A missing namespace is treated as a match: it cannot be ruled out.
  # (The overlay is exempt: its own Flux Kustomizations ARE the roots.)
  if ! nested="$(yq -N -r "$FLUX_KUSTOMIZATION"' | [
      (.metadata.name // ""), (.spec.sourceRef.kind // ""),
      (.spec.sourceRef.namespace // .metadata.namespace // ""), (.spec.sourceRef.name // "")
    ] | map(sub("^$", "-")) | join("	")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $root: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  while IFS=$'\t' read -r n_name n_kind n_ns n_src; do
    [ -n "$n_name" ] || continue
    # A variable in the reference can make this the platform's own source after discovery,
    # while the literal reference appears to belong to an external tenant artifact.
    case "$n_kind/$n_ns/$n_src" in
      *"\${"*) refuse "production root $root renders Flux Kustomization $n_name whose source is decided by Flux substitution, so its consumers are UNKNOWN" ;;
    esac
    if [ "$n_kind" = "$src_kind" ] && [ "$n_src" = "$src_name" ] &&
      { [ "$n_ns" = "$src_ns" ] || [ "$n_ns" = '-' ]; }; then
      refuse "production root $root renders Flux Kustomization $n_name, which applies another path from the platform's own source ($src_kind/$src_ns/$src_name); that layer is not rendered here, so its consumers are UNKNOWN"
    fi
  done <<<"$nested"
done <<<"$roots"

# ── 3. Refuse what a static render cannot see ────────────────────────────────────────
consumer_kinds=''
while IFS=$'\t' read -r file label; do
  [ -n "$file" ] || continue
  refuse_source_overrides "$file" "$label"
  # The literal characters `${` mark a Flux post-build substitution, deliberately not
  # expanded by the shell.
  if ! substituted="$(yq -N -r 'select(.kind == "OCIRepository")
      | select(([(.spec.url // ""), (.spec.ref.tag // ""), (.spec.ref.semver // ""), (.spec.ref.digest // ""),
                 ((.spec.verify.matchOIDCIdentity // []) | map(.subject // "") | join(" "))]
                | join(" ")) | test("\\$\\{"))
      | (.metadata.namespace // "-") + "/" + (.metadata.name // "-")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $label: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  if [ -n "$substituted" ]; then
    refuse "production render $label holds OCIRepository $(printf '%s\n' "$substituted" | paste -sd ' ' -) whose URL, ref or subject is decided by Flux substitution, which a static render cannot resolve; spell those fields literally"
  fi

  # Any document carrying an OCIRepository TEMPLATE: a nested mapping with a `spec`, which a
  # `sourceRef` or `chartRef` (kind and name only) never has. Its subject is not inspected —
  # a templated one would read as no shared workflow at all — so every template counts.
  # The row is built for EVERY document and filtered afterwards: yq builds an array even for
  # a document an earlier `select` dropped, filled with nulls (measured, yq 4.54), so
  # selecting first fed nulls to `sub`.
  if ! carriers="$(yq -N -r '[(.kind // ""), ((.metadata.namespace // "-") + "/" + (.metadata.name // "-")),
         ([.. | select(type == "!!map" and .kind == "OCIRepository" and has("spec"))] | length | tostring),
         (.spec.schema.kind // "")]
      | select(.[0] != "OCIRepository" and .[2] != "0") | map(sub("^$", "-")) | join("	")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $label: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  while IFS=$'\t' read -r c_kind c_id _count c_schema; do
    [ -n "$c_kind" ] || continue
    if [ "$c_kind" != 'ResourceGraphDefinition' ]; then
      refuse "production render $label holds $c_kind $c_id, which carries an OCIRepository template; the objects it generates exist only in the cluster, so neither the file scan nor this render has a document for them"
    fi
    if [ "$c_schema" = '-' ]; then
      refuse "production render $label holds ResourceGraphDefinition $c_id, which templates an OCIRepository but names no schema kind, so its instances cannot be counted"
    fi
    consumer_kinds="$consumer_kinds$c_schema
"
  done <<<"$carriers"
done <<<"$sources"

# kro creates a templated OCIRepository for every instance of its RGD's kind, inside the
# cluster. The RGD may sit in one render and its instances in another, so every kind is
# collected before any render is searched for instances.
while IFS= read -r kind; do
  [ -n "$kind" ] || continue
  instances=0
  while IFS=$'\t' read -r file label; do
    [ -n "$file" ] || continue
    if ! count="$(KIND="$kind" yq -N -r '[select(.kind == strenv(KIND))] | length' "$file" 2>"$work/yq.err")"; then
      refuse "could not count $kind instances in the render of $label: $(tr '\n' ' ' <"$work/yq.err")"
    fi
    while IFS= read -r c; do
      [ -n "$c" ] || continue
      instances=$((instances + c))
    done <<<"$count"
  done <<<"$sources"
  if [ "$instances" -gt 0 ]; then
    refuse "production renders $instances $kind instance(s); kro turns each into an OCIRepository inside the cluster, which neither the file scan nor this render has a document for"
  fi
done <<<"$(printf '%s' "$consumer_kinds" | sort -u)"

# ── 4. Compare the two consumer sets ───────────────────────────────────────────────────
# A file either side cannot read, parse or attribute refuses: its consumers are UNKNOWN, and
# dropping it would compare two partial sets that can still agree.
rendered=''
while IFS=$'\t' read -r file label; do
  [ -n "$file" ] || continue
  rows="$(consumer_rows "$file" with-subject)" ||
    refuse "could not read the consumers in the production render of $label, so the rendered set is UNKNOWN"
  rendered="$rendered$rows
"
done <<<"$sources"
printf '%s' "$rendered" | sed '/^$/d' | LC_ALL=C sort -u >"$work/rendered"
scanned="$(discover_consumers "$SCAN_ROOT" with-subject)" ||
  refuse 'the file scan could not read every file it selected, so the scanned set is UNKNOWN'
printf '%s\n' "$scanned" | sed '/^$/d' | LC_ALL=C sort -u >"$work/scanned"

if [ ! -s "$work/rendered" ] && [ ! -s "$work/scanned" ]; then
  refuse "neither the file scan nor the production render found a single consumer; an empty set agreeing with an empty set compares nothing (roots: $root_labels)"
fi

LC_ALL=C comm -23 "$work/scanned" "$work/rendered" >"$work/only-scanned"
LC_ALL=C comm -13 "$work/scanned" "$work/rendered" >"$work/only-rendered"

describe() {
  local repo workflow version artifact subjects
  while IFS=$'\t' read -r repo workflow version artifact subjects; do
    [ -n "$repo" ] || continue
    printf '    artifact=%s repo=%s workflow=%s ref=%s subject=%s\n' \
      "$artifact" "$repo" "$workflow" "${version:-<none>}" "$subjects"
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
      printf '  Rendered by production but NOT discovered by the file scan (a patched ref, URL or subject, or a consumer in a file the scan never opens):\n'
      describe "$work/only-rendered"
    fi
    printf 'The same artifact on both sides means an overlay changed one of its fields. Until the two agree, the\n'
    printf 'publish-revision report names a selector production does not deploy, or misses one it does.\n'
  } >&2
  exit 1
fi

printf 'guard-consumer-discovery-conservation: %d consumer(s) found by the file scan match the production render exactly (overlay and roots: %s, %s).\n' \
  "$(grep -c . "$work/rendered")" "$OVERLAY_LABEL" "$root_labels"
