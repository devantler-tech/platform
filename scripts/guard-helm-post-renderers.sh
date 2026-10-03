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
#   2. renders it with `helm template` from the release's own values, as both an install and an
#      upgrade, because a chart can render differently on `.Release.IsUpgrade`;
#   3. applies the post-renderers in order, each through `kubectl kustomize` exactly as Flux builds
#      them (a Kustomization over the rendered output carrying the renderer's `patches` and
#      `images`). Any error kustomize reports is the error helm-controller would report.
#
# WHICH RELEASES. With `--base <revision>`, only HelmReleases whose effective definition differs
# from that revision are rendered: the release spec after substitution, its chart source,
# selected ConfigMap values, and the admitted rendering profile. That covers a changed post-renderer,
# and also a chart bump or values change that moves
# what an unchanged post-renderer patches. Every other release is skipped without a chart pull, so
# the check stays proportionate to what a pull request changes. Without `--base`, every release
# with post-renderers is rendered. A base tree that cannot be rendered is no reason to check less:
# every release is checked instead.
#
# CONTROLLER PROFILE. --flux-version is an explicit, audited offline profile, not live version
# discovery. Flux 2.8.8 embeds Helm 4.2.0 and post-renders regular manifests only (nohooks).
# Its API has no postRenderStrategy field. Other profiles are UNKNOWN until audited. A matching
# 2.8.x selector does not prove the running patch: production delivery still needs OIDC readback.
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
# `${...}` in the chart source. Historical preserveValues and revision-dependent templates are
# also UNKNOWN: helm template --is-upgrade still renders revision 1, not the deployed revision.
# HelmVersion-dependent charts are UNKNOWN too: the official CLI injects v4.2.0 into template
# capabilities, whereas helm-controller's SDK build defaults to v4.2. Pinning the binary alone
# does not make that input identical.
# No cluster overlay, or an overlay that names no Flux
# Kustomization, or a tree that renders no HelmRelease at all, is exit 2 as well: a selector that
# matched nothing is indistinguishable from a clean tree.
#
# Usage: guard-helm-post-renderers.sh --flux-version 2.8.8 [--base <git-revision>] [--kube-version <version>] <k8s-root>
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

usage="usage: $0 --flux-version 2.8.8 [--base <git-revision>] [--kube-version <version>] <k8s-root>"
base=''
kube_version=''
flux_version=''
while [ "$#" -gt 0 ]; do
  case $1 in
    --flux-version)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then die "$usage"; fi
      flux_version="${2#v}"
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
    --)
      shift
      break
      ;;
    -*) die "unknown option '$1'; $usage" ;;
    *) break ;;
  esac
done
[ "$#" -eq 1 ] || die "$usage"
[ "$flux_version" = 2.8.8 ] || die "an audited --flux-version 2.8.8 profile is required"
root="${1%/}"
[ -d "$root/clusters" ] || die "'$root/clusters' is not a directory"
for tool in helm jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done
if [ -n "$base" ]; then
  command -v git >/dev/null 2>&1 || die "git is required for --base but not installed"
fi

scratch="$(mktemp -d)" || die "cannot create a scratch directory"
trap 'rm -rf "$scratch"' EXIT

# Charts are pulled anonymously, with no ambient Helm or Docker configuration: a source that
# needs credentials is refused below, so none is ever read from the machine running the guard.
mkdir -p "$scratch/helm/cache" "$scratch/helm/config" "$scratch/helm/data" "$scratch/docker" ||
  die "cannot create the Helm scratch directories"
printf '{}\n' >"$scratch/docker/config.json"
printf '{}\n' >"$scratch/helm/registry.json"
export HELM_CACHE_HOME="$scratch/helm/cache" HELM_CONFIG_HOME="$scratch/helm/config" \
  HELM_DATA_HOME="$scratch/helm/data" HELM_REGISTRY_CONFIG="$scratch/helm/registry.json" \
  DOCKER_CONFIG="$scratch/docker" HELM_PLUGINS="$scratch/helm/plugins"
