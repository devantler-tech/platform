#!/usr/bin/env bash
#
# Fail when a PodDisruptionBudget selects no workload its cluster renders (#3596).
#
# WHY. A PodDisruptionBudget whose selector matches nothing is valid YAML: it renders, it passes
# schema validation and the Kubescape scan, and it reads as disruption protection while providing
# none. Its only symptom is `status.expectedPods: 0` on the live cluster, after it has shipped.
# homepage's budget shipped that way (#4365): its selector named a label Flagger rewrites on the
# Deployment that serves traffic, so it matched only the idle source Deployment and the two serving
# pods had no budget. A label change in any chart or vendored bundle detaches a budget the same way.
#
# WHAT THIS DOES. It renders every cluster overlay under `<k8s-root>/clusters` the way Flux applies
# it (following the `spec.path` of each Flux Kustomization, with its `images`, `patches`,
# `components`, `targetNamespace`, `namePrefix` and `nameSuffix`), applies Flux's `postBuild`
# substitution, and requires every rendered PodDisruptionBudget to select the pod template of at
# least one workload rendered into its namespace: a Deployment, StatefulSet, DaemonSet, ReplicaSet,
# ReplicationController or Rollout. Jobs and CronJobs are not counted: a budget that matches only
# their pods protects nothing that serves.
#
# WHERE THE WORKLOADS COME FROM.
#   1. The manifests the same cluster renders.
#   2. HelmReleases. Most budgets here guard a workload only a chart renders, which no Kustomize
#      build sees. When the manifests alone do not satisfy a budget, every HelmRelease installed
#      into its namespace is rendered: the chart is pulled at the pinned version from the source
#      the release names, rendered from the release's own values as both an install and an upgrade
#      (a chart can render differently on `.Release.IsUpgrade`), and its post-renderers are applied
#      in order through `kubectl kustomize`, as Flux builds them. The budget must be satisfied in
#      both renders. Charts are pulled and rendered by the Helm command
#      `scripts/build-controller-helm.sh` builds, the renderer the post-renderer guard audits;
#      `CONTROLLER_HELM` names one already built.
#   3. Flagger. A Canary's target is not what serves: Flagger creates `<target>-primary`, whose pod
#      template carries the target's labels with one rewritten to `<value>-primary`, and scales
#      the target itself to 0 between rollouts. So the primary is derived and counted, and the
#      target is not. The rewritten label is the first of Flagger's `-selector-labels` that the
#      target's `spec.selector.matchLabels` holds; the default list is modelled (flagger v1.45.0,
#      cmd/flagger/main.go), and a flagger HelmRelease that sets `selectorLabels` is cannot-check.
#
# ⚠️ REPLICAS ARE NOT JUDGED. A budget over a workload deliberately scaled to zero still selects
# it, so `replicas: 0` (origin-ca-issuer) is no finding. The one zero-replica workload this refuses
# to count is a Flagger target, because the pods that serve carry different labels.
#
# ⚠️ WHAT THIS DOES NOT SEE. The chart renders without a cluster, so a template that branches on
# `.Capabilities.APIVersions` or on a `lookup` renders as if the cluster's own APIs and objects
# were absent, and `.Release.Revision` is always 1. A value held only in an encrypted Secret
# renders as the string `placeholder`. A workload a chart renders into a namespace other than the
# one it is installed into is not found. A PodDisruptionBudget a chart renders for itself is not
# checked: only the budgets this repository declares are.
#
# ⚠️ CANNOT-CHECK IS NEVER CLEAN. A budget nothing rendered satisfies, in a namespace holding a
# HelmRelease this guard cannot render the way Flux does, is exit 2: a chart source that needs
# credentials or is not a HelmRepository or OCIRepository, a `valuesFrom` entry without a
# `targetPath`, `valuesFiles`, `upgrade.preserveValues`, `postRenderStrategy`, a post-renderer that
# is not a kustomize `patches`/`images` renderer or does not apply, or a chart that fails to pull
# or render. A release that cannot be rendered is no obstacle to a budget something else
# satisfies. A Canary whose target nothing renders, or whose primary Flagger could not create, is
# exit 2 the same way. No cluster overlay, an overlay that names no Flux Kustomization, or a tree
# that renders no PodDisruptionBudget at all is exit 2 as well: a selector that matched nothing is
# indistinguishable from a clean tree.
#
# A budget whose pods are built where nothing here can render them (an operator building a
# workload from its custom resource at runtime, a Deployment the node OS installs, a chart this
# guard cannot render) is allowed only by a reviewed row in `scripts/pdb-selector-exceptions.tsv`
# naming the budget, what builds its pods, and where its labels were verified. A row never stands
# in for a rendered mismatch: one naming a HelmRelease this guard can render is refused. A row for
# a budget that a rendered workload satisfies, or that no cluster renders, is itself a violation.
#
# Usage: guard-pdb-selector-match.sh [--kube-version <version>] <k8s-root>
#
# Exit codes:
#   0  every rendered PodDisruptionBudget selects a rendered workload, or carries a reviewed exception
#   1  at least one PodDisruptionBudget selects no rendered workload, or an exception row is stale
#      or names a producer that does not hold
#   2  cannot check: bad usage, a missing root, tool or exceptions file, a render or parse failure,
#      a budget that cannot be decided, a malformed exception row, or an anti-vacuity failure.
#      Budgets found detached in the same run are still listed.

set -uo pipefail

die() {
  printf 'guard-pdb-selector-match: %s\n' "$*" >&2
  exit 2
}

