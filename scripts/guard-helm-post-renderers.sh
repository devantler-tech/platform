#!/usr/bin/env bash
#
# Fail when a HelmRelease's post-renderers cannot apply to the chart output they patch (#3581).
#
# WHY. A `postRenderers` block is inert data while it sits in the repository: every static check
# here (`ksail workload validate`, `kubectl kustomize`, Kubescape) sees a HelmRelease field, never
# the patch running. helm-controller executes it at reconcile time, against the chart's rendered
# output, and nothing before that does. #3577 merged fully green with a JSON-6902 `add` beneath
# `/spec/template/spec/securityContext`, which flux-operator 0.50.0 does not render at all. The
# upgrade failed `add operation does not apply: doc is missing path`, and because a JSON patch is
# all-or-nothing it took the sibling ops with it. The release could no longer upgrade, while
# helm-controller kept reporting it Ready from the previous successful upgrade.
#
# WHAT THIS DOES. It renders every cluster overlay under `<k8s-root>/clusters` the way Flux applies
# it (following the `spec.path` of each Flux Kustomization, with its `images`, `patches`,
# `components`, `targetNamespace`, `namePrefix` and `nameSuffix`), applies Flux's `postBuild`
# substitution to the release, its source and referenced ConfigMaps, then for each release:
#   1. pulls its chart at the pinned version from the source the release names;
#   2. renders it with the audited embedded-SDK Helm command from the release's own values, as both an install and an
#      upgrade, because a chart can render differently on `.Release.IsUpgrade`;
#   3. applies the post-renderers in order, each through `kubectl kustomize` exactly as Flux builds
#      them (a Kustomization over the rendered output carrying the renderer's `patches` and
#      `images`). Any error kustomize reports is the error helm-controller would report.
#
# WHICH RELEASES. With `--base <revision>`, only HelmReleases whose effective definition differs
# from that revision are rendered: the release spec after substitution, its chart source,
# selected ConfigMap values, and the admitted rendering profile. With an explicit Kubernetes
# version, --base-kube-version supplies the base revision's input separately; a pin-only change
# must not disappear by assigning the current pin to both profiles. That covers a changed post-renderer,
# and also a chart bump or values change that moves
# what an unchanged post-renderer patches. Every other release is skipped without a chart pull, so
# the check stays proportionate to what a pull request changes. Without `--base`, every release
# with post-renderers is rendered. A base tree that cannot be rendered is no reason to check less:
# every release is checked instead.
#
# CONTROLLER PROFILE. --flux-version is an explicit, audited offline profile, not live version
# discovery. Flux 2.9.6 with the signed helm-controller 1.7.1 image/CRD override
# embeds Helm 4.3.1. The distribution selector alone does not identify this profile.
# The controller's default postRenderStrategy
# is `combined`, which post-renders hooks and ordinary manifests in one stream.
# This guard models explicit `nohooks` and `combined` strategies; an omitted
# strategy or the separate-stream strategy is UNKNOWN until explicitly modeled.
# Other profiles are UNKNOWN until audited. A matching
# declared selector does not prove the running patch: production delivery still needs OIDC readback.
#
# ⚠️ WHAT THIS DOES NOT SEE. The chart renders without a cluster, so a template that branches on
# `.Capabilities.APIVersions` or on a `lookup` renders as if those APIs and objects were absent.
# A value held only in an encrypted Secret, through `postBuild.substituteFrom` or a `valuesFrom`
# entry with a `targetPath`, renders as the string `placeholder`. A patch whose target matches
# nothing applies cleanly in Flux too, so it passes here. CRDs from a chart's `crds/` directory
# are not post-rendered by Helm and are not rendered here.
#
# ⚠️ CANNOT-CHECK IS NEVER CLEAN. A release this guard cannot render the way Flux does is exit 2:
# a chart source that needs credentials or is not a HelmRepository or OCIRepository, a
# `valuesFrom` entry without a `targetPath`, `valuesFiles`, a post-renderer that is not a
# kustomize `patches`/`images` renderer, a chart that fails to pull or render, or an unresolved
# `${...}` in the chart source. A chart pull is tried POST_RENDERER_GUARD_PULL_ATTEMPTS times
# (default 3), waiting POST_RENDERER_GUARD_PULL_BACKOFF seconds (default 2) and twice as long
# after each failure, before it counts as failed. Historical preserveValues and revision-dependent templates are
# also UNKNOWN: helm template --is-upgrade still renders revision 1, not the deployed revision.
# Rendering uses the unmodified Helm 4.3.1 command built with Go 1.27.3 and no version linker
# flags, matching the controller's complete embedded-SDK build metadata. The official release
# CLI is used only to pull charts: it injects v4.3.1 rather than the controller's v4.3 capability.
# No cluster overlay, a directory under `clusters/` other than `base` that holds no kustomization
# file, an overlay that names no Flux Kustomization, or a tree that renders no HelmRelease at all,
# is exit 2 as well: a selector that matched nothing is indistinguishable from a clean tree.
# The overlays are rendered and the charts pulled by `scripts/flux-render.lib.sh`, the library
# the PodDisruptionBudget selector guard renders with.
#
# Usage: guard-helm-post-renderers.sh --flux-version 2.9.6 [--release <namespace/name>] [--base <git-revision>] [--kube-version <version> [--base-kube-version <version>]] <k8s-root>
#
# Exit codes:
#   0  every checked HelmRelease's post-renderers apply to its chart's rendered output
#   1  at least one HelmRelease's post-renderers do not apply (each is named, with the error)
#   2  cannot check: bad usage, a missing root, tool or base revision, a render, pull or parse
#      failure, a release this guard cannot render the way Flux does, or an anti-vacuity failure

