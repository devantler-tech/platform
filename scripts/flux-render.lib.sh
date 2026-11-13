# shellcheck shell=bash
# shellcheck disable=SC2034 # the variables set here are read by the guard that sources this file
#
# Renders the cluster overlays the way Flux applies them, and pulls and prepares the charts their
# HelmReleases name, for the CI guards that judge the rendered manifests (#4465):
# guard-helm-post-renderers.sh and guard-pdb-selector-match.sh. They carried two copies of these
# steps, and the copies had diverged; one copy means a fix lands once, and each guard's
# protections hold for both:
#
#   - Every directory under `clusters/` other than `base` is a cluster, and one holding none of the
#     three file names `kubectl kustomize` builds from stops the run: an overlay passed over would
#     have its objects go unread while the rest reports clean.
#   - A chart pull is tried a bounded number of times, waiting longer after each failure, before it
#     counts as failed: a registry that is briefly unreachable would otherwise turn an unrelated
#     pull request into a cannot-check.
#   - The controller renderer must carry the audited build metadata of the embedded Helm SDK, not
#     merely run.
#   - A chart, a packaged dependency or a template value that reads `.Release.Revision` is refused:
#     an offline render always has revision 1, so it cannot render what the cluster runs.
#
# The caller defines `die` (print its own prefix and the message, exit 2), sets `controller_helm`
# (empty to build one), and calls `flux_render_init` with a private scratch
# directory before any other function here. Functions that `die` are meant to run in the guard's
# own shell, or in a subshell whose exit status the guard checks.

flux_render_tab="$(printf '\t')"
# The scripts beside this file: the renderer build and the capture plugin.
flux_render_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Anonymous Helm and Docker configuration under <scratch>: a chart source that needs credentials is
# refused by `flux_render_chart_source`, so none is ever read from the machine running the guard.
flux_render_init() { # <scratch>
  flux_render_scratch="$1"
  mkdir -p "$flux_render_scratch/helm/cache" "$flux_render_scratch/helm/config" "$flux_render_scratch/helm/data" \
    "$flux_render_scratch/docker" "$flux_render_scratch/charts" || die "cannot create the Helm scratch directories"
  printf '{}\n' >"$flux_render_scratch/docker/config.json"
  printf '{}\n' >"$flux_render_scratch/helm/registry.json"
  export HELM_CACHE_HOME="$flux_render_scratch/helm/cache" HELM_CONFIG_HOME="$flux_render_scratch/helm/config" \
    HELM_DATA_HOME="$flux_render_scratch/helm/data" HELM_REGISTRY_CONFIG="$flux_render_scratch/helm/registry.json" \
    DOCKER_CONFIG="$flux_render_scratch/docker" HELM_PLUGINS="$flux_render_scratch/helm/plugins"
  : >"$flux_render_scratch/charts.tsv"
  flux_render_pull_attempts=3
  flux_render_pull_backoff=2
  flux_render_renderer_checked=0
}

# Sets how often a chart pull is tried and how long to wait first. The names are the settings the
# caller read them from, for the refusal.
flux_render_pull_policy() { # <attempts> <backoff-seconds> <attempts-name> <backoff-name>
  case $1 in '' | *[!0-9]* | 0) die "$3 must be a whole number of at least 1, got '$1'" ;; esac
  case $2 in '' | *[!0-9]*) die "$4 must be a whole number of seconds, got '$2'" ;; esac
  flux_render_pull_attempts="$1"
  flux_render_pull_backoff="$2"
}