usage="usage: $0 [--kube-version <version>] <k8s-root>"
kube_version=''
while [ "$#" -gt 0 ]; do
  case $1 in
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
root="${1%/}"
[ -d "$root/clusters" ] || die "'$root/clusters' is not a directory"
for tool in jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || die "cannot resolve the script directory"
root_real="$(cd "$root" && pwd -P)" || die "cannot resolve '$root'"

exceptions_file="${PDB_SELECTOR_EXCEPTIONS:-$script_dir/pdb-selector-exceptions.tsv}"
[ -f "$exceptions_file" ] ||
  die "exceptions file '$exceptions_file' not found — refusing to run without the reviewed disposition list"

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

controller_helm="${CONTROLLER_HELM:-}"
controller_helm_checked=0
ensure_renderer() {
  [ "$controller_helm_checked" = 0 ] || return 0
  if [ -z "$controller_helm" ]; then
    controller_helm="$scratch/controller-helm"
    "$script_dir/build-controller-helm.sh" "$controller_helm" >"$scratch/build.log" 2>&1 ||
      die "cannot build the controller renderer: $(tail -c 500 "$scratch/build.log")"
  fi
  [ -x "$controller_helm" ] || die "the controller renderer '$controller_helm' is not executable"
  "$controller_helm" version >/dev/null 2>&1 || die "the controller renderer '$controller_helm' does not run"
  controller_helm_checked=1
}

tab="$(printf '\t')"

field() { # <row> <n>
  printf '%s\n' "$1" | awk -F '\t' -v n="$2" '{ print $n }'
}

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

# A malformed row is exit 2: a row this guard cannot read is a disposition nobody can audit, and
# ignoring it would let a detached budget pass on a row that says nothing.
: >"$scratch/exceptions.jsonl"
lineno=0
while IFS= read -r row || [ -n "$row" ]; do
  lineno=$((lineno + 1))
  case $row in '' | '#'*) continue ;; esac
  budget="$(field "$row" 1)"
  producer="$(field "$row" 2)"
  reason="$(field "$row" 3)"
  printf '%s' "$budget" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$' ||
    die "$exceptions_file:$lineno: column 1 must name a PodDisruptionBudget as <namespace>/<name>, got '$budget'"
  printf '%s' "$producer" | grep -Eq '^(external:[a-z0-9]([-a-z0-9]*[a-z0-9])?|[A-Z][A-Za-z0-9]*/[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?)$' ||
    die "$exceptions_file:$lineno: '$budget' names no producer (column 2 must be <Kind>/<namespace>/<name> of a rendered object, or external:<what>, got '$producer')"
  [ -n "$reason" ] || die "$exceptions_file:$lineno: '$budget' carries no reason (column 3)"
  [ "$(jq -r --arg key "$budget" 'select(.key == $key) | .key' "$scratch/exceptions.jsonl" | grep -c .)" = 0 ] ||
    die "$exceptions_file:$lineno: '$budget' is listed more than once"
  jq -cn --arg key "$budget" --arg producer "$producer" --arg reason "$reason" \
    '{key: $key, producer: $producer, reason: $reason}' >>"$scratch/exceptions.jsonl" ||
    die "$exceptions_file:$lineno: cannot record the row for '$budget'"
done <"$exceptions_file"
jq -cs '.' "$scratch/exceptions.jsonl" >"$scratch/exceptions.json" || die "cannot read the exception rows"

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