helm_version="$(helm version --template '{{.Version}}')" || die "cannot read the Helm version"
[ "$helm_version" = v4.2.0 ] || die "Flux $flux_version requires Helm v4.2.0, not '$helm_version'"

tab="$(printf '\t')"

# Prints the path from one existing directory to another, both resolved physically.
# Kustomize refuses an absolute resource path, so the wrapper names its targets this way.
relpath() { # <from-dir> <to-dir>
  local from to common up rest
  from="$(cd "$1" && pwd -P)" || return 1
  to="$(cd "$2" && pwd -P)" || return 1
  common="$from"
  up=""
  while [ "${to#"$common"/}" = "$to" ] && [ "$to" != "$common" ]; do
    common="${common%/*}"
    up="../$up"
  done
  rest="${to#"$common"}"
  rest="${rest#/}"
  printf '%s%s' "$up" "${rest:-.}"
}

# Resolves Flux postBuild substitution in YAML text as kustomize-controller does: a variable from
# substitute/substituteFrom wins, then an inline default (`${name:=default}` or `${name:-default}`),
# then `placeholder` for a value held only in a Secret. `$${name}` is Flux's escape for a literal.
# shellcheck disable=SC2016 # awk program text, not shell expansions
substitute() { # <variables.tsv> <yaml>
  awk -F '\t' '
    FILENAME == ARGV[1] { vars[$1] = substr($0, length($1) + 2); next }
    {
      line = $0
      out = ""
      while (match(line, /[$][{][A-Za-z_][A-Za-z0-9_]*(:?[-=][^}]*)?[}]/)) {
        start = RSTART
        len = RLENGTH
        token = substr(line, start + 2, len - 3)
        if (start > 1 && substr(line, start - 1, 1) == "$") {
          out = out substr(line, 1, start - 2) "${" token "}"
          line = substr(line, start + len)
          continue
        }
        match(token, /^[A-Za-z_][A-Za-z0-9_]*/)
        name = substr(token, 1, RLENGTH)
        rest = substr(token, RLENGTH + 1)
        has_default = rest != ""
        sub(/^:?[-=]/, "", rest)
        if ((name in vars) && (vars[name] != "" || !has_default)) value = vars[name]
        else if (has_default) value = rest
        else value = "placeholder"
        out = out substr(line, 1, start - 1) value
        line = substr(line, start + len)
      }
      print out line
    }' "$1" "$2"
}