# Builds the controller renderer when none was named, and refuses one whose build metadata is not
# the audited embedded SDK's: an official CLI, or another build, renders different capabilities.
# Then prepares the private plugin that captures Helm's real input to a post-renderer: the final
# `helm template` output has already lost transient annotations a patch may inspect.
flux_render_ensure_renderer() {
  [ "$flux_render_renderer_checked" = 0 ] || return 0
  if [ -z "$controller_helm" ]; then
    controller_helm="$flux_render_scratch/controller-helm"
    "$flux_render_dir/build-controller-helm.sh" "$controller_helm" >"$flux_render_scratch/build.log" 2>&1 ||
      die "cannot build the audited controller SDK renderer: $(tail -c 500 "$flux_render_scratch/build.log")"
  fi
  [ -x "$controller_helm" ] || die "the controller SDK renderer '$controller_helm' is not executable"
  local metadata
  metadata="$("$controller_helm" version --template '{{ printf "%#v" . }}')" ||
    die 'cannot read the controller SDK renderer metadata'
  [ "$metadata" = 'version.BuildInfo{Version:"v4.3", GitCommit:"", GitTreeState:"", GoVersion:"go1.27.3", KubeClientVersion:"v1.37"}' ] ||
    die 'the controller SDK renderer has unaudited capability metadata'
  bash "$flux_render_dir/prepare-helm-render-input-capture.sh" "$HELM_PLUGINS" ||
    die 'cannot prepare the private capture plugin'
  flux_render_renderer_checked=1
}