# The workloads among rendered objects, each with its pod-template labels. A workload that names
# no namespace takes the namespace its source installs into.
# shellcheck disable=SC2016 # jq variables, not shell expansions
workloads='[ .[] | select(.kind as $k | ["Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Rollout"] | index($k))
  | { namespace: (.metadata.namespace // $namespace), kind, name: (.metadata.name // ""),
      labels: ((.spec.template.metadata.labels // {}) | with_entries(.value |= tostring)),
      selector: ((.spec.selector.matchLabels? // {}) | with_entries(.value |= tostring)),
      origin: $origin, mode: $mode } ]'

# One HelmRelease, with the chart source it names and the ConfigMap values it merges, looked up
# among everything the same cluster renders.
# shellcheck disable=SC2016 # jq variables, not shell expansions
release_record='. as $all
  | [ .[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
      and (.metadata.namespace // "") == $ns and .metadata.name == $name) ]
  | if length != 1 then error("expected exactly one HelmRelease \($ns)/\($name), found \(length)") else .[0] end
  | . as $r
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
         and (.metadata.namespace // "") == $ref.namespace)][0] // null) end)}'

# Decides every PodDisruptionBudget of one cluster. Input: {cluster, pdbs, candidates, canaries,
# releases, objects, exceptions, selectorLabels}. Output: one {status, key, text} per budget,
# status being ok, excepted, finding or unknown.
# shellcheck disable=SC2016 # jq variables, not shell expansions
verdict='
  def modes: ["install", "upgrade"];
  def pairs: to_entries | map("\(.key)=\(.value)") | sort | join(",") | if . == "" then "(no labels)" else . end;
  # One boolean per requirement of the selector on input, against a label set.
  def satisfied($labels):
    [ ((.matchLabels // {}) | to_entries[] | ($labels[.key] != null and $labels[.key] == (.value | tostring))),
      ((.matchExpressions // [])[] | . as $e
        | ($labels | has($e.key)) as $has
        | (($e.values // []) | map(tostring)) as $values
        | if $e.operator == "In" then $has and any($values[]; . == $labels[$e.key])
          elif $e.operator == "NotIn" then ($has | not) or all($values[]; . != $labels[$e.key])
          elif $e.operator == "Exists" then $has
          elif $e.operator == "DoesNotExist" then ($has | not)
          else error("selector operator \($e.operator) is not one a label selector defines") end) ];
  def selects($labels): satisfied($labels) | all;
  def describe:
    [ ((.matchLabels // {}) | to_entries[] | "\(.key)=\(.value)"),
      ((.matchExpressions // [])[] | "\(.key) \(.operator)"
        + (if ((.values // []) | length) > 0 then " (" + (.values | map(tostring) | join(",")) + ")" else "" end)) ]
    | if length == 0 then "{} (every pod in the namespace)" else join(", ") end;

  . as $d
  # Flagger: per Canary and render, the target it scales to 0 and the primary it creates, or why
  # that cannot be derived.
  | [ $d.canaries[] | . as $c | modes[] as $m
      | [ $d.candidates[] | select(.namespace == $c.namespace and .kind == $c.kind and .name == $c.target
          and (.mode == "any" or .mode == $m)) ] as $targets
      | if ($targets | length) == 0 then
          {namespace: $c.namespace, mode: $m,
           unknown: "Canary \($c.namespace)/\($c.name) targets \($c.kind) \($c.target), which nothing rendered into the namespace produces"}
        else $targets[] | . as $t
          | ([ $d.selectorLabels[] | select($t.selector[.] != null) ] | .[0]) as $label
          | if $label == null then
              {namespace: $c.namespace, mode: $m,
               unknown: "Canary \($c.namespace)/\($c.name) targets \($t.kind) \($t.name), whose spec.selector.matchLabels holds none of the labels Flagger builds a primary from (\($d.selectorLabels | join(", ")))"}
            else
              {namespace: $c.namespace, mode: $m, canary: $c.name, source: $t,
               primary: ($t + {name: ($t.name + "-primary"),
                 labels: ($t.labels | with_entries(select(.key | contains("toolkit.fluxcd.io") | not))
                   | .[$label] = ($t.selector[$label] + "-primary")),
                 origin: "the primary Flagger creates for \($t.kind) \($t.name), \($t.origin)", mode: $m})}
            end
        end ] as $flagger
  | def idle($ns; $m): [ $flagger[] | select(.source != null and .namespace == $ns and .mode == $m)
        | .source + {idle: .canary} ];
    def live($ns; $m):
      [ $d.candidates[] | select(.namespace == $ns and (.mode == "any" or .mode == $m)) | . as $w
        | select(any(idle($ns; $m)[]; .kind == $w.kind and .name == $w.name) | not) | . + {idle: null} ]
      + [ $flagger[] | select(.primary != null and .namespace == $ns and .mode == $m) | .primary + {idle: null} ];
    [ $d.pdbs[] | . as $p | "\($p.namespace)/\($p.name)" as $key
      | "\($d.cluster): PodDisruptionBudget \($key)" as $label
      | [ modes[] as $m | {mode: $m, hits: (if $p.selector == null then []
            else [ live($p.namespace; $m)[] | . as $w | select($p.selector | selects($w.labels)) ] end)} ] as $renders
      | [ $renders[] | select((.hits | length) == 0) | .mode ] as $missing
      | if ($missing | length) == 0 then
          {status: "ok", key: $key,
           text: "\($label) selects \($renders[0].hits[0].kind) \($renders[0].hits[0].name) (\($renders[0].hits[0].origin))"}
        else
          $missing[0] as $m
          | (if $p.selector == null then 0 else ($p.selector | (.matchLabels // {} | length) + (.matchExpressions // [] | length)) end) as $total
          | ([ live($p.namespace; $m)[], idle($p.namespace; $m)[] ]
              | map(. as $w | . + {score: (if $p.selector == null then 0 else ($p.selector | satisfied($w.labels) | map(select(.)) | length) end)})
              | sort_by([(if .idle != null then 1 else 0 end), -.score, .kind, .name])) as $near
          | ([ $near[] | select(.idle != null and .score == $total and $p.selector != null) ]) as $idle_hits
          | (if $p.selector == null then "\($label) has no selector, and a budget without one selects no pod"
             else "\($label) selects no workload rendered into namespace \($p.namespace)"
               + (if ($missing | length) == 1 then " in the \($m) render" else "" end)
               + "\n    selector: \($p.selector | describe)" end
             + (if ($idle_hits | length) > 0 then
                  "\n    it matches only \($idle_hits[0].kind) \($idle_hits[0].name), which Flagger Canary \($idle_hits[0].idle) keeps at 0 replicas between rollouts; the pods that serve belong to \($idle_hits[0].name)-primary"
                else "" end)
             + (if ($near | length) == 0 then "\n    nearest:  no workload is rendered into namespace \($p.namespace)"
                else "\n    nearest:  " + ([ $near[0:3][] | "\(.kind) \(.name) (\(.origin)): \(.labels | pairs) — \(.score) of \($total) selector requirement(s) match"
                  + (if .idle != null then ", scaled to 0 by Flagger" else "" end) ] | join("\n              ")) end)) as $detail
          | ([ $d.releases[] | select(.namespace == $p.namespace and (.rendered | not))
                | "HelmRelease \(.id) cannot be rendered (\(.reason))" ]
             + ([ $flagger[] | select(.unknown != null and .namespace == $p.namespace) | .unknown ] | unique)) as $blind
          | ([ $d.exceptions[] | select(.key == $key) ] | .[0]) as $x
          | if $x == null then
              if ($blind | length) > 0 then
                {status: "unknown", key: $key, text: "\($detail)\n    cannot decide: \($blind | join("; "))"}
              else {status: "finding", key: $key, text: $detail} end
            elif ($x.producer | startswith("external:")) then
              {status: "excepted", key: $key, text: "\($label) is excepted: its pods are built by \($x.producer) (\($x.reason))"}
            elif ($d.objects | index($x.producer)) == null then
              {status: "finding", key: $key,
               text: "\($detail)\n    its exception names \($x.producer), which cluster \($d.cluster) does not render"}
            elif ($x.producer | startswith("HelmRelease/")) then
              ([ $d.releases[] | select("HelmRelease/" + .id == $x.producer) ] | .[0]) as $release
              | if $release == null then
                  {status: "finding", key: $key,
                   text: "\($detail)\n    its exception names \($x.producer), which is not installed into namespace \($p.namespace)"}
                elif $release.rendered then
                  {status: "finding", key: $key,
                   text: "\($detail)\n    its exception names \($x.producer), which renders here: an exception cannot stand in for a rendered mismatch"}
                else
                  {status: "excepted", key: $key,
                   text: "\($label) is excepted: its pods are built by \($x.producer), which cannot be rendered (\($release.reason)) (\($x.reason))"}
                end
            else
              {status: "excepted", key: $key, text: "\($label) is excepted: its pods are built by \($x.producer) (\($x.reason))"}
            end
        end ]'

# Pulls a chart once per reference and prints the path of the archive. Returns 1, with the reason
# in <reason-file>, when the chart cannot be pulled.
: >"$scratch/charts.tsv"
pull_chart() { # <reason-file> <ref> <repo-url or ""> <version or "">
  local reason_file="$1" ref="$2" repo="$3" version="$4" key dir archives
  key="$ref|$repo|$version"
  dir="$(awk -F '\t' -v k="$key" '$1 == k { print $2 }' "$scratch/charts.tsv")"
  if [ -z "$dir" ]; then
    dir="$scratch/charts/$(($(wc -l <"$scratch/charts.tsv") + 1))"
    if ! mkdir -p "$dir"; then
      printf %s "cannot create a chart directory" >"$reason_file"
      return 1
    fi
    printf '%s\t%s\n' "$key" "$dir" >>"$scratch/charts.tsv"
    if [ -n "$repo" ] && [ -n "$version" ]; then
      "$controller_helm" pull "$ref" --repo "$repo" --version "$version" --destination "$dir" >"$dir.log" 2>&1
    elif [ -n "$repo" ]; then
      "$controller_helm" pull "$ref" --repo "$repo" --destination "$dir" >"$dir.log" 2>&1
    elif [ -n "$version" ]; then
      "$controller_helm" pull "$ref" --version "$version" --destination "$dir" >"$dir.log" 2>&1
    else
      "$controller_helm" pull "$ref" --destination "$dir" >"$dir.log" 2>&1
    fi || : # the archive count below decides; a failed pull leaves none
  fi
  archives="$(find "$dir" -maxdepth 1 -name '*.tgz' | wc -l | tr -d ' ')"
  if [ "$archives" != 1 ]; then
    printf 'cannot pull chart %s%s%s: %s' "$ref" "${repo:+ from $repo}" "${version:+ at $version}" \
      "$(tr '\n' ' ' <"$dir.log" | head -c 300)" >"$reason_file"
    return 1
  fi
  find "$dir" -maxdepth 1 -name '*.tgz'
}

# Renders one HelmRelease's chart as an install and an upgrade and applies its post-renderers,
# leaving <work>/install.yaml and <work>/upgrade.yaml. Returns 1, with the reason in
# <work>/reason, when the release cannot be rendered the way Flux does.
render_release() { # <resolved.json> <namespace> <name> <work>
  local resolved="$1" ns="$2" name="$3" work="$4" record pull error ref repo version tgz release namespace
  local mode value_ref target value hash count index
  local value_args=()
  mkdir -p "$work" || die "cannot create '$work'"
  record="$work/record.json"
  jq -c --arg ns "$ns" --arg name "$name" "$release_record" "$resolved" >"$record" 2>"$work/record.err" ||
    die "cannot resolve HelmRelease $ns/$name: $(head -c 500 "$work/record.err")"
  cannot() { printf '%s' "$*" >"$work/reason"; }

  if ! jq -e '(.release.spec.postRenderers // []) | all(.[]; (keys == ["kustomize"]) and
      ((.kustomize // {}) | keys - ["patches", "images"] | length == 0) and
      (((.kustomize // {}).patches // []) | all(.[]; keys - ["patch", "target"] | length == 0)))' \
    "$record" >/dev/null; then
    cannot "a post-renderer is not a kustomize patches/images renderer"
    return 1
  fi
  if ! jq -e '.release.spec.postRenderStrategy == null' "$record" >/dev/null; then
    cannot "spec.postRenderStrategy is not modelled"
    return 1
  fi
  if ! jq -e '.release.spec.upgrade.preserveValues != true' "$record" >/dev/null; then
    cannot "spec.upgrade.preserveValues needs historical release values"
    return 1
  fi
  if ! jq -e '((.release.spec.chart.spec.valuesFiles // []) | length == 0) and (.release.spec.chart.spec.valuesFile == null)' \
    "$record" >/dev/null; then
    cannot "spec.chart.spec.valuesFiles is not rendered"
    return 1
  fi

  # A targetPath reference overwrites inline values in Flux. Start with spec.values, then apply
  # each targetPath in reference order. ConfigMaps use their selected data, parsed by Helm's own
  # strvals parser (--set). A Secret's value is the placeholder; a whole values document cannot
  # be stood in for.
  if ! jq -e '(.release.spec.valuesFrom // []) | all(.[]; (.targetPath // "") | test("^[A-Za-z0-9_-]+([.][A-Za-z0-9_-]+)*$"))' \
    "$record" >/dev/null; then
    cannot "a spec.valuesFrom entry has no plain dotted targetPath, so the values it merges cannot be stood in for"
    return 1
  fi
  jq '.release.spec.values // {}' "$record" >"$work/values.json" || die "HelmRelease $ns/$name: cannot assemble its values"
  value_args=(--values "$work/values.json")
  while IFS= read -r value_ref; do
    if [ "$(jq '.value == null' <<<"$value_ref")" = true ]; then
      [ "$(jq '.ref.optional == true and .present == false' <<<"$value_ref")" = true ] && continue
      cannot "required valuesFrom $(jq -r '.ref.kind + "/" + .ref.name + ":" + (.ref.valuesKey // "values.yaml")' <<<"$value_ref") is not rendered"
      return 1
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
    die "HelmRelease $ns/$name: cannot resolve its chart"
  error="$(jq -r '.error // ""' <<<"$pull")"
  if [ -n "$error" ]; then
    cannot "$error"
    return 1
  fi
  ref="$(jq -r '.ref' <<<"$pull")"
  repo="$(jq -r '.repo' <<<"$pull")"
  version="$(jq -r '.version' <<<"$pull")"
  ensure_renderer
  tgz="$(pull_chart "$work/reason" "$ref" "$repo" "$version")" || return 1

  release="$(jq -r '.release.spec.releaseName // (if .release.spec.targetNamespace then
      "\(.release.spec.targetNamespace)-\(.release.metadata.name)" else .release.metadata.name end)' "$record")"
  if [ "$(jq '.release.spec.releaseName == null' "$record")" = true ] && [ "${#release}" -gt 53 ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      hash="$(printf '%s' "$release" | sha256sum)" || die "HelmRelease $ns/$name: cannot hash its release name"
    elif command -v shasum >/dev/null 2>&1; then
      hash="$(printf '%s' "$release" | shasum -a 256)" || die "HelmRelease $ns/$name: cannot hash its release name"
    else
      die "HelmRelease $ns/$name: sha256sum or shasum is required to shorten its release name"
    fi
    release="${release:0:40}-${hash:0:12}"
  fi
  namespace="$(jq -r '.release.spec.targetNamespace // .release.metadata.namespace' "$record")"
  count="$(jq '(.release.spec.postRenderers // []) | length' "$record")" || die "HelmRelease $ns/$name: cannot count its post-renderers"

  for mode in install upgrade; do
    if [ "$mode" = upgrade ]; then
      set -- --is-upgrade
    else
      set --
    fi
    if [ -n "$kube_version" ]; then
      set -- "$@" --kube-version "$kube_version"
    fi
    if ! "$controller_helm" template "$release" "$tgz" --namespace "$namespace" "${value_args[@]}" "$@" \
      >"$work/$mode.rendered.yaml" 2>"$work/$mode.err"; then
      cannot "its chart does not render as an $mode: $(tr '\n' ' ' <"$work/$mode.err" | head -c 300)"
      return 1
    fi
    # Hooks are not part of the release's manifest, and Flux does not post-render them.
    mkdir -p "$work/$mode" || die "cannot create '$work/$mode'"
    yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$work/$mode.rendered.yaml" \
      >"$work/$mode/resources.yaml" 2>"$work/$mode.err" ||
      die "HelmRelease $ns/$name: cannot read its $mode render: $(head -c 500 "$work/$mode.err")"
    index=0
    while [ "$index" -lt "$count" ]; do
      jq --argjson i "$index" '{apiVersion: "kustomize.config.k8s.io/v1beta1", kind: "Kustomization",
          resources: ["resources.yaml"]} + (.release.spec.postRenderers[$i].kustomize | with_entries(select(.value != null)))' \
        "$record" >"$work/$mode/kustomization.yaml" || die "HelmRelease $ns/$name: cannot build post-renderer $((index + 1))"
      if ! kubectl kustomize "$work/$mode" >"$work/$mode.next.yaml" 2>"$work/$mode.err"; then
        cannot "post-renderer $((index + 1)) of $count does not apply to its $mode render: $(tr '\n' ' ' <"$work/$mode.err" | head -c 300)"
        return 1
      fi
      mv "$work/$mode.next.yaml" "$work/$mode/resources.yaml" || die "cannot stage the output of post-renderer $((index + 1))"
      index=$((index + 1))
    done
    cp "$work/$mode/resources.yaml" "$work/$mode.yaml" || die "cannot keep the $mode render of HelmRelease $ns/$name"
  done
}

: >"$scratch/findings"
: >"$scratch/unknown"
: >"$scratch/used"
clusters=0
total_pdbs=0
total_ok=0
total_excepted=0
total_rendered=0
for overlay in "$root"/clusters/*/; do
  overlay="${overlay%/}"
  cluster="${overlay##*/}"
  # clusters/base is the template the overlays share; its paths still carry placeholders.
  [ "$cluster" != base ] || continue
  [ -f "$overlay/kustomization.yaml" ] || continue
  clusters=$((clusters + 1))
  work="$scratch/$cluster"
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
      /* | .. | ../* | */.. | */../*) die "Flux path '$path' named by '$overlay' leaves '$root'" ;;
    esac
    [ -d "$root/$path" ] || die "Flux path '$path' named by '$overlay' does not exist under '$root'"
    deprecated="$(yq '(.spec.patchesStrategicMerge != null) or (.spec.patchesJson6902 != null)' "$doc")" ||
      die "cannot read the patch fields of the Flux Kustomization for '$path'"
    [ "$deprecated" = false ] ||
      die "the Flux Kustomization for '$path' uses patchesStrategicMerge or patchesJson6902, which this guard does not apply — use spec.patches"
    layers=$((layers + 1))
    rendered="$work/layer-$layers"
    wrap="$rendered.wrap"
    mkdir -p "$wrap" || die "cannot create a scratch directory for '$path'"
    resource="$(relpath "$wrap" "$root/$path")" || die "cannot resolve Flux path '$path'"
    RESOURCE="$resource" yq -N "$wrapper" "$doc" >"$wrap/kustomization.yaml" 2>"$rendered.wrap.err" ||
      die "cannot build the Flux view of '$path': $(head -c 500 "$rendered.wrap.err")"
    yq '.spec.components // [] | .[]' "$doc" >"$rendered.components" 2>"$rendered.components.err" ||
      die "cannot read spec.components for '$path': $(head -c 500 "$rendered.components.err")"
    while IFS= read -r component || [ -n "$component" ]; do
      [ -n "$component" ] || continue
      component_dir="$root/$path/$component"
      [ -d "$component_dir" ] || die "component '$component' named for Flux path '$path' does not exist"
      component_real="$(cd "$component_dir" && pwd -P)" || die "cannot resolve component '$component' for '$path'"
      case $component_real in
        "$root_real" | "$root_real"/*) ;;
        *) die "component '$component' named for Flux path '$path' leaves '$root'" ;;
      esac
      COMPONENT="$(relpath "$wrap" "$component_dir")" yq -i '.components += [strenv(COMPONENT)]' "$wrap/kustomization.yaml" ||
        die "cannot add component '$component' to the Flux view of '$path'"
    done <"$rendered.components"
    kubectl kustomize --load-restrictor LoadRestrictionsNone "$wrap" >"$rendered.yaml" 2>"$rendered.err" ||
      die "cannot render '$root/$path' for cluster '$cluster': $(head -c 500 "$rendered.err")"
    printf '%s\t%s\t%s\n' "$doc" "$rendered" "$path" >>"$work/layers.tsv"
  done
  [ "$layers" -gt 0 ] || die "cluster overlay '$overlay' names no Flux Kustomization"

  # Everything the cluster renders: substitution ConfigMaps are found here.
  # shellcheck disable=SC2046 # one word per rendered layer file, none with spaces
  yq ea -o=json -I=0 '[.] | map(select(. != null))' $(cut -f2 "$work/layers.tsv" | sed 's/$/.yaml/') \
    >"$work/all.json" 2>"$work/all.err" ||
    die "cannot read what cluster '$cluster' renders: $(head -c 500 "$work/all.err")"
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
  # shellcheck disable=SC2046 # one word per layer file, none with spaces
  yq ea -o=json -I=0 '[.] | map(select(. != null))' $(cut -f2 "$work/layers.tsv" | sed 's/$/.resolved.yaml/') \
    >"$work/resolved.json" 2>"$work/resolved.err" ||
    die "cannot collect the substituted cluster '$cluster': $(head -c 500 "$work/resolved.err")"

  # A budget this guard cannot place or read is cannot-check: it must name its namespace, and only
  # policy/v1 selector semantics (a missing selector selects nothing, an empty one everything) are
  # modelled.
  jq -c '[ .[] | select(.kind == "PodDisruptionBudget" and ((.apiVersion // "") | test("^policy/")))
      | {apiVersion, namespace: (.metadata.namespace // ""), name: (.metadata.name // ""), selector: (.spec.selector // null)} ]' \
    "$work/resolved.json" >"$work/pdbs.json" || die "cannot read the PodDisruptionBudgets cluster '$cluster' renders"
  bad="$(jq -r '[ .[] | select(.apiVersion != "policy/v1" or .namespace == "" or .name == "") ] | .[0]
      | if . == null then "" else "\(.apiVersion) PodDisruptionBudget \(.namespace)/\(.name)" end' "$work/pdbs.json")" ||
    die "cannot inspect the PodDisruptionBudgets cluster '$cluster' renders"
  [ -z "$bad" ] ||
    die "cluster '$cluster' renders '$bad', which is not policy/v1 or names no namespace — this guard cannot place it"
  pdbs="$(jq 'length' "$work/pdbs.json")" || die "cannot count the PodDisruptionBudgets cluster '$cluster' renders"
  total_pdbs=$((total_pdbs + pdbs))
  [ "$pdbs" -gt 0 ] || continue

  jq -c --arg namespace '' --arg origin 'a rendered manifest' --arg mode any "$workloads" "$work/resolved.json" \
    >"$work/manifest-workloads.json" || die "cannot read the workloads cluster '$cluster' renders"
  # Only a Canary in a namespace that holds a budget changes a verdict.
  jq -c --slurpfile pdbs "$work/pdbs.json" '[ .[] | select(.kind == "Canary" and ((.apiVersion // "") | test("^flagger[.]app/")))
      | {namespace: (.metadata.namespace // ""), name: .metadata.name,
         kind: (.spec.targetRef.kind // ""), target: (.spec.targetRef.name // "")}
      | select(.namespace as $ns | any($pdbs[0][]; .namespace == $ns)) ]' \
    "$work/resolved.json" >"$work/canaries.json" || die "cannot read the Canaries cluster '$cluster' renders"
  jq -c '[ .[] | select(.kind != null and .metadata.name != null) | "\(.kind)/\(.metadata.namespace // "")/\(.metadata.name)" ]' \
    "$work/resolved.json" >"$work/objects.json" || die "cannot list the objects cluster '$cluster' renders"
  jq -c '[ .[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/")))
      | {namespace: (.spec.targetNamespace // .metadata.namespace // ""), source: (.metadata.namespace // ""),
         name: .metadata.name, id: "\(.metadata.namespace // "")/\(.metadata.name)"} ]' \
    "$work/resolved.json" >"$work/releases.json" || die "cannot read the HelmReleases cluster '$cluster' renders"

  # Flagger rewrites one of these labels on the primary it creates. The flag's default is modelled;
  # the chart passes -selector-labels only when its selectorLabels value is set.
  beside="$(jq 'length' "$work/canaries.json")" || die "cannot count the Canaries beside a PodDisruptionBudget in cluster '$cluster'"
  if [ "$beside" != 0 ]; then
    unsupported="$(jq -r '[ .[] | select(.kind != "Deployment" and .kind != "DaemonSet" and .kind != "Service") ] | .[0]
        | if . == null then "" else "Canary \(.namespace)/\(.name) targets a \(.kind)" end' "$work/canaries.json")" ||
      die "cannot read the Canary targets cluster '$cluster' renders"
    [ -z "$unsupported" ] || die "cluster '$cluster': $unsupported, which is not a kind this guard models a Flagger primary for"
    flagger="$(jq -r '[ .[] | select(.kind == "HelmRelease" and (.spec.chart.spec.chart // "") == "flagger") ]
        | if length == 0 then "absent"
          elif any(.[]; ((.spec.values.selectorLabels // "") != "") or (((.spec.valuesFrom // []) | length) > 0)) then "custom"
          else "default" end' "$work/resolved.json")" || die "cannot read the flagger HelmRelease cluster '$cluster' renders"
    case $flagger in
      default) ;;
      absent) die "cluster '$cluster' renders a Canary beside a PodDisruptionBudget but no flagger HelmRelease, so the labels Flagger rewrites cannot be read" ;;
      *) die "cluster '$cluster': the flagger HelmRelease sets selectorLabels or valuesFrom, and this guard models Flagger's default -selector-labels only" ;;
    esac
  fi

  decide() { # <candidates.json> <releases.json> <exceptions.json> <out>
    jq -n --arg cluster "$cluster" --slurpfile pdbs "$work/pdbs.json" --slurpfile candidates "$1" \
      --slurpfile canaries "$work/canaries.json" --slurpfile releases "$2" --slurpfile objects "$work/objects.json" \
      --slurpfile exceptions "$3" \
      '{cluster: $cluster, pdbs: $pdbs[0], candidates: $candidates[0],
        canaries: [ $canaries[0][] | select(.kind == "Deployment" or .kind == "DaemonSet") ],
        releases: $releases[0], objects: $objects[0], exceptions: $exceptions[0],
        selectorLabels: ["app", "name", "app.kubernetes.io/name"]} | '"$verdict" >"$4" 2>"$4.err" ||
      die "cannot decide the PodDisruptionBudgets of cluster '$cluster': $(head -c 500 "$4.err")"
  }

  # First with the manifests alone and no exception: every namespace holding a budget they do not
  # satisfy has its HelmReleases rendered, so an excepted budget is still compared with them.
  printf '[]\n' >"$work/none.json"
  decide "$work/manifest-workloads.json" "$work/none.json" "$work/none.json" "$work/first.json"
  jq -r --slurpfile pdbs "$work/pdbs.json" '[ .[] | select(.status != "ok") | .key as $key
      | $pdbs[0][] | select("\(.namespace)/\(.name)" == $key) | .namespace ] | unique | .[]' "$work/first.json" \
    >"$work/namespaces" || die "cannot list the namespaces of cluster '$cluster' that need their charts rendered"

  cp "$work/manifest-workloads.json" "$work/candidates.json" || die "cannot stage the workloads of cluster '$cluster'"
  : >"$work/attempted.jsonl"
  while IFS= read -r namespace_needed; do
    [ -n "$namespace_needed" ] || continue
    jq -r --arg ns "$namespace_needed" '.[] | select(.namespace == $ns) | "\(.source)\t\(.name)"' "$work/releases.json" \
      >"$work/installed.tsv" || die "cannot list the HelmReleases installed into namespace '$namespace_needed' of cluster '$cluster'"
    while IFS="$tab" read -r release_ns release_name; do
      [ -n "$release_name" ] || continue
      release_work="$work/releases/${release_ns}__${release_name}"
      if render_release "$work/resolved.json" "$release_ns" "$release_name" "$release_work"; then
        for mode in install upgrade; do
          yq ea -o=json -I=0 '[.] | map(select(. != null))' "$release_work/$mode.yaml" >"$release_work/$mode.json" 2>"$release_work/$mode.err" ||
            die "cannot read the $mode render of HelmRelease $release_ns/$release_name: $(head -c 500 "$release_work/$mode.err")"
          jq -c --arg namespace "$namespace_needed" --arg origin "HelmRelease $release_ns/$release_name" --arg mode "$mode" \
            "$workloads" "$release_work/$mode.json" >"$release_work/$mode.workloads.json" ||
            die "cannot read the workloads HelmRelease $release_ns/$release_name renders"
          jq -c -s 'add' "$work/candidates.json" "$release_work/$mode.workloads.json" >"$work/candidates.next.json" ||
            die "cannot add the workloads HelmRelease $release_ns/$release_name renders"
          mv "$work/candidates.next.json" "$work/candidates.json" ||
            die "cannot keep the workloads HelmRelease $release_ns/$release_name renders"
        done
        jq -cn --arg namespace "$namespace_needed" --arg id "$release_ns/$release_name" \
          '{namespace: $namespace, id: $id, rendered: true, reason: ""}' >>"$work/attempted.jsonl" ||
          die "cannot record HelmRelease $release_ns/$release_name"
        total_rendered=$((total_rendered + 1))
      else
        [ -s "$release_work/reason" ] ||
          die "HelmRelease $release_ns/$release_name could not be rendered and left no reason"
        jq -cn --arg namespace "$namespace_needed" --arg id "$release_ns/$release_name" --rawfile reason "$release_work/reason" \
          '{namespace: $namespace, id: $id, rendered: false, reason: $reason}' >>"$work/attempted.jsonl" ||
          die "cannot record HelmRelease $release_ns/$release_name"
      fi
    done <"$work/installed.tsv"
  done <"$work/namespaces"
  jq -cs '.' "$work/attempted.jsonl" >"$work/attempted.json" || die "cannot collect the HelmReleases rendered for cluster '$cluster'"

  decide "$work/candidates.json" "$work/attempted.json" "$scratch/exceptions.json" "$work/verdict.json"
  # Every budget must come back with one of the four verdicts: one the decision dropped would
  # otherwise read as clean.
  counts="$(jq -r '[length] + [("ok", "excepted", "finding", "unknown") as $s | [ .[] | select(.status == $s) ] | length]
      | map(tostring) | join(" ")' "$work/verdict.json")" || die "cannot count the verdicts of cluster '$cluster'"
  read -r decided ok excepted detached undecided <<<"$counts"
  if [ "$decided" != "$pdbs" ] || [ "$((ok + excepted + detached + undecided))" != "$pdbs" ]; then
    die "cluster '$cluster' renders $pdbs PodDisruptionBudget(s) but $((ok + excepted + detached + undecided)) of $decided verdict(s) are known — refusing to report the rest as clean"
  fi
  jq -r '.[] | select(.status == "ok") | "  ok        " + .text' "$work/verdict.json" ||
    die "cannot report the PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "excepted") | "  excepted  " + .text' "$work/verdict.json" ||
    die "cannot report the excepted PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "excepted") | .key' "$work/verdict.json" >>"$scratch/used" ||
    die "cannot record the exception rows cluster '$cluster' uses"
  jq -r '.[] | select(.status == "finding") | .text' "$work/verdict.json" >>"$scratch/findings" ||
    die "cannot record the detached PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "unknown") | .text' "$work/verdict.json" >>"$scratch/unknown" ||
    die "cannot record the undecided PodDisruptionBudgets of cluster '$cluster'"
  total_ok=$((total_ok + ok))
  total_excepted=$((total_excepted + excepted))
done

[ "$clusters" -gt 0 ] || die "no cluster overlay with a kustomization.yaml under '$root/clusters'"
[ "$total_pdbs" -gt 0 ] ||
  die "no cluster renders any PodDisruptionBudget under '$root' — refusing to report an empty render as clean"

jq -r '.[].key' "$scratch/exceptions.json" >"$scratch/exception-keys" || die "cannot list the exception rows"
while IFS= read -r key || [ -n "$key" ]; do
  [ -n "$key" ] || continue
  grep -qxF -- "$key" "$scratch/used" ||
    printf 'stale exception: PodDisruptionBudget %s is satisfied by a rendered workload, or no cluster renders it\n' "$key" >>"$scratch/findings"
done <"$scratch/exception-keys"

summary="$clusters cluster(s), $total_pdbs PodDisruptionBudget(s), $total_rendered HelmRelease chart(s) rendered"
status=0
if [ -s "$scratch/findings" ]; then
  printf 'guard-pdb-selector-match: PodDisruptionBudgets that select no rendered workload, and exception rows that do not hold:\n' >&2
  sed 's/^/  /' "$scratch/findings" >&2
  printf 'A budget whose selector matches no pod reports expectedPods: 0 and holds back no eviction. Point the selector at the labels of the pod template that serves.\n' >&2
  printf 'A workload nothing here can render is a reviewed row in %s naming the budget, what builds its pods and where its labels were verified; remove a stale row.\n' "$exceptions_file" >&2
  status=1
fi
if [ -s "$scratch/unknown" ]; then
  printf 'guard-pdb-selector-match: PodDisruptionBudgets that cannot be checked:\n' >&2
  sed 's/^/  /' "$scratch/unknown" >&2
  status=2
fi
if [ "$status" != 0 ]; then
  printf 'guard-pdb-selector-match: %s\n' "$summary" >&2
  exit "$status"
fi

printf 'guard-pdb-selector-match: %s: %d select a rendered workload, %d excepted\n' "$summary" "$total_ok" "$total_excepted"
exit 0