# The wrapper carries the Flux fields that change what the directory renders into.
# shellcheck disable=SC2016 # `$s` is a yq variable, not a shell expansion
wrapper='.spec as $s
  | {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": [strenv(RESOURCE)]}
  | with(select($s.images != null); .images = $s.images)
  | with(select($s.patches != null); .patches = $s.patches)
  | with(select($s.targetNamespace != null); .namespace = $s.targetNamespace)
  | with(select($s.namePrefix != null); .namePrefix = $s.namePrefix)
  | with(select($s.nameSuffix != null); .nameSuffix = $s.nameSuffix)'

# Substitution preserves objects annotated `kustomize.toolkit.fluxcd.io/substitute: disabled`.
post_rendered='select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
  and ((.spec.postRenderers // []) | length) > 0)'
substitute_disabled='(.metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"] // "") == "disabled"'

# The variables one Flux Kustomization substitutes: its substituteFrom ConfigMaps in order, later
# ones winning, then its inline substitute map. Secrets are encrypted here and are not read.
# shellcheck disable=SC2016 # `$flux`, `$ns` and `$n` are jq variables, not shell expansions
variables='. as $all
  | ($flux.metadata.namespace // "") as $ns
  | [ ($flux.spec.postBuild.substituteFrom // [])[] | select(.kind == "ConfigMap") | .name as $n
      | ([$all[] | select(.kind == "ConfigMap" and .metadata.name == $n and (.metadata.namespace // "") == $ns)][0].data // {}) ]
  + [ $flux.spec.postBuild.substitute // {} ]
  | add // {} | to_entries[]
  | select((.value | type) == "string" and (.value | test("[\t\n]") | not))
  | "\(.key)\t\(.value)"'

# One record per post-rendered HelmRelease: the release, the Flux path it came from, and the chart
# source it names, looked up among everything the same cluster renders.
# shellcheck disable=SC2016 # jq variables, not shell expansions
record='.[] | select(. != null) | . as $r
  | ($r.metadata.namespace // "") as $ns
  | (if $r.spec.chartRef != null then
       {kind: $r.spec.chartRef.kind, name: $r.spec.chartRef.name, namespace: ($r.spec.chartRef.namespace // $ns)}
     elif $r.spec.chart.spec.sourceRef != null then
       {kind: $r.spec.chart.spec.sourceRef.kind, name: $r.spec.chart.spec.sourceRef.name,
        namespace: ($r.spec.chart.spec.sourceRef.namespace // $ns)}
     else null end) as $ref
  | {cluster: $cluster, layer: $layer, release: $r, sourceRef: $ref,
     profile: $profile,
     valuesRefs: [($r.spec.valuesFrom // [])[] | . as $v
       | ([$all[0][] | select(.kind == $v.kind and .metadata.name == $v.name
           and (.metadata.namespace // "") == $ns)][0] // null) as $obj
       | {ref: $v, present: ($obj != null), value: (if $v.kind == "Secret" then "placeholder"
           else ($obj.data[$v.valuesKey // "values.yaml"] // null) end)}],
     source: (if $ref == null then null else
       ([$all[0][] | select(.kind == $ref.kind and .metadata.name == $ref.name
         and (.metadata.namespace // "") == $ref.namespace)][0] // null) end)}'

# Renders every cluster overlay under <k8s-root> and writes one record per post-rendered
# HelmRelease to <out>/records/<cluster>__<namespace>__<name>.json, with the part that decides the
# render in <out>/canon/ under the same name. Counts go to <out>/counts. Exits 2 on any failure:
# call it in a subshell.
collect() { # <k8s-root> <out>
  local tree="$1" out="$2" tree_real overlay cluster work doc path layers rendered wrap resource
  local component component_dir component_real flux_json releases key clusters=0 all_releases=0
  local profile selector line
  mkdir -p "$out/records" "$out/canon" || die "cannot create '$out'"
  tree_real="$(cd "$tree" && pwd -P)" || die "cannot resolve '$tree'"
  for overlay in "$tree"/clusters/*/; do
    overlay="${overlay%/}"
    cluster="${overlay##*/}"
    # clusters/base is the template the overlays share; its paths still carry placeholders.
    [ "$cluster" != base ] || continue
    [ -f "$overlay/kustomization.yaml" ] || continue
    clusters=$((clusters + 1))
    work="$out/$cluster"
    mkdir -p "$work/flux" || die "cannot create a scratch directory for '$cluster'"

    kubectl kustomize "$overlay" >"$work/root.yaml" 2>"$work/root.err" ||
      die "cannot render cluster overlay '$overlay': $(head -c 500 "$work/root.err")"
    # shellcheck disable=SC2016 # `$index` is a yq variable, not a shell expansion
    (cd "$work/flux" &&
      yq -N -s '"doc-" + $index' \
        'select(.kind == "Kustomization" and ((.apiVersion // "") | test("^kustomize[.]toolkit[.]fluxcd[.]io/")))' \
        "$work/root.yaml") 2>"$work/flux.err" ||
      die "cannot read the Flux Kustomizations rendered by '$overlay': $(head -c 500 "$work/flux.err")"

    layers=0
    : >"$work/layers.tsv"
    for doc in "$work/flux"/doc-*.yml; do
      [ -f "$doc" ] || continue
      path="$(yq '.spec.path // ""' "$doc")" || die "cannot read spec.path from a Flux Kustomization rendered by '$overlay'"
      [ -n "$path" ] || die "a Flux Kustomization rendered by '$overlay' has no spec.path"
      path="${path#./}"
      case $path in
        /* | .. | ../* | */.. | */../*) die "Flux path '$path' named by '$overlay' leaves '$tree'" ;;
      esac
      [ -d "$tree/$path" ] || die "Flux path '$path' named by '$overlay' does not exist under '$tree'"
      [ "$(yq '(.spec.patchesStrategicMerge != null) or (.spec.patchesJson6902 != null)' "$doc")" = false ] ||
        die "the Flux Kustomization for '$path' uses patchesStrategicMerge or patchesJson6902, which this guard does not apply — use spec.patches"
      layers=$((layers + 1))
      rendered="$work/layer-$layers"
      wrap="$rendered.wrap"
      mkdir -p "$wrap" || die "cannot create a scratch directory for '$path'"
      resource="$(relpath "$wrap" "$tree/$path")" || die "cannot resolve Flux path '$path'"
      RESOURCE="$resource" yq -N "$wrapper" "$doc" >"$wrap/kustomization.yaml" 2>"$rendered.wrap.err" ||
        die "cannot build the Flux view of '$path': $(head -c 500 "$rendered.wrap.err")"
      yq '.spec.components // [] | .[]' "$doc" >"$rendered.components" 2>"$rendered.components.err" ||
        die "cannot read spec.components for '$path': $(head -c 500 "$rendered.components.err")"
      while IFS= read -r component || [ -n "$component" ]; do
        [ -n "$component" ] || continue
        component_dir="$tree/$path/$component"
        [ -d "$component_dir" ] || die "component '$component' named for Flux path '$path' does not exist"
        component_real="$(cd "$component_dir" && pwd -P)" || die "cannot resolve component '$component' for '$path'"
        case $component_real in
          "$tree_real" | "$tree_real"/*) ;;
          *) die "component '$component' named for Flux path '$path' leaves '$tree'" ;;
        esac
        COMPONENT="$(relpath "$wrap" "$component_dir")" yq -i '.components += [strenv(COMPONENT)]' "$wrap/kustomization.yaml" ||
          die "cannot add component '$component' to the Flux view of '$path'"
      done <"$rendered.components"
      kubectl kustomize --load-restrictor LoadRestrictionsNone "$wrap" >"$rendered.yaml" 2>"$rendered.err" ||
        die "cannot render '$tree/$path' for cluster '$cluster': $(head -c 500 "$rendered.err")"
      printf '%s\t%s\t%s\n' "$doc" "$rendered" "$path" >>"$work/layers.tsv"
    done
    [ "$layers" -gt 0 ] || die "cluster overlay '$overlay' names no Flux Kustomization"

    # Everything the cluster renders: chart sources and substitution ConfigMaps are found here.
    # shellcheck disable=SC2046 # one word per rendered layer file, none with spaces
    yq ea -o=json -I=0 '[.] | map(select(. != null))' $(cut -f2 "$work/layers.tsv" | sed 's/$/.yaml/') \
      >"$work/all.json" 2>"$work/all.err" ||
      die "cannot read what cluster '$cluster' renders: $(head -c 500 "$work/all.err")"
    all_releases=$((all_releases + $(jq '[.[] | select(.kind == "HelmRelease")] | length' "$work/all.json")))

    while IFS="$tab" read -r doc rendered path; do
      flux_json="$(yq -o=json -I=0 '.' "$doc")" || die "cannot read the Flux Kustomization for '$path'"
      jq -r --argjson flux "$flux_json" "$variables" "$work/all.json" >"$rendered.vars" 2>"$rendered.vars.err" ||
        die "cannot read the substitution variables for '$path': $(head -c 500 "$rendered.vars.err")"
      yq "select(($substitute_disabled) | not)" "$rendered.yaml" >"$rendered.input.yaml" ||
        die "cannot read the objects rendered from '$path'"
      yq "select($substitute_disabled)" "$rendered.yaml" >"$rendered.literal.yaml" ||
        die "cannot read the literal objects rendered from '$path'"
      if [ "$(jq -r '.spec.postBuild != null' <<<"$flux_json")" = true ]; then
        substitute "$rendered.vars" "$rendered.input.yaml" >"$rendered.substituted.yaml" ||
          die "cannot substitute the objects rendered from '$path'"
      else
        cp "$rendered.input.yaml" "$rendered.substituted.yaml" || die "cannot copy the objects rendered from '$path'"
      fi
      yq ea '.' "$rendered.substituted.yaml" "$rendered.literal.yaml" >"$rendered.resolved.yaml" ||
        die "cannot collect the substituted objects rendered from '$path'"
    done <"$work/layers.tsv"
    # Sources can be rendered by a different layer than their release. Resolve every layer first.
    # shellcheck disable=SC2046 # one word per layer file, none with spaces
    yq ea -o=json -I=0 '[.] | map(select(. != null))' $(cut -f2 "$work/layers.tsv" | sed 's/$/.resolved.yaml/') \
      >"$work/resolved.json" || die "cannot collect the substituted cluster '$cluster'"
    selector="$(jq -er '[.[] | select(.kind == "FluxInstance") | .spec.distribution.version] | unique
      | if length == 1 then .[0] else error("missing or conflicting FluxInstance selectors") end' "$work/resolved.json")" ||
      die "cluster '$cluster' has no unique FluxInstance distribution selector"
    case $selector in
      2.8.x | 2.8.8 | v2.8.8) ;;
      *) die "cluster '$cluster' selector '$selector' does not admit audited Flux $flux_version" ;;
    esac
    profile="$(jq -n --arg flux "$flux_version" --arg helm "$helm_version" --arg kube "$kube_version" --arg selector "$selector" \
      '{flux: $flux, helm: $helm, kube: $kube, selector: $selector, hooks: "nohooks"}')"
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

# Pulls a chart once per reference and prints the path of the archive.
pull_chart() { # <label> <ref> <repo-url or ""> <version or "">
  local label="$1" ref="$2" repo="$3" version="$4" key dir archives
  key="$ref|$repo|$version"
  dir="$(awk -F '\t' -v k="$key" '$1 == k { print $2 }' "$scratch/charts.tsv")"
  if [ -z "$dir" ]; then
    dir="$scratch/charts/$(($(wc -l <"$scratch/charts.tsv") + 1))"
    mkdir -p "$dir" || die "cannot create a chart directory"
    if [ -n "$repo" ] && [ -n "$version" ]; then
      helm pull "$ref" --repo "$repo" --version "$version" --destination "$dir" >"$dir.log" 2>&1
    elif [ -n "$repo" ]; then
      helm pull "$ref" --repo "$repo" --destination "$dir" >"$dir.log" 2>&1
    elif [ -n "$version" ]; then
      helm pull "$ref" --version "$version" --destination "$dir" >"$dir.log" 2>&1
    else
      helm pull "$ref" --destination "$dir" >"$dir.log" 2>&1
    fi || die "$label: cannot pull chart $ref${repo:+ from $repo}${version:+ at $version}: $(head -c 500 "$dir.log")"
    printf '%s\t%s\n' "$key" "$dir" >>"$scratch/charts.tsv"
  fi
  archives="$(find "$dir" -maxdepth 1 -name '*.tgz' | wc -l | tr -d ' ')"
  [ "$archives" = 1 ] || die "$label: pulling $ref left $archives chart archives, expected exactly one"
  find "$dir" -maxdepth 1 -name '*.tgz'
}

# Applies one release's post-renderers, in order, to one stream of its rendered chart. Prints the
# kustomize error and returns 1 when a post-renderer does not apply.
apply_post_renderers() { # <record> <stream.yaml> <dir>
  local record="$1" stream="$2" dir="$3" count index
  count="$(jq '.release.spec.postRenderers | length' "$record")"
  mkdir -p "$dir" || die "cannot create '$dir'"
  cp "$stream" "$dir/resources.yaml" || die "cannot stage '$stream'"
  index=0
  while [ "$index" -lt "$count" ]; do
    jq --argjson i "$index" '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
        resources: ["resources.yaml"]} + (.release.spec.postRenderers[$i].kustomize | with_entries(select(.value != null)))' \
      "$record" >"$dir/kustomization.yaml" || die "cannot build post-renderer $((index + 1))"
    if ! kubectl kustomize "$dir" >"$dir/next.yaml" 2>"$dir/error"; then
      printf 'post-renderer %d of %d: %s' "$((index + 1))" "$count" "$(tr '\n' ' ' <"$dir/error" | head -c 600)"
      return 1
    fi
    mv "$dir/next.yaml" "$dir/resources.yaml" || die "cannot stage the output of post-renderer $((index + 1))"
    index=$((index + 1))
  done
}

# Renders one post-rendered HelmRelease's chart and applies its post-renderers. Appends a finding
# to $scratch/findings when they do not apply; exits 2 when the release cannot be rendered the way
# Flux does.
check_release() { # <record> <work>
  local record="$1" work="$2" label pull ref repo version chart_desc tgz release namespace
  local mode error rendered value_ref target value hash dependency pending inspected=0
  local value_args=()
  label="$(jq -r '"\(.cluster): HelmRelease \(.release.metadata.namespace)/\(.release.metadata.name)"' "$record")"
  mkdir -p "$work" || die "cannot create '$work'"

  jq -e '.release.spec.postRenderers | all(.[]; (keys == ["kustomize"]) and
      ((.kustomize // {}) | keys - ["patches", "images"] | length == 0) and
      (((.kustomize // {}).patches // []) | all(.[]; keys - ["patch", "target"] | length == 0)))' \
    "$record" >/dev/null ||
    die "$label: a post-renderer is not a kustomize patches/images renderer, which is all Flux's Kustomize post-renderer applies — this guard cannot execute it"
  jq -e '.release.spec.postRenderStrategy == null' "$record" >/dev/null ||
    die "$label: spec.postRenderStrategy is unsupported by Flux $flux_version"
  jq -e '.release.spec.upgrade.preserveValues != true' "$record" >/dev/null ||
    die "$label: spec.upgrade.preserveValues needs historical release values this offline guard cannot read"
  jq -e '((.release.spec.chart.spec.valuesFiles // []) | length == 0) and (.release.spec.chart.spec.valuesFile == null)' \
    "$record" >/dev/null || die "$label: spec.chart.spec.valuesFiles is not rendered by this guard"

  # A targetPath reference overwrites inline values in Flux. Start with spec.values, then apply
  # each targetPath in reference order. ConfigMaps use their selected data, parsed by Helm's own
  # strvals parser (--set), including numeric/boolean types and escaped separators. Secret values
  # retain the declared placeholder limitation; a whole values document cannot be stood in for.
  jq -e '(.release.spec.valuesFrom // []) | all(.[]; (.targetPath // "") | test("^[A-Za-z0-9_-]+([.][A-Za-z0-9_-]+)*$"))' \
    "$record" >/dev/null ||
    die "$label: a spec.valuesFrom entry has no plain dotted targetPath, so the values it merges cannot be stood in for"
  jq '.release.spec.values // {}' "$record" >"$work/values.json" || die "$label: cannot assemble its values"
  value_args=(--values "$work/values.json")
  while IFS= read -r value_ref; do
    if [ "$(jq '.value == null' <<<"$value_ref")" = true ]; then
      [ "$(jq '.ref.optional == true and .present == false' <<<"$value_ref")" = true ] && continue
      die "$label: required valuesFrom $(jq -r '.ref.kind + "/" + .ref.name + ":" + (.ref.valuesKey // "values.yaml")' <<<"$value_ref") is not rendered"
    fi
    target="$(jq -r '.ref.targetPath' <<<"$value_ref")"
    value="$(jq -r '.value' <<<"$value_ref")"
    value_args+=(--set "$target=$value")
  done < <(jq -c '.valuesRefs[]' "$record")

  # The chart reference, as `helm pull` arguments, or the reason it cannot be pulled anonymously.
  pull="$(jq -c '
    def creds: (.spec.secretRef != null) or (.spec.certSecretRef != null) or ((.spec.provider // "generic") != "generic");
    def fail($why): {error: $why};
    .release.spec as $s | .source as $src
    | if $src == null then fail("its chart source \(.sourceRef.kind // "?")/\(.sourceRef.namespace // "?")/\(.sourceRef.name // "?") is not rendered in this cluster")
      elif ($src | tostring | test("[$][{]")) then fail("its chart source \($src.kind)/\($src.metadata.name) still carries a ${...} substitution")
      elif ($src | creds) then fail("its chart source \($src.kind)/\($src.metadata.name) needs credentials, and charts are pulled anonymously here")
      elif $src.kind == "HelmRepository" and $s.chart.spec.chart != null then
        ($src.spec.url // "") as $url
        | if ($src.spec.type == "oci") or ($url | startswith("oci://")) then
            {ref: "\($url | sub("/+$"; ""))/\($s.chart.spec.chart)", repo: "", version: ($s.chart.spec.version // "")}
          else {ref: $s.chart.spec.chart, repo: $url, version: ($s.chart.spec.version // "")} end
      elif $src.kind == "OCIRepository" and $s.chartRef != null then
        ($src.spec.url // "") as $url
        | if ($url | startswith("oci://") | not) then fail("OCIRepository \($src.metadata.name) has no oci:// URL")
          elif $src.spec.ref.digest != null then {ref: "\($url)@\($src.spec.ref.digest)", repo: "", version: ""}
          elif $src.spec.ref.semverFilter != null then fail("OCIRepository \($src.metadata.name) filters semver tags, which helm pull cannot")
          elif $src.spec.ref.semver != null then {ref: $url, repo: "", version: $src.spec.ref.semver}
          elif $src.spec.ref.tag != null then {ref: $url, repo: "", version: $src.spec.ref.tag}
          else fail("OCIRepository \($src.metadata.name) names no digest, semver or tag") end
      else fail("its chart source kind \($src.kind) is not a HelmRepository or OCIRepository") end' "$record")" ||
    die "$label: cannot resolve its chart"
  error="$(jq -r '.error // ""' <<<"$pull")"
  [ -z "$error" ] || die "$label: $error"
  ref="$(jq -r '.ref' <<<"$pull")"
  repo="$(jq -r '.repo' <<<"$pull")"
  version="$(jq -r '.version' <<<"$pull")"
  chart_desc="$ref${version:+@$version}"
  tgz="$(pull_chart "$label" "$ref" "$repo" "$version")" || exit 2
  mkdir -p "$work/chart" || die "cannot create the chart inspection directory"
  tar -xzf "$tgz" -C "$work/chart" || die "$label: cannot inspect $chart_desc"
  mkdir -p "$work/dependencies" || die "cannot create the dependency inspection directory"
  : >"$work/inspected"
  # Dependencies may themselves be packaged charts. Inspect their templates too, without running
  # chart code or fetching any extra artifacts. A bounded nesting failure is UNKNOWN, never clean.
  while :; do
    pending=0
    find "$work/chart" "$work/dependencies" -name '*.tgz' -type f >"$work/archives" || die "$label: cannot enumerate chart dependencies"
    while IFS= read -r dependency; do
      grep -qxF "$dependency" "$work/inspected" && continue
      inspected=$((inspected + 1))
      [ "$inspected" -le 100 ] || die "$label: too many packaged chart dependencies to inspect"
      mkdir -p "$work/dependencies/$inspected" || die "$label: cannot create a dependency inspection directory"
      tar -xzf "$dependency" -C "$work/dependencies/$inspected" || die "$label: cannot inspect a packaged chart dependency"
      printf '%s\n' "$dependency" >>"$work/inspected"
      pending=1
    done <"$work/archives"
    [ "$pending" = 1 ] || break
  done
  # An offline Helm template upgrade always has Revision=1. Refuse charts (including dependencies)
  # that read Revision anywhere; do not pretend revision 2 proves every future upgrade either.
  if grep -R -E -q "\.[[:space:]]*Revision|[\"']Revision[\"']" "$work/chart" "$work/dependencies"; then
    die "$label: revision-dependent chart $chart_desc needs release history this guard cannot render"
  fi
  if grep -R -E -q "\.[[:space:]]*HelmVersion|[\"']HelmVersion[\"']" "$work/chart" "$work/dependencies"; then
    die "$label: HelmVersion-dependent chart $chart_desc needs the controller SDK capabilities, not the CLI build metadata"
  fi

  release="$(jq -r '.release.spec.releaseName // (if .release.spec.targetNamespace then
      "\(.release.spec.targetNamespace)-\(.release.metadata.name)" else .release.metadata.name end)' "$record")"
  if [ "$(jq '.release.spec.releaseName == null' "$record")" = true ] && [ "${#release}" -gt 53 ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      hash="$(printf '%s' "$release" | sha256sum)" || die "$label: cannot hash its release name"
    elif command -v shasum >/dev/null 2>&1; then
      hash="$(printf '%s' "$release" | shasum -a 256)" || die "$label: cannot hash its release name"
    else
      die "$label: sha256sum or shasum is required to shorten its release name"
    fi
    release="${release:0:40}-${hash:0:12}"
  fi
  namespace="$(jq -r '.release.spec.targetNamespace // .release.metadata.namespace' "$record")"

  for mode in install upgrade; do
    rendered="$work/$mode.yaml"
    if [ "$mode" = upgrade ]; then
      set -- --is-upgrade
    else
      set --
    fi
    if [ -n "$kube_version" ]; then
      set -- "$@" --kube-version "$kube_version"
    fi
    helm template "$release" "$tgz" --namespace "$namespace" "${value_args[@]}" "$@" \
      >"$rendered" 2>"$rendered.err" ||
      die "$label: cannot assemble its values or render $chart_desc as an $mode: $(head -c 500 "$rendered.err")"
    yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$rendered" >"$work/$mode.manifests.yaml" ||
      die "$label: cannot split the $mode render of $chart_desc into hooks and manifests"
      error="$(apply_post_renderers "$record" "$work/$mode.manifests.yaml" "$work/$mode.manifests")"
      case $? in
        0) ;;
        1)
          printf '%s: its post-renderers do not apply to the %s render of %s (%s): %s\n' \
            "$label" "$mode" "$chart_desc" manifests "$error" >>"$scratch/findings"
          return 0
          ;;
        *) exit 2 ;;
      esac
  done
  printf '  ok  %s: %s post-renderer(s) apply to %s (install and upgrade)\n' \
    "$label" "$(jq '.release.spec.postRenderers | length' "$record")" "$chart_desc"
}

(collect "$root" "$scratch/head") || exit 2
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
  elif ! (collect "$scratch/base-tree" "$scratch/base") 2>"$scratch/base.err"; then
    printf 'guard-helm-post-renderers: the tree at %s cannot be rendered (%s); checking every post-rendered HelmRelease\n' \
      "$base" "$(tr '\n' ' ' <"$scratch/base.err" | head -c 300)"
  else
    base_usable=1
  fi
fi

: >"$scratch/findings"
: >"$scratch/charts.tsv"
post_rendered_count=0
unchanged=0
checked=0
for record in "$scratch/head/records"/*.json; do
  [ -f "$record" ] || continue
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