set -uo pipefail

die() {
  printf 'guard-helm-post-renderers: %s\n' "$*" >&2
  exit 2
}

usage="usage: $0 --flux-version 2.9.6 [--release <namespace/name>] [--base <git-revision>] [--kube-version <version> [--base-kube-version <version>]] <k8s-root>"
base=''
kube_version=''
base_kube_version=''
flux_version=''
release_selector=''
while [ "$#" -gt 0 ]; do
  case $1 in
    --flux-version)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$usage"; fi
      flux_version="${2#v}"
      shift 2
      ;;
    --release)
      if [ "$#" -lt 2 ] || [[ ! "$2" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
        die '--release requires a literal namespace/name'
      fi
      release_selector="$2"
      shift 2
      ;;
    --base)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$usage"; fi
      base="$2"
      shift 2
      ;;
    --kube-version)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$usage"; fi
      kube_version="$2"
      shift 2
      ;;
    --base-kube-version)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$usage"; fi
      base_kube_version="$2"
      shift 2
      ;;
    --)
      shift
      break
      ;;
    -*) die "unknown option '$1'; $usage" ;;
    *) break ;;
  esac
done
[ "$#" -eq 1 ] || die "$usage"
[ "$flux_version" = 2.9.6 ] || die "an audited --flux-version 2.9.6 profile is required"
if [ -n "$base" ] && [ -n "$kube_version" ]; then
  [ -n "$base_kube_version" ] || die "--base-kube-version is required when comparing an explicit Kubernetes profile"
fi
if [ -n "$base_kube_version" ]; then
  if [ -z "$base" ] || [ -z "$kube_version" ]; then
    die "--base-kube-version requires --base and --kube-version"
  fi
fi
root="${1%/}"
[ -d "$root/clusters" ] || die "'$root/clusters' is not a directory"
for tool in helm jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done
if [ -n "$base" ]; then
  command -v git >/dev/null 2>&1 || die "git is required for --base but not installed"
fi
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || die "cannot resolve the script directory"
# shellcheck source=scripts/flux-render.lib.sh
source "$script_dir/flux-render.lib.sh"

scratch="$(mktemp -d)" || die "cannot create a scratch directory"
trap 'rm -rf "$scratch"' EXIT

flux_render_init "$scratch"
flux_render_pull_policy "${POST_RENDERER_GUARD_PULL_ATTEMPTS:-3}" "${POST_RENDERER_GUARD_PULL_BACKOFF:-2}" \
  POST_RENDERER_GUARD_PULL_ATTEMPTS POST_RENDERER_GUARD_PULL_BACKOFF
helm_version="$(helm version --template '{{.Version}}')" || die "cannot read the Helm version"
[ "$helm_version" = v4.3.1 ] || die "Flux $flux_version requires Helm v4.3.1, not '$helm_version'"

controller_helm="${CONTROLLER_HELM:-}"

tab="$flux_render_tab"