# Prints the path from one existing directory to another, both resolved physically.
# Kustomize refuses an absolute resource path, so the wrapper names its targets this way.
flux_render_relpath() { # <from-dir> <to-dir>
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
flux_render_substitute() { # <variables.tsv> <yaml>
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
flux_render_wrapper='.spec as $s
  | {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": [strenv(RESOURCE)]}
  | with(select($s.images != null); .images = $s.images)
  | with(select($s.patches != null); .patches = $s.patches)
  | with(select($s.targetNamespace != null); .namespace = $s.targetNamespace)
  | with(select($s.namePrefix != null); .namePrefix = $s.namePrefix)
  | with(select($s.nameSuffix != null); .nameSuffix = $s.nameSuffix)'

# Substitution preserves objects annotated `kustomize.toolkit.fluxcd.io/substitute: disabled`.
flux_render_substitute_disabled='(.metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"] // "") == "disabled"'

# The variables one Flux Kustomization substitutes: its substituteFrom ConfigMaps in order, later
# ones winning, then its inline substitute map. Secrets are encrypted here and are not read.
# shellcheck disable=SC2016 # `$flux`, `$ns` and `$n` are jq variables, not shell expansions
flux_render_variables='. as $all
  | ($flux.metadata.namespace // "") as $ns
  | [ ($flux.spec.postBuild.substituteFrom // [])[] | select(.kind == "ConfigMap") | .name as $n
      | ([$all[] | select(.kind == "ConfigMap" and .metadata.name == $n and (.metadata.namespace // "") == $ns)][0].data // {}) ]
  + [ $flux.spec.postBuild.substitute // {} ]
  | add // {} | to_entries[]
  | select((.value | type) == "string" and (.value | test("[\t\n]") | not))
  | "\(.key)\t\(.value)"'

# Succeeds when <overlay>, one match of `<tree>/clusters/*/`, is a cluster to render. `base` is the
# template the overlays share, whose paths still carry placeholders, and the pattern itself (when
# `clusters/` holds no directory) is no overlay. Every other directory is a cluster, and one with
# none of the three file names `kubectl kustomize` builds from is refused rather than passed over.
flux_render_is_cluster() { # <overlay without its trailing slash>
  [ -d "$1" ] || return 1
  [ "${1##*/}" != base ] || return 1
  if [ ! -f "$1/kustomization.yaml" ] && [ ! -f "$1/kustomization.yml" ] && [ ! -f "$1/Kustomization" ]; then
    die "'$1' holds no kustomization.yaml, kustomization.yml or Kustomization, so it cannot be rendered as a cluster overlay — refusing to skip it"
  fi
}

# Renders one cluster overlay of <tree> the way Flux applies it: every Flux Kustomization it names
# is built from its `spec.path` with its `images`, `patches`, `components`, `targetNamespace`,
# `namePrefix` and `nameSuffix`, and its `postBuild` substitution is applied. Leaves in <work>:
#   layers.tsv     one line per Flux Kustomization: its document, the layer's file stem, its path
#   all.json       every object the cluster renders, before substitution
#   resolved.json  every object the cluster renders, substituted
# and <stem>.resolved.yaml per layer.
flux_render_cluster() { # <tree> <overlay> <work>
  local tree="$1" overlay="$2" work="$3" tree_real cluster doc path layers rendered wrap resource component
  local component_dir component_real deprecated flux_json
  cluster="${overlay##*/}"
  tree_real="$(cd "$tree" && pwd -P)" || die "cannot resolve '$tree'"
  mkdir -p "$work/flux" || die "cannot create a scratch directory for '$cluster'"

  kubectl kustomize "$overlay" >"$work/root.yaml" 2>"$work/root.err" ||
    die "cannot render cluster overlay '$overlay': $(head -c 500 "$work/root.err")"
  # One file per Flux Kustomization, so each is rendered with its own fields.
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
    deprecated="$(yq '(.spec.patchesStrategicMerge != null) or (.spec.patchesJson6902 != null)' "$doc")" ||
      die "cannot read the patch fields of the Flux Kustomization for '$path'"
    [ "$deprecated" = false ] ||
      die "the Flux Kustomization for '$path' uses patchesStrategicMerge or patchesJson6902, which this guard does not apply — use spec.patches"
    layers=$((layers + 1))
    rendered="$work/layer-$layers"
    wrap="$rendered.wrap"
    mkdir -p "$wrap" || die "cannot create a scratch directory for '$path'"
    resource="$(flux_render_relpath "$wrap" "$tree/$path")" || die "cannot resolve Flux path '$path'"
    RESOURCE="$resource" yq -N "$flux_render_wrapper" "$doc" >"$wrap/kustomization.yaml" 2>"$rendered.wrap.err" ||
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
      COMPONENT="$(flux_render_relpath "$wrap" "$component_dir")" yq -i '.components += [strenv(COMPONENT)]' "$wrap/kustomization.yaml" ||
        die "cannot add component '$component' to the Flux view of '$path'"
    done <"$rendered.components"
    kubectl kustomize --load-restrictor LoadRestrictionsNone "$wrap" >"$rendered.yaml" 2>"$rendered.err" ||
      die "cannot render '$tree/$path' for cluster '$cluster': $(head -c 500 "$rendered.err")"
    printf '%s\t%s\t%s\n' "$doc" "$rendered" "$path" >>"$work/layers.tsv"
  done
  [ "$layers" -gt 0 ] || die "cluster overlay '$overlay' names no Flux Kustomization"

  # Everything the cluster renders: substitution ConfigMaps are found here.
  # shellcheck disable=SC2046 # one word per rendered layer file, none with spaces
  yq ea -o=json -I=0 '[.] | map(select(. != null))' $(cut -f2 "$work/layers.tsv" | sed 's/$/.yaml/') \
    >"$work/all.json" 2>"$work/all.err" ||
    die "cannot read what cluster '$cluster' renders: $(head -c 500 "$work/all.err")"
  while IFS="$flux_render_tab" read -r doc rendered path; do
    flux_json="$(yq -o=json -I=0 '.' "$doc")" || die "cannot read the Flux Kustomization for '$path'"
    jq -r --argjson flux "$flux_json" "$flux_render_variables" "$work/all.json" >"$rendered.vars" 2>"$rendered.vars.err" ||
      die "cannot read the substitution variables for '$path': $(head -c 500 "$rendered.vars.err")"
    yq "select(($flux_render_substitute_disabled) | not)" "$rendered.yaml" >"$rendered.input.yaml" ||
      die "cannot read the objects rendered from '$path'"
    yq "select($flux_render_substitute_disabled)" "$rendered.yaml" >"$rendered.literal.yaml" ||
      die "cannot read the literal objects rendered from '$path'"
    if [ "$(jq -r '.spec.postBuild != null' <<<"$flux_json")" = true ]; then
      flux_render_substitute "$rendered.vars" "$rendered.input.yaml" >"$rendered.substituted.yaml" ||
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
    >"$work/resolved.json" 2>"$work/resolved.err" ||
    die "cannot collect the substituted cluster '$cluster': $(head -c 500 "$work/resolved.err")"
}

# What one HelmRelease renders from, looked up among <$all>, everything its cluster renders: the
# release, the chart source it names and the values each `valuesFrom` entry merges.
# shellcheck disable=SC2016 # jq definition text, not shell expansions
flux_render_release_inputs='def release_inputs($all):
  . as $r
  | ($r.metadata.namespace // "") as $ns
  | (if $r.spec.chartRef != null then
       {kind: $r.spec.chartRef.kind, name: $r.spec.chartRef.name, namespace: ($r.spec.chartRef.namespace // $ns)}
     elif $r.spec.chart.spec.sourceRef != null then
       {kind: $r.spec.chart.spec.sourceRef.kind, name: $r.spec.chart.spec.sourceRef.name,
        namespace: ($r.spec.chart.spec.sourceRef.namespace // $ns)}
     else null end) as $ref
  | {release: $r, sourceRef: $ref,
     valuesRefs: [($r.spec.valuesFrom // [])[] | . as $v
       | ([$all[] | select(.kind == $v.kind and .metadata.name == $v.name
           and (.metadata.namespace // "") == $ns)][0] // null) as $obj
       | {ref: $v, present: ($obj != null), value: (if $v.kind == "Secret" then "placeholder"
           else ($obj.data[$v.valuesKey // "values.yaml"] // null) end)}],
     source: (if $ref == null then null else
       ([$all[] | select(.kind == $ref.kind and .metadata.name == $ref.name
         and (.metadata.namespace // "") == $ref.namespace)][0] // null) end)};'

# The chart reference of a release record, as `helm pull` arguments ({ref, repo, version}), or the
# reason it cannot be pulled anonymously ({error}).
# shellcheck disable=SC2016 # jq program text, not shell expansions
flux_render_chart_source='
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
    else fail("its chart source kind \($src.kind) is not a HelmRepository or OCIRepository") end'

# Pulls a chart once per reference with <puller> (a Helm command) and prints the path of the
# archive. A failed pull is tried again, up to the pull policy's bound, waiting longer each time.
# Returns 1, with the reason in <reason-file>, when the chart cannot be pulled; a later release
# naming the same chart reuses that result without pulling again.
flux_render_pull_chart() { # <reason-file> <puller> <ref> <repo-url or ""> <version or "">
  local reason_file="$1" puller="$2" ref="$3" repo="$4" version="$5" key dir archives attempt delay
  key="$ref|$repo|$version"
  dir="$(awk -F '\t' -v k="$key" '$1 == k { print $2 }' "$flux_render_scratch/charts.tsv")"
  if [ -z "$dir" ]; then
    dir="$flux_render_scratch/charts/$(($(wc -l <"$flux_render_scratch/charts.tsv") + 1))"
    printf '%s\t%s\n' "$key" "$dir" >>"$flux_render_scratch/charts.tsv"
    attempt=1
    delay="$flux_render_pull_backoff"
    while :; do
      # Each attempt starts from an empty directory: a partial archive must not be counted.
      rm -rf "$dir"
      if ! mkdir -p "$dir"; then
        printf %s "cannot create a chart directory" >"$reason_file"
        return 1
      fi
      if [ -n "$repo" ] && [ -n "$version" ]; then
        "$puller" pull "$ref" --repo "$repo" --version "$version" --destination "$dir" >"$dir.log" 2>&1
      elif [ -n "$repo" ]; then
        "$puller" pull "$ref" --repo "$repo" --destination "$dir" >"$dir.log" 2>&1
      elif [ -n "$version" ]; then
        "$puller" pull "$ref" --version "$version" --destination "$dir" >"$dir.log" 2>&1
      else
        "$puller" pull "$ref" --destination "$dir" >"$dir.log" 2>&1
      fi || : # the archive count decides; a failed pull leaves none
      archives="$(find "$dir" -maxdepth 1 -name '*.tgz' | wc -l | tr -d ' ')"
      if [ "$archives" = 1 ] || [ "$attempt" -ge "$flux_render_pull_attempts" ]; then break; fi
      sleep "$delay"
      delay=$((delay * 2))
      attempt=$((attempt + 1))
    done
    printf '%s\n' "$attempt" >"$dir.attempts"
  fi
  archives="$(find "$dir" -maxdepth 1 -name '*.tgz' 2>/dev/null | wc -l | tr -d ' ')"
  if [ "$archives" != 1 ]; then
    printf 'cannot pull chart %s%s%s after %s attempt(s): %s' "$ref" "${repo:+ from $repo}" "${version:+ at $version}" \
      "$(cat "$dir.attempts" 2>/dev/null || printf '?')" "$(tr '\n' ' ' <"$dir.log" | head -c 300)" >"$reason_file"
    return 1
  fi
  find "$dir" -maxdepth 1 -name '*.tgz'
}

# Refuses a chart whose render depends on the release revision. An offline `helm template
# --is-upgrade` still renders revision 1, not the deployed one, so such a chart cannot be rendered
# the way the cluster runs it. The chart, its packaged dependencies (nested ones too, without
# running chart code or fetching anything) and the release's template values (inline, and the
# selected ConfigMap values, which `tpl` can evaluate) are inspected. Returns 1 with the reason in
# <reason-file> when the chart is refused or cannot be inspected: a failed inspection is never clean.
flux_render_refuse_revision() { # <record> <chart.tgz> <work> <chart description> <reason-file>
  local record="$1" tgz="$2" work="$3" chart_desc="$4" reason_file="$5" dependency pending inspected=0
  refuse() {
    printf '%s' "$*" >"$reason_file"
  }
  if ! mkdir -p "$work/chart"; then refuse "cannot create the chart inspection directory" && return 1; fi
  if ! tar -xzf "$tgz" -C "$work/chart"; then refuse "cannot inspect $chart_desc" && return 1; fi
  if ! mkdir -p "$work/dependencies"; then refuse "cannot create the dependency inspection directory" && return 1; fi
  : >"$work/inspected"
  while :; do
    pending=0
    if ! find "$work/chart" "$work/dependencies" -name '*.tgz' -type f >"$work/archives"; then
      refuse "cannot enumerate chart dependencies" && return 1
    fi
    while IFS= read -r dependency; do
      grep -qxF "$dependency" "$work/inspected" && continue
      inspected=$((inspected + 1))
      if [ "$inspected" -gt 100 ]; then refuse "too many packaged chart dependencies to inspect" && return 1; fi
      if ! mkdir -p "$work/dependencies/$inspected"; then refuse "cannot create a dependency inspection directory" && return 1; fi
      if ! tar -xzf "$dependency" -C "$work/dependencies/$inspected"; then
        refuse "cannot inspect a packaged chart dependency" && return 1
      fi
      printf '%s\n' "$dependency" >>"$work/inspected"
      pending=1
    done <"$work/archives"
    [ "$pending" = 1 ] || break
  done
  if ! jq -r '[.release.spec.values // {}, [.valuesRefs[].value]] | .. | strings' "$record" >"$work/template-inputs"; then
    refuse "cannot inspect its template values" && return 1
  fi
  # Revision anywhere, including raw-string property keys.
  grep -R -E -q "\.[[:space:]]*Revision|[\"'\`]Revision[\"'\`]" "$work/chart" "$work/dependencies" "$work/template-inputs"
  case $? in
    0) refuse "revision-dependent chart $chart_desc needs release history this guard cannot render" && return 1 ;;
    1) ;;
    *) refuse "cannot inspect revision inputs in $chart_desc" && return 1 ;;
  esac
  grep -R -E -q "\.[[:space:]]*HelmVersion|[\"'\`]HelmVersion[\"'\`]" "$work/chart" "$work/dependencies" "$work/template-inputs"
  case $? in
    0 | 1) ;;
    *) refuse "cannot inspect HelmVersion inputs in $chart_desc" && return 1 ;;
  esac
}

# Assembles a release record's values as Flux merges them, into <work>/values.json and the global
# array flux_render_value_args (Helm arguments). A targetPath reference overwrites inline values in
# Flux: start with spec.values, then apply each targetPath in reference order. ConfigMaps use their
# selected data, parsed by Helm's own strvals parser (--set), including numeric/boolean types and
# escaped separators. A Secret's value is the placeholder; a whole values document cannot be stood
# in for. Returns 1, with the reason in <reason-file>, when the values cannot be stood in for.
flux_render_values() { # <record> <work> <reason-file> <label>
  local record="$1" work="$2" reason_file="$3" label="$4" value_ref target value
  flux_render_value_args=()
  if ! jq -e '(.release.spec.valuesFrom // []) | all(.[]; (.targetPath // "") | test("^[A-Za-z0-9_-]+([.][A-Za-z0-9_-]+)*$"))' \
    "$record" >/dev/null; then
    printf %s "a spec.valuesFrom entry has no plain dotted targetPath, so the values it merges cannot be stood in for" >"$reason_file"
    return 1
  fi
  jq '.release.spec.values // {}' "$record" >"$work/values.json" || die "$label: cannot assemble its values"
  flux_render_value_args=(--values "$work/values.json")
  while IFS= read -r value_ref; do
    if [ "$(jq '.value == null' <<<"$value_ref")" = true ]; then
      [ "$(jq '.ref.optional == true and .present == false' <<<"$value_ref")" = true ] && continue
      printf 'required valuesFrom %s is not rendered' \
        "$(jq -r '.ref.kind + "/" + .ref.name + ":" + (.ref.valuesKey // "values.yaml")' <<<"$value_ref")" >"$reason_file"
      return 1
    fi
    target="$(jq -r '.ref.targetPath' <<<"$value_ref")"
    value="$(jq -r '.value' <<<"$value_ref")"
    flux_render_value_args+=(--set "$target=$value")
  done < <(jq -c '.valuesRefs[]' "$record")
}

# Prints the Helm release name helm-controller installs a release record under: spec.releaseName,
# or <targetNamespace>-<name>, shortened past 53 characters as helm-controller does.
flux_render_release_name() { # <record> <label>
  local record="$1" label="$2" release hash
  release="$(jq -r '.release.spec.releaseName // (if .release.spec.targetNamespace then
      "\(.release.spec.targetNamespace)-\(.release.metadata.name)" else .release.metadata.name end)' "$record")" ||
    die "$label: cannot read its release name"
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
  printf '%s\n' "$release"
}

# Applies a release record's post-renderers, in order, to <dir>/resources.yaml, each through
# `kubectl kustomize` exactly as Flux builds them (a Kustomization over the rendered output carrying
# the renderer's `patches` and `images`), leaving the result in <dir>/resources.yaml. Returns 1 when
# one does not apply, with its 1-based index in flux_render_failed_at, the count in
# flux_render_failed_of and kustomize's error in <dir>/error.
flux_render_apply_post_renderers() { # <record> <dir> <label>
  local record="$1" dir="$2" label="$3" count index
  count="$(jq '(.release.spec.postRenderers // []) | length' "$record")" || die "$label: cannot count its post-renderers"
  index=0
  while [ "$index" -lt "$count" ]; do
    jq --argjson i "$index" '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
        resources: ["resources.yaml"]} + (.release.spec.postRenderers[$i].kustomize | with_entries(select(.value != null)))' \
      "$record" >"$dir/kustomization.yaml" || die "$label: cannot build post-renderer $((index + 1))"
    if ! kubectl kustomize "$dir" >"$dir/next.yaml" 2>"$dir/error"; then
      flux_render_failed_at=$((index + 1))
      flux_render_failed_of="$count"
      return 1
    fi
    mv "$dir/next.yaml" "$dir/resources.yaml" || die "$label: cannot stage the output of post-renderer $((index + 1))"
    index=$((index + 1))
  done
}
