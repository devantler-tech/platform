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
# THE COMPARED IDENTITY includes the literal OCIRepository name and namespace, the exact URL,
# and every field the report acts on — the source repository and the
# artifact (both from `spec.url`), the shared workflow, and the effective `spec.ref` (digest >
# semver > tag, `unpinned` when omitted) — plus the cosign subjects themselves, so an overlay
# that narrows or widens a consumer's signer constraint is a divergence too. A rename or
# namespace change is a divergence even when the report's artifact selector stays the same.
# The report retains its public rows; only this guard asks for the richer object identity.
#
# WHAT A STATIC RENDER CANNOT SEE IS REFUSED, NEVER ASSUMED EQUAL
#   - a Flux-side transform on a production root (`spec.patches`, `spec.components`,
#     `spec.namePrefix`, `spec.nameSuffix`, `spec.targetNamespace`, and the v1beta2 `spec.patchesStrategicMerge` and
#     `spec.patchesJson6902`): kustomize-controller applies it after the build, so
#     `kubectl kustomize` does not show what Flux applies;
#   - production roots whose sole source is not the KSail-generated platform artifact:
#     agreeing source references do not prove this checkout holds their manifests;
#   - a rendered declaration or FluxInstance sync/patch that could redirect that generated
#     source to another artifact or revision, or change its content selection (`ignore`/`layerSelector`);
#   - a suspended production root, or a path that resolves outside the published k8s/ tree;
#   - a nested Flux Kustomization that applies another path from that same source: only the
#     overlay's roots are rendered, so that layer would go unseen;
#   - another rendered OCIRepository pointing at the platform artifact, followed by a
#     Kustomization or mapping-backed template: renaming a source cannot hide another layer;
#   - an admission mutation with unbounded kinds or a consumer/root kind match, and
#     unevaluated CEL mutations or native mutating webhooks that reach sources, roots
#     or their policy/controller carriers: persisted objects can differ from the static render;
#   - Kyverno generate/clone rules targeting consumers, roots or controller carriers,
#     including unbounded targets; kro instances in mapping-backed carriers count too;
#   - a mapping-backed nested Kustomization template on the platform source, including
#     ResourceSet resources and step resources, or a substituted nested source reference:
#     its applied path is not rendered here;
#   - an OCIRepository declaring semverFilter: the report does not evaluate Flux tag filters;
#   - an attributed OCIRepository with suspension other than absence or literal false:
#     the selected registry revision is not necessarily its fetched artifact;
#   - structural or consumer contract mapping keys decided by post-build substitution:
#     selecting only literal field names can omit an entire consumer;
#   - an OCIRepository whose URL, ref or subject carries `${`: Flux post-build substitution
#     decides it at apply time;
#   - a document or object template whose kind or apiVersion carries `${`: substitution can turn it into
#     an OCIRepository (or another production root) after discovery has already omitted it;
#   - a top-level OCIRepository or FluxInstance whose name/namespace carries `${`: it can
#     become the generated platform source identity only after the override check;
#   - an OCIRepository TEMPLATE (a nested mapping with a `spec`, not a `sourceRef`) inside any
#     rendered document: a controller creates that object inside the cluster, so neither the
#     scan nor any render has a document for it. A kro ResourceGraphDefinition is admitted
#     only while it names its complete schema GVK and production renders no matching instance;
#     any other carrier (a Flux Operator ResourceSet, say) is refused outright;
#   - non-empty ResourceSet resourcesTemplate strings, including step templates: their
#     runtime templating cannot be reproduced by traversing static YAML mappings;
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
SCAN_ROOT="$(cd -P "$SCAN_ROOT" && pwd -P)"
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
# A policy can rewrite or create another policy/controller before that carrier
# creates a consumer. Keep the complete carrier boundary shared across checks.
readonly CONSUMER_CARRIER_KINDS='OCIRepository|Kustomization|FluxInstance|ResourceSet|ResourceGraphDefinition|ClusterPolicy|Policy|MutatingPolicy|GeneratingPolicy|MutatingAdmissionPolicy|MutatingWebhookConfiguration'
readonly CONSUMER_LITERAL_KIND='^[A-Za-z][A-Za-z0-9]*$'
readonly CONSUMER_LITERAL_API_VERSION='^([a-z0-9]([a-z0-9.-]*[a-z0-9])?/)?[A-Za-z0-9]+$'
export CONSUMER_CARRIER_KINDS CONSUMER_LITERAL_KIND CONSUMER_LITERAL_API_VERSION
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
  local file="$1" label="$2" kinds api_versions mapping_keys
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
  # Substitution applies to keys too. Inspect structural keys on every object
  # mapping, then the complete contract of literal Flux source/root objects.
  # Ordinary keys inside unrelated workload/configuration data stay outside it.
  if ! mapping_keys="$(yq -N -r '[
      (.. | select(type == "!!map" and
        (has("apiVersion") or has("kind") or has("spec") or has("metadata")))
        | to_entries | .[] | .key | select(test("\\$\\{"))),
      (.. | select(type == "!!map" and
        ((.kind // "" | tostring) | test("^(" + strenv(CONSUMER_CARRIER_KINDS) + ")$")))
        | .. | select(type == "!!map") | to_entries | .[] | .key
        | select(test("\\$\\{"))),
      (.. | select(type == "!!map" and .kind == "ResourceSet")
        | [.spec.resources[], .spec.steps[].resources[]] | .[]
        | .. | select(type == "!!map") | to_entries | .[] | .key
        | select(test("<<")))
    ] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read object mapping keys in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds a consumer or object mapping key decided by substitution, so its consumers are UNKNOWN"
  done <<<"$mapping_keys"
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
  # The checkout being rendered is what KSail publishes to latest. A different ref
  # can select older manifests even when the artifact URL still matches. An omitted
  # or empty ref uses Flux's latest default; explicit digest/semver fields do not.
  if ! checks="$(yq -N -r 'select(.kind == "OCIRepository" and .metadata.name == "flux-system"
      and ((.metadata.namespace // "") == "" or .metadata.namespace == "flux-system"))
      | (.spec.ref // {} | (type == "!!map" and
          ((keys | length) == 0 or ((keys | length) == 1 and .tag == "latest"))))' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the platform source reference in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = true ] ||
      refuse "production render $label changes the platform source reference from latest, so its consumers are UNKNOWN"
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
          and .spec.sync.ref == "latest"
          and (.spec.sync.name // "flux-system") == "flux-system"
          and ((.spec.sync.path // "") | sub("^\\./", "")) == "clusters/prod")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read FluxInstance sync in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r check; do
    [ -z "$check" ] || [ "$check" = true ] ||
      refuse "production render $label holds a FluxInstance platform source override, so its consumers are UNKNOWN"
  done <<<"$checks"
  # Controller Deployment patches cannot target the source. All other patch targets could
  # select it; verification operations and ref changes that preserve latest are established
  # as preserving its identity and artifact. A strategic patch or broader operation is UNKNOWN.
  if ! checks="$(yq -N -r 'select(.kind == "FluxInstance" and .metadata.name == "flux" and .metadata.namespace == "flux-system")
      | .spec.kustomize.patches[] | select(.target.kind != "Deployment") | (.patch | from_yaml) | .[]
      | (.op == "test" or ((.op == "add" or .op == "replace" or .op == "remove")
          and (((.path // "") | test("^/spec/verify(/|$)"))
            or (.op == "remove" and (.path == "/spec/ref" or .path == "/spec/ref/tag"))
            or ((.op == "add" or .op == "replace") and
              ((.path == "/spec/ref/tag" and .value == "latest")
                or (.path == "/spec/ref" and (.value // {} | (type == "!!map" and
                    ((keys | length) == 0 or ((keys | length) == 1 and .tag == "latest"))))))))))' "$file" 2>"$work/yq.err")"; then
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
      + ((.spec.targetNamespace // "") | length)) | tostring),
    ((.spec.suspend // false) | tostring)
  ] | map(sub("^$", "-")) | join("	")' "$work/overlay.yaml" >"$work/overlay.rows" 2>"$work/overlay.rows.err"; then
  refuse "could not read the production overlay's render: $(tr '\n' ' ' <"$work/overlay.rows.err")"
fi

roots=''
src_kind=''
src_ns=''
src_name=''
while IFS=$'\t' read -r name path kind ns source transforms suspended; do
  [ -n "$name" ] || continue
  [ "$path" != '-' ] || refuse "production Flux Kustomization $name names no spec.path"
  [ "$suspended" = false ] || refuse "production Flux Kustomization $name is suspended or has an invalid suspension state, so its current consumers are UNKNOWN"
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
  dir="$(cd -P "$dir" && pwd -P)" || refuse "could not resolve production root $root, so its consumers are UNKNOWN"
  case "$dir" in
    "$K8S_DIR" | "$K8S_DIR"/*) ;;
    *) refuse "production root $root resolves outside the published k8s tree, so its consumers are UNKNOWN" ;;
  esac
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

# An OCI source's identity is not its artifact. Gather aliases from EVERY render
# before following references, so roots cannot evade this bound by declaration
# order. A missing namespace may inherit the source namespace at runtime.
platform_aliases=''
while IFS=$'\t' read -r file label; do
  [ -n "$file" ] || continue
  if ! aliases="$(yq -N -r 'select(.kind == "OCIRepository"
      and ((.apiVersion // "") | test("^source\\.toolkit\\.fluxcd\\.io/")))
      | select(((.spec.url // "") | sub("/+$", "")) == "oci://ghcr.io/devantler-tech/platform/manifests")
      | [(.metadata.namespace // ""), (.metadata.name // "")]
      | map(sub("^$", "-")) | join("\t")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read platform-artifact source aliases in $label, so its consumers are UNKNOWN"
  fi
  while IFS=$'\t' read -r alias_ns alias_name; do
    [ -n "$alias_ns" ] || continue
    # The generated identity already has explicit root/template checks below.
    [ "$alias_ns/$alias_name" != "$src_ns/$src_name" ] || continue
    [ "$alias_name" != '-' ] || refuse "a platform-artifact source in $label has no name, so its consumers are UNKNOWN"
    platform_aliases="$platform_aliases$alias_ns"$'\t'"$alias_name
"
  done <<<"$aliases"
done <<<"$(printf '%b' "$sources")"

if [ -n "$platform_aliases" ]; then
  while IFS=$'\t' read -r file label; do
    [ -n "$file" ] || continue
    # Overlay document roots are the paths we rendered. Its nested templates,
    # and every root's top-level or nested Kustomizations, can add unseen paths.
    export CONSUMER_ALIAS_INCLUDE_ROOT=false
    [ "$label" = "$OVERLAY_LABEL" ] || export CONSUMER_ALIAS_INCLUDE_ROOT=true
    if ! alias_refs="$(yq -N -r '.. | select(type == "!!map"
        and .kind == "Kustomization" and has("spec")
        and ((.apiVersion // "") | test("^kustomize\\.toolkit\\.fluxcd\\.io/"))
        and ((path | length) > 0 or strenv(CONSUMER_ALIAS_INCLUDE_ROOT) == "true"))
        | select(.spec.sourceRef.kind == "OCIRepository")
        | [(.metadata.name // ""), (.spec.sourceRef.namespace // .metadata.namespace // ""),
            (.spec.sourceRef.name // "")]
        | map(sub("^$", "-")) | join("\t")' "$file" 2>"$work/yq.err")"; then
      refuse "could not read platform-artifact source alias references in $label, so its consumers are UNKNOWN"
    fi
    while IFS=$'\t' read -r ref_name ref_ns ref_source; do
      [ -n "$ref_name" ] || continue
      while IFS=$'\t' read -r alias_ns alias_name; do
        [ -n "$alias_ns" ] || continue
        if [ "$ref_source" = "$alias_name" ] &&
          { [ "$ref_ns" = "$alias_ns" ] || [ "$ref_ns" = '-' ] || [ "$alias_ns" = '-' ]; }; then
          refuse "production render $label holds Kustomization $ref_name following platform-artifact source $alias_ns/$alias_name; its additional path is not rendered here, so its consumers are UNKNOWN"
        fi
      done <<<"$platform_aliases"
    done <<<"$alias_refs"
  done <<<"$(printf '%b' "$sources")"
fi

# ── 3. Refuse what a static render cannot see ────────────────────────────────────────
consumer_gvks=''
while IFS=$'\t' read -r file label; do
  [ -n "$file" ] || continue
  refuse_source_overrides "$file" "$label"
  # ResourceSet evaluates Go templates in mapping-backed resource objects too.
  # Require literal primary GVK fields before looking for a consumer or schema
  # instance. Reference metadata and workload data are not primary object types.
  if ! resource_types="$(yq -N -r '[.. | select(type == "!!map" and .kind == "ResourceSet")
      | [(.spec.resources // []), (.spec.steps[].resources // [])] | .[]
      | select(type != "!!seq" or ([.[] | select(type != "!!map"
          or (.kind | type) != "!!str" or (.apiVersion | type) != "!!str"
          or ((.kind | tostring | test(strenv(CONSUMER_LITERAL_KIND))) | not)
          or ((.apiVersion | tostring | test(strenv(CONSUMER_LITERAL_API_VERSION))) | not))] | length) > 0)] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read ResourceSet object types in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds ResourceSet object types decided at runtime, so its consumers are UNKNOWN"
  done <<<"$resource_types"
  # Native callbacks are not evaluated by a static build. Refuse rules that
  # can reach sources, roots or their policy/controller carriers. Selectors and
  # operation restrictions cannot establish what the callback will persist.
  if ! native_mutations="$(yq -N -r '[.. | select(type == "!!map" and
      .kind == "MutatingWebhookConfiguration") | .webhooks[] | .rules[]
      | select(((.apiGroups | type) != "!!seq" or (.apiGroups | length) == 0
          or ([.apiGroups[] | select(type != "!!str" or (. | tostring |
            test("^(source[.]toolkit[.]fluxcd[.]io|kustomize[.]toolkit[.]fluxcd[.]io|fluxcd[.]controlplane[.]io|kro[.]run|kyverno[.]io|policies[.]kyverno[.]io|admissionregistration[.]k8s[.]io)$|\\*|\\$\\{|<<|\\{\\{")))] | length) > 0)
        and ((.resources | type) != "!!seq" or (.resources | length) == 0
          or ([.resources[] | select(type != "!!str" or (. | tostring |
            test("^(ocirepositories|kustomizations|fluxinstances|resourcesets|resourcegraphdefinitions|clusterpolicies|policies|mutatingpolicies|generatingpolicies|mutatingadmissionpolicies|mutatingwebhookconfigurations)(/|$)|\\*|\\$\\{|<<|\\{\\{")))] | length) > 0))] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not bound native admission mutations in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds a native admission mutation that can change consumers or their controller carriers, so its consumers are UNKNOWN"
  done <<<"$native_mutations"
  # Admission and mutate-existing run after this build. Literal unrelated kind
  # matches are safe; source/root matches, wildcards and missing kind bounds are
  # unknown even when a rule has exclusions or conditional preconditions.
  if ! mutations="$(yq -N -r '.. | select(type == "!!map" and
      (.kind == "ClusterPolicy" or .kind == "Policy"))
      | .spec.rules[] | {"mutates": has("mutate"),
        "affected": (([.match | .. | select(type == "!!map" and has("resources"))] | length) == 0
        or ([.match | .. | select(type == "!!map" and has("resources"))
          | select((.resources.kinds | type) != "!!seq" or (.resources.kinds | length) == 0)] | length) > 0
        or ([.match | .. | select(type == "!!map" and has("resources")) | .resources.kinds[]
          | select(type != "!!str" or (. | tostring | test("^$|(^|/)(" + strenv(CONSUMER_CARRIER_KINDS) + ")(/|$)|\\*|\\?|\\[|\\$\\{|<<|\\{\\{")))] | length) > 0
        or ([.mutate | .. | select(type == "!!map" and has("targets")) | .targets[]
          | select(.kind == null or (.kind | type) != "!!str"
            or (.kind | tostring | test("^$|(^|/)(" + strenv(CONSUMER_CARRIER_KINDS) + ")(/|$)|\\*|\\?|\\[|\\$\\{|<<|\\{\\{")))] | length) > 0)}
      | select(.mutates == true) | .affected' "$file" 2>"$work/yq.err")"; then
    refuse "could not bound admission mutations in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r affected; do
    [ -z "$affected" ] || [ "$affected" = false ] ||
      refuse "production render $label holds an admission mutation that can change consumers or production roots, so its consumers are UNKNOWN"
  done <<<"$mutations"
  # Generate and clone execute after this render too. A literal unrelated kind
  # is bounded; missing/wildcard kinds or source/root/controller carriers are not.
  if ! generated="$(yq -N -r '[.. | select(type == "!!map" and
      (.kind == "ClusterPolicy" or .kind == "Policy"))
      | .spec.rules[] | select(has("generate"))
      | select((.generate | type) != "!!map"
        or ((.generate | has("foreach")) and
          ((.generate.foreach | type) != "!!seq" or (.generate.foreach | length) == 0
            or ([.generate.foreach[] | select(type != "!!map"
              or (.kind == null and .cloneList == null) or has("foreach"))] | length) > 0))
        or ([[.generate, .generate.foreach[]] | .[]
          | select((.kind == null and .cloneList == null and .foreach == null)
            or (.kind != null and ((.kind | type) != "!!str"
              or (.apiVersion | type) != "!!str"
              or ((.kind | tostring | test(strenv(CONSUMER_LITERAL_KIND))) | not)
              or ((.apiVersion | tostring | test(strenv(CONSUMER_LITERAL_API_VERSION))) | not)
              or (.kind | tostring | test("^$|(^|/)(" + strenv(CONSUMER_CARRIER_KINDS) + ")(/|$)|\\*|\\?|\\[|\\$\\{|<<|\\{\\{"))))
            or (has("cloneList") and
              ((.cloneList.kinds | type) != "!!seq" or (.cloneList.kinds | length) == 0
                or ([.cloneList.kinds[] | select(type != "!!str"
                  or (. | tostring | test("^$|(^|/)(" + strenv(CONSUMER_CARRIER_KINDS) + ")(/|$)|\\*|\\?|\\[|\\$\\{|<<|\\{\\{")))] | length) > 0)))
          ] | length) > 0)]
      | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not bound consumer generation in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds consumer generation or cloning that cannot be accounted for by a static build, so its consumers are UNKNOWN"
  done <<<"$generated"
  # CEL mutations are not evaluated by this static guard. Refuse their declared
  # presence rather than infer identity preservation from a pre-admission row.
  if ! mutations="$(yq -N -r '[.. | select(type == "!!map" and
      (.kind == "MutatingPolicy" or .kind == "MutatingAdmissionPolicy"
        or .kind == "GeneratingPolicy"))] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read CEL admission mutations in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds an unevaluated CEL admission mutation or generation, so its consumers are UNKNOWN"
  done <<<"$mutations"
  # Substitution precedes controller creation, including the inherited source
  # namespace. A nonliteral nested reference can become the platform's source.
  # OCI-producing RGDs are bounded below by a complete literal schema GVK and
  # zero matching instances across every render. Only their dormant documents
  # may defer this check; Kustomization-only RGDs have no such existing bound.
  if ! nested_sources="$(yq -N -r 'select(.kind != "ResourceGraphDefinition"
      or ([.. | select(type == "!!map" and .kind == "OCIRepository" and has("spec"))] | length) == 0)
      | [.. | select(type == "!!map"
      and .kind == "Kustomization" and has("spec") and (path | length) > 0)
      | [(.spec.sourceRef.kind // ""), (.spec.sourceRef.name // ""),
          (.spec.sourceRef.namespace // .metadata.namespace // "")]
      | map(tostring) | join(" ") | select(test("\\$\\{|<<|\\{\\{"))] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read nested source references in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds a nested source reference decided by substitution, so its consumers are UNKNOWN"
  done <<<"$nested_sources"
  # A nested FluxInstance is a source/root factory, even without a literal OCI
  # mapping in its template. Its generated sync is not rendered here. The same
  # dormant OCI-RGD exception still requires the complete schema and absence of
  # matching instances across all roots below.
  if ! nested_instances="$(yq -N -r 'select(.kind != "ResourceGraphDefinition"
      or ([.. | select(type == "!!map" and .kind == "OCIRepository" and has("spec"))] | length) == 0)
      | [.. | select(type == "!!map" and .kind == "FluxInstance" and has("spec")
        and (path | length) > 0)] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read nested FluxInstance templates in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds a nested FluxInstance that can create unseen sources or roots, so its consumers are UNKNOWN"
  done <<<"$nested_instances"
  # A controller-created Kustomization can apply an otherwise unseen path from
  # this artifact. path excludes the document root, which is handled above.
  if ! nested_templates="$(yq -N -r '[.. | select(type == "!!map"
      and .kind == "Kustomization" and has("spec") and (path | length) > 0)
      | select(.spec.sourceRef.kind == "OCIRepository"
          and .spec.sourceRef.name == "flux-system"
          and ((.spec.sourceRef.namespace // .metadata.namespace // "") == "flux-system"
            or (.spec.sourceRef.namespace // .metadata.namespace // "") == ""))] | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read nested Kustomization templates in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds a Kustomization template that can apply an unseen platform-artifact path, so its consumers are UNKNOWN"
  done <<<"$nested_templates"
  # ResourceSet strings are controller templates, not YAML object mappings. They
  # may contain multi-document YAML and Go-template control flow, so refuse any
  # non-empty top-level, step or nested template rather than omit its objects.
  if ! templates="$(yq -N -r '.. | select(type == "!!map" and .kind == "ResourceSet")
      | [(.spec.resourcesTemplate // ""), (.spec.steps[].resourcesTemplate // "")]
      | map(select(. != "")) | length' "$file" 2>"$work/yq.err")"; then
    refuse "could not read ResourceSet resourcesTemplate in the render of $label, so its consumers are UNKNOWN"
  fi
  while IFS= read -r count; do
    [ -z "$count" ] || [ "$count" = 0 ] ||
      refuse "production render $label holds non-empty ResourceSet resourcesTemplate or step resourcesTemplate, so its consumers are UNKNOWN"
  done <<<"$templates"
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
         (.spec.schema.kind // ""), (.spec.schema.group // "kro.run"), (.spec.schema.apiVersion // "")]
      | select(.[0] != "OCIRepository" and .[2] != "0") | map(sub("^$", "-")) | join("	")' "$file" 2>"$work/yq.err")"; then
    refuse "could not read the render of $label: $(tr '\n' ' ' <"$work/yq.err")"
  fi
  while IFS=$'\t' read -r c_kind c_id _count c_schema c_group c_version; do
    [ -n "$c_kind" ] || continue
    if [ "$c_kind" != 'ResourceGraphDefinition' ]; then
      refuse "production render $label holds $c_kind $c_id, which carries an OCIRepository template; the objects it generates exist only in the cluster, so neither the file scan nor this render has a document for them"
    fi
    if [ "$c_schema" = '-' ]; then
      refuse "production render $label holds ResourceGraphDefinition $c_id, which templates an OCIRepository but names no schema kind, so its instances cannot be counted"
    fi
    # shellcheck disable=SC2016 # A literal substitution marker, never shell expansion.
    if [ "$c_group" = '-' ] || [ "$c_version" = '-' ] ||
      [[ "$c_group/$c_version/$c_schema" == *'${'* ]] ||
      [[ "$c_group" == */* ]] || [[ "$c_version" == */* ]]; then
      refuse "production render $label holds ResourceGraphDefinition $c_id without a literal complete schema GVK, so its instances cannot be counted"
    fi
    consumer_gvks="$consumer_gvks$c_schema"$'\t'"$c_group/$c_version
"
  done <<<"$carriers"
done <<<"$sources"

# kro creates a templated OCIRepository for every instance of its RGD's complete GVK, inside the
# cluster. The RGD may sit in one render and its instances in another, so every GVK is
# collected before any render is searched for instances.
while IFS=$'\t' read -r kind api_version; do
  [ -n "$kind" ] || continue
  instances=0
  while IFS=$'\t' read -r file label; do
    [ -n "$file" ] || continue
    if ! count="$(CONSUMER_GVK_KIND="$kind" CONSUMER_GVK_API_VERSION="$api_version" yq -N -r '[.. | select(type == "!!map" and .kind == strenv(CONSUMER_GVK_KIND) and .apiVersion == strenv(CONSUMER_GVK_API_VERSION))] | length' "$file" 2>"$work/yq.err")"; then
      refuse "could not count $kind instances in the render of $label, so its consumers are UNKNOWN: $(tr '\n' ' ' <"$work/yq.err")"
    fi
    while IFS= read -r c; do
      [ -n "$c" ] || continue
      instances=$((instances + c))
    done <<<"$count"
    # A cloneList names GVKs as strings, so mapping-backed instance counting
    # cannot see the instances it creates. Join every root's direct/foreach
    # clone selectors to the complete schemas collected from every root first.
    # Kind-only and two-part selectors cannot rule out this schema; explicit
    # three-part foreign group/version selectors retain their literal boundary.
    if ! clones="$(CONSUMER_GVK_KIND="$kind" CONSUMER_GVK_SELECTOR="$api_version/$kind" yq -N -r '[.. | select(type == "!!map" and
        (.kind == "ClusterPolicy" or .kind == "Policy")) | .spec.rules[]
        | [.generate, .generate.foreach[]] | .[] | .cloneList.kinds[]
        | select(. == strenv(CONSUMER_GVK_SELECTOR) or . == strenv(CONSUMER_GVK_KIND)
          or (((split("/") | length) == 2) and
            ((split("/") | .[1]) == strenv(CONSUMER_GVK_KIND))))] | length' "$file" 2>"$work/yq.err")"; then
      refuse "could not bound cloning of $kind in the render of $label, so its consumers are UNKNOWN"
    fi
    while IFS= read -r c; do
      [ -z "$c" ] || [ "$c" = 0 ] ||
        refuse "production render $label clones a consumer-producing schema ($api_version/$kind) after the static build, so its consumers are UNKNOWN"
    done <<<"$clones"
  done <<<"$sources"
  if [ "$instances" -gt 0 ]; then
    refuse "production renders $instances $kind instance(s) of $api_version; kro turns each into an OCIRepository inside the cluster, which neither the file scan nor this render has a document for"
  fi
done <<<"$(printf '%s' "$consumer_gvks" | sort -u)"

# ── 4. Compare the two consumer sets ───────────────────────────────────────────────────
# A file either side cannot read, parse or attribute refuses: its consumers are UNKNOWN, and
# dropping it would compare two partial sets that can still agree.
rendered=''
object_contracts=''
while IFS=$'\t' read -r file label; do
  [ -n "$file" ] || continue
  rows="$(consumer_rows "$file" with-identity)" ||
    refuse "could not read the consumers in the production render of $label, so the rendered set is UNKNOWN"
  rendered="$rendered$rows
"
  rows="$(consumer_rows "$file" with-object-contract)" ||
    refuse "could not read OCI object contracts in the production render of $label, so the rendered set is UNKNOWN"
  object_contracts="$object_contracts$rows
"
done <<<"$sources"
printf '%s' "$rendered" | sed '/^$/d' | LC_ALL=C sort -u >"$work/rendered"
printf '%s' "$object_contracts" | sed '/^$/d' | LC_ALL=C sort -u >"$work/object-contracts"
scanned="$(discover_consumers "$SCAN_ROOT" with-identity)" ||
  refuse 'the file scan could not read every file it selected, so the scanned set is UNKNOWN'
printf '%s\n' "$scanned" | sed '/^$/d' | LC_ALL=C sort -u >"$work/scanned"

if [ ! -s "$work/rendered" ] && [ ! -s "$work/scanned" ]; then
  refuse "neither the file scan nor the production render found a single consumer; an empty set agreeing with an empty set compares nothing (roots: $root_labels)"
fi

LC_ALL=C comm -23 "$work/scanned" "$work/rendered" >"$work/only-scanned"
LC_ALL=C comm -13 "$work/scanned" "$work/rendered" >"$work/only-rendered"

# Print each mismatched conservation row's selectors and literal object identity together.
describe() {
  local repo workflow version artifact subjects namespace name url
  while IFS=$'\t' read -r repo workflow version artifact subjects namespace name url; do
    [ -n "$repo" ] || continue
    printf '    artifact=%s repo=%s workflow=%s ref=%s subject=%s source=%s/%s url=%s\n' \
      "$artifact" "$repo" "$workflow" "${version:-<none>}" "$subjects" "$namespace" "$name" "$url"
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
      printf '  Rendered by production but NOT discovered by the file scan (a patched identity, ref, URL or subject, or a consumer in a file the scan never opens):\n'
      describe "$work/only-rendered"
    fi
    printf 'The same artifact on both sides means an overlay changed one of its fields. Until the two agree, the\n'
    printf 'publish-revision report discovery does not describe production\047s exact OCI objects.\n'
  } >&2
  exit 1
fi

# The complete consumer sets now agree, but an unsigned or foreign-registry OCI source
# can overwrite one of those objects without contributing a report row. Compare EVERY
# rendered top-level Flux OCI contract where its identity could coincide with an attributed
# consumer. An absent namespace is unknown, so it overlaps every explicit namespace for
# that name. Identical contracts repeat harmlessly; unrelated unsigned identities stay out.
conflicts="$(awk -F '\t' '
  NR == FNR { consumer_namespace[++count] = $6; consumer_name[count] = $7; next }
  {
    contract = $0; sub(/^[^\t]*\t[^\t]*\t/, "", contract)
    for (i = 1; i <= count; i++) {
      if ($2 != consumer_name[i] || ($1 != "-" && consumer_namespace[i] != "-" && $1 != consumer_namespace[i])) continue
      if (seen[i] && previous[i] != contract) conflicts[consumer_namespace[i] "/" consumer_name[i]] = 1
      previous[i] = contract; seen[i] = 1
    }
  }
  END { for (identity in conflicts) print identity }
' "$work/rendered" "$work/object-contracts" | LC_ALL=C sort)"
if [ -n "$conflicts" ]; then
  conflicts="$(printf '%s\n' "$conflicts" | paste -sd ' ' -)"
  refuse "the rendered set has a conflicting OCIRepository identity $conflicts, so its consumers are UNKNOWN"
fi

printf 'guard-consumer-discovery-conservation: %d consumer(s) found by the file scan match the production render exactly (overlay and roots: %s, %s).\n' \
  "$(grep -c . "$work/rendered")" "$OVERLAY_LABEL" "$root_labels"