post_rendered='select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
  and ((.spec.postRenderers // []) | length) > 0)'

# One record per post-rendered HelmRelease: the release, the Flux path it came from, and the chart
# source it names, looked up among everything the same cluster renders.
# shellcheck disable=SC2016 # jq variables, not shell expansions
record="$flux_render_release_inputs"'
  .[] | select(. != null) | release_inputs($all[0]) + {cluster: $cluster, layer: $layer, profile: $profile}'

# Renders every cluster overlay under <k8s-root> and writes one record per post-rendered
# HelmRelease to <out>/records/<cluster>__<namespace>__<name>.json, with the part that decides the
# render in <out>/canon/ under the same name. Counts go to <out>/counts. Exits 2 on any failure:
# call it in a subshell.
collect() { # <k8s-root> <out> <kube-version>
  local tree="$1" out="$2" overlay cluster work doc rendered path releases key clusters=0 all_releases=0
  local profile selector line kube_version="$3"
  mkdir -p "$out/records" "$out/canon" || die "cannot create '$out'"
  for overlay in "$tree"/clusters/*/; do
    overlay="${overlay%/}"
    flux_render_is_cluster "$overlay" || continue
    cluster="${overlay##*/}"
    clusters=$((clusters + 1))
    work="$out/$cluster"
    flux_render_cluster "$tree" "$overlay" "$work"
    all_releases=$((all_releases + $(jq '[.[] | select(.kind == "HelmRelease")] | length' "$work/all.json")))
    selector="$(jq -er '[.[] | select(.kind == "FluxInstance") | .spec.distribution.version] | unique
      | if length == 1 then .[0] else error("missing or conflicting FluxInstance selectors") end' "$work/resolved.json")" ||
      die "cluster '$cluster' has no unique FluxInstance distribution selector"
    case $selector in
      2.9.6 | v2.9.6) ;;
      *) die "cluster '$cluster' selector '$selector' does not admit audited Flux $flux_version" ;;
    esac
    jq -e '[.[] | select(.kind == "FluxInstance")] | length == 1' "$work/resolved.json" >/dev/null ||
      die "cluster '$cluster' must render exactly one FluxInstance"
    jq '.[] | select(.kind == "FluxInstance")' "$work/resolved.json" >"$work/flux-instance.json" ||
      die "cannot collect the effective FluxInstance"
    bash "$script_dir/guard-controller-helm-profile.sh" "$work/flux-instance.json" ||
      die "cluster '$cluster' has an unaudited controller image/CRD composition"
    profile="$(jq -n --arg flux "$flux_version" --arg helm "$helm_version" --arg kube "$kube_version" --arg selector "$selector" \
      '{flux: $flux, helm: $helm, kube: $kube, selector: $selector, hooks: ["nohooks", "combined"]}')"
    while IFS="$tab" read -r doc rendered path; do
      releases="$(yq ea -o=json -I=0 "[. | $post_rendered] | map(select(. != null))" "$rendered.resolved.yaml" 2>"$rendered.releases.err")" ||
        die "cannot parse the substituted HelmReleases rendered from '$path': $(head -c 500 "$rendered.releases.err")"
      jq -c --arg cluster "$cluster" --arg layer "$path" --argjson profile "$profile" --slurpfile all "$work/resolved.json" "$record" \
        <<<"$releases" >"$rendered.records.jsonl" || die "cannot resolve the releases rendered from '$path'"
      while IFS= read -r line; do
        [ -n "$line" ] || continue
        key="$(jq -r '"\(.cluster)__\(.release.metadata.namespace // "")__\(.release.metadata.name)"' <<<"$line")" ||
          die "cannot name a HelmRelease rendered from '$path'"
        [ ! -e "$out/records/$key.json" ] ||
          die "cluster '$cluster' renders HelmRelease ${key#*__} more than once (again from '$path')"
        printf '%s\n' "$line" >"$out/records/$key.json"
        jq -cS '{spec: .release.spec, sourceKind: (.source.kind // null), source: (.source.spec // null), valuesRefs, profile}' \
          <<<"$line" >"$out/canon/$key.json" || die "cannot summarise HelmRelease ${key#*__}"
      done <"$rendered.records.jsonl"
    done <"$work/layers.tsv"
  done
  [ "$clusters" -gt 0 ] || die "no cluster overlay with a kustomization.yaml under '$tree/clusters'"
  [ "$all_releases" -gt 0 ] ||
    die "no cluster renders any HelmRelease under '$tree' — refusing to report an empty render as clean"
  printf '%s %s\n' "$clusters" "$all_releases" >"$out/counts"
}

# Renders one post-rendered HelmRelease's chart and applies its post-renderers. Appends a finding
# to $scratch/findings when they do not apply; exits 2 when the release cannot be rendered the way
# Flux does.
check_release() { # <record> <work>
  local record="$1" work="$2" label pull ref repo version chart_desc tgz release namespace
  local mode error rendered wire_input hook_strategy
  label="$(jq -r '"\(.cluster): HelmRelease \(.release.metadata.namespace)/\(.release.metadata.name)"' "$record")"
  mkdir -p "$work" || die "cannot create '$work'"

  jq -e '.release.spec.postRenderers | all(.[]; (keys == ["kustomize"]) and
      ((.kustomize // {}) | keys - ["patches", "images"] | length == 0) and
      (((.kustomize // {}).patches // []) | all(.[]; keys - ["patch", "target"] | length == 0)))' \
    "$record" >/dev/null ||
    die "$label: a post-renderer is not a kustomize patches/images renderer, which is all Flux's Kustomize post-renderer applies — this guard cannot execute it"
  jq -e '.release.spec.postRenderStrategy | IN("nohooks", "combined")' "$record" >/dev/null ||
    die "$label: spec.postRenderStrategy must be explicit nohooks or combined; omitted and separate-stream strategies are not modeled"
  hook_strategy="$(jq -r '.release.spec.postRenderStrategy' "$record")" || die "$label: cannot read its post-render strategy"
  jq -e '.release.spec.upgrade.preserveValues != true' "$record" >/dev/null ||
    die "$label: spec.upgrade.preserveValues needs historical release values this offline guard cannot read"
  jq -e '((.release.spec.chart.spec.valuesFiles // []) | length == 0) and (.release.spec.chart.spec.valuesFile == null)' \
    "$record" >/dev/null || die "$label: spec.chart.spec.valuesFiles is not rendered by this guard"

  flux_render_values "$record" "$work" "$work/reason" "$label" || die "$label: $(cat "$work/reason")"

  pull="$(jq -c "$flux_render_chart_source" "$record")" || die "$label: cannot resolve its chart"
  error="$(jq -r '.error // ""' <<<"$pull")"
  [ -z "$error" ] || die "$label: $error"
  ref="$(jq -r '.ref' <<<"$pull")"
  repo="$(jq -r '.repo' <<<"$pull")"
  version="$(jq -r '.version' <<<"$pull")"
  chart_desc="$ref${version:+@$version}"
  # The official release CLI pulls: the controller renderer only renders.
  tgz="$(flux_render_pull_chart "$work/reason" helm "$ref" "$repo" "$version")" || die "$label: $(cat "$work/reason")"
  flux_render_refuse_revision "$record" "$tgz" "$work" "$chart_desc" "$work/reason" || die "$label: $(cat "$work/reason")"

  release="$(flux_render_release_name "$record" "$label")" || exit 2
  namespace="$(jq -r '.release.spec.targetNamespace // .release.metadata.namespace' "$record")"

  flux_render_ensure_renderer

  for mode in install upgrade; do
    rendered="$work/$mode.yaml"
    wire_input="$work/$mode.postrender-input.yaml"
    if [ "$mode" = upgrade ]; then
      set -- --is-upgrade
    else
      set --
    fi
    if [ -n "$kube_version" ]; then
      set -- "$@" --kube-version "$kube_version"
    fi
    HELM_GUARD_CAPTURE="$wire_input" "$controller_helm" template "$release" "$tgz" --namespace "$namespace" "${flux_render_value_args[@]}" "$@" \
      --post-renderer guard-input-capture \
      >"$rendered" 2>"$rendered.err" ||
      die "$label: cannot assemble its values or render $chart_desc as an $mode: $(head -c 500 "$rendered.err")"
    [ -s "$wire_input" ] || die "$label: the SDK did not capture the $mode post-renderer input"
    if [ "$hook_strategy" = nohooks ]; then
      yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$wire_input" >"$work/$mode.manifests.yaml" ||
        die "$label: cannot split the $mode render of $chart_desc into hooks and manifests"
    else
      yq 'select(tag == "!!map")' "$wire_input" >"$work/$mode.manifests.yaml" ||
        die "$label: cannot collect the combined $mode render of $chart_desc"
    fi
    mkdir -p "$work/$mode.manifests" || die "cannot create '$work/$mode.manifests'"
    cp "$work/$mode.manifests.yaml" "$work/$mode.manifests/resources.yaml" || die "cannot stage '$work/$mode.manifests.yaml'"
    if ! flux_render_apply_post_renderers "$record" "$work/$mode.manifests" "$label"; then
      printf '%s: its post-renderers do not apply to the %s render of %s (%s): post-renderer %d of %d: %s\n' \
        "$label" "$mode" "$chart_desc" "$hook_strategy" "$flux_render_failed_at" "$flux_render_failed_of" \
        "$(tr '\n' ' ' <"$work/$mode.manifests/error" | head -c 600)" >>"$scratch/findings"
      return 0
    fi
  done
  printf '  ok  %s: %s post-renderer(s) apply to %s (install and upgrade)\n' \
    "$label" "$(jq '.release.spec.postRenderers | length' "$record")" "$chart_desc"
}

(collect "$root" "$scratch/head" "$kube_version") || exit 2
read -r clusters all_releases <"$scratch/head/counts"

base_usable=0
if [ -n "$base" ]; then
  git -C "$root" rev-parse --verify --quiet "${base}^{commit}" >/dev/null ||
    die "base revision '$base' is not available; the checkout must fetch it"
  prefix="$(git -C "$root" rev-parse --show-prefix)" || die "'$root' is not inside a git repository"
  toplevel="$(git -C "$root" rev-parse --show-toplevel)" || die "'$root' is not inside a git repository"
  mkdir -p "$scratch/base-tree" || die "cannot create the base tree directory"
  # From the top level: run in a subdirectory, git archive would look for the prefix inside it.
  if ! git -C "$toplevel" archive --format=tar "${base}:${prefix}" >"$scratch/base.tar" 2>"$scratch/base.err"; then
    printf 'guard-helm-post-renderers: %s holds no %s; checking every post-rendered HelmRelease\n' "$base" "${prefix:-tree}"
  elif ! tar -x -C "$scratch/base-tree" -f "$scratch/base.tar"; then
    die "cannot unpack the base tree at $base"
  elif ! (collect "$scratch/base-tree" "$scratch/base" "$base_kube_version") 2>"$scratch/base.err"; then
    printf 'guard-helm-post-renderers: the tree at %s cannot be rendered (%s); checking every post-rendered HelmRelease\n' \
      "$base" "$(tr '\n' ' ' <"$scratch/base.err" | head -c 300)"
  else
    base_usable=1
  fi
fi

: >"$scratch/findings"
post_rendered_count=0
unchanged=0
checked=0
for record in "$scratch/head/records"/*.json; do
  [ -f "$record" ] || continue
  if [ -n "$release_selector" ]; then
    identity="$(jq -er '.release.metadata.namespace + "/" + .release.metadata.name' "$record")" ||
      die 'cannot identify a post-rendered HelmRelease'
    [ "$identity" = "$release_selector" ] || continue
  fi
  post_rendered_count=$((post_rendered_count + 1))
  key="$(basename "$record" .json)"
  if [ "$base_usable" = 1 ] && [ -f "$scratch/base/canon/$key.json" ] &&
    cmp -s "$scratch/head/canon/$key.json" "$scratch/base/canon/$key.json"; then
    unchanged=$((unchanged + 1))
    continue
  fi
  checked=$((checked + 1))
  check_release "$record" "$scratch/work/$key"
done

if [ -n "$release_selector" ] && [ "$post_rendered_count" = 0 ]; then
  die "selected HelmRelease $release_selector was not found with post-renderers; nothing was checked"
fi

summary="$clusters cluster(s), $all_releases HelmRelease(s), $post_rendered_count with post-renderers"
if [ "$base_usable" = 1 ]; then
  summary="$summary, $unchanged unchanged since $base, $checked checked"
else
  summary="$summary, $checked checked"
fi

if [ -s "$scratch/findings" ]; then
  printf 'guard-helm-post-renderers: post-renderers that helm-controller cannot apply:\n' >&2
  sed 's/^/  /' "$scratch/findings" >&2
  printf 'helm-controller fails the install or upgrade on this error, and the release cannot upgrade until it is fixed.\n' >&2
  printf 'A JSON-6902 add needs its parent path in the rendered chart; a strategic-merge patch creates a missing parent.\n' >&2
  printf 'guard-helm-post-renderers: %s\n' "$summary" >&2
  exit 1
fi

printf 'guard-helm-post-renderers: %s, all apply\n' "$summary"
exit 0
