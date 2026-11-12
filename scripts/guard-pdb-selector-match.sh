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
#      `CONTROLLER_HELM` names one already built, and its build metadata must be the audited SDK's.
#      The overlays are rendered and the charts pulled by `scripts/flux-render.lib.sh`, the library
#      the post-renderer guard renders with.
#   3. Flagger. A Canary is read from the manifests and from the install and the upgrade render
#      of every chart rendered. Its target is not what serves: Flagger creates `<target>-primary`, whose pod
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
# checked: only the budgets this repository declares are. A chart is rendered only for a namespace
# holding a budget the manifests alone do not satisfy, so a Canary that only a chart renders, over
# a workload a manifest renders, beside a budget that manifest already satisfies, is not read.
#
# ⚠️ CANNOT-CHECK IS NEVER CLEAN. A budget nothing rendered satisfies, in a namespace holding a
# HelmRelease this guard cannot render the way Flux does, is exit 2: a chart source that needs
# credentials or is not a HelmRepository or OCIRepository, a `valuesFrom` entry without a
# `targetPath`, `valuesFiles`, `upgrade.preserveValues`, an unsupported post-render strategy
# (or none on a post-rendered release), a post-renderer that is not a kustomize `patches`/`images`
# renderer or does not apply, a chart that fails to pull or render, or a chart (or a packaged
# dependency, or a template value) that reads `.Release.Revision`, which an offline render always
# renders as 1. A chart pull is tried
# PDB_GUARD_PULL_ATTEMPTS times (default 3), waiting
# PDB_GUARD_PULL_BACKOFF seconds (default 2) and twice as long after each failure, before it counts
# as failed. A release that cannot be rendered is no obstacle to a budget something else
# satisfies. A Canary whose target nothing renders, or whose primary Flagger could not create, is
# exit 2 the same way. No cluster overlay, a directory under `clusters/` other than `base` that
# holds no kustomization file, an overlay that names no Flux Kustomization, or a tree that renders
# no PodDisruptionBudget at all is exit 2 as well: a selector that matched nothing is
# indistinguishable from a clean tree.
#
# EXCEPTIONS. A budget whose pods are built where nothing here can render them (an operator
# building a workload from its custom resource at runtime, a Deployment the node OS installs, a
# chart this guard cannot render) is allowed only by a reviewed row in
# `scripts/pdb-selector-exceptions.tsv`. A row applies to ONE cluster and names the budget, what
# builds its pods, the version pin its labels were verified at, and where they were verified.
# What a row can and cannot excuse:
#   - It never applies to another cluster: the same budget in a second cluster is judged there.
#   - It is refused (exit 1) when the cluster renders a workload into the budget's namespace that
#     satisfies at least one of the selector's matchLabels, In or Exists requirements: that is a
#     candidate this guard can judge, and it does not match. The one way past that is for the row
#     to name the workload, as <Kind>/<name>, in its reviewed-unrelated column: a reviewer's
#     statement that it is another workload that happens to share a label (a grouping label such
#     as `app.kubernetes.io/part-of`), not the one the budget was written for. A name listed
#     there that no longer applies is refused too. A rendered workload that satisfies none of
#     the requirements is taken to be unrelated without being named; so a row cannot be told
#     apart from a wrong one when the cluster renders the budget's own workload under labels that
#     share nothing with the selector. That is the limit of what a row is checked against.
#   - It is refused when it names a workload kind as the producer (this guard reads those itself),
#     an object the cluster does not render, a HelmRelease this guard can render, or a HelmRelease
#     that is not the unrenderable release of that name installed into the budget's namespace.
#   - It is refused when the budget has no selector: such a budget selects no pod.
#   - It is refused when the pin it was verified at has moved: the version the cluster renders
#     today must equal the verified version in the row, so a bump fails until the labels are
#     verified again at the new version. A pin that cannot be read is exit 2.
#   - A row for a budget that a rendered workload satisfies, that its cluster does not render, or
#     for a cluster that is no overlay here, is itself a violation.
#
#
# WHICH BUDGETS. Rendering a chart means pulling it from its registry, so with
# `--base <git-revision>` only the budgets whose inputs changed since that revision have their
# charts rendered. A budget's inputs are everything its verdict is read from: the budgets of its
# namespace, the workloads and Canaries the manifests render there, every HelmRelease installed
# into it (after substitution, with its chart source and selected ConfigMap values), the flagger
# HelmRelease, the exception rows of those budgets with what their pins read and whether their
# producers are rendered, and the Kubernetes version (`--base-kube-version` gives the base
# revision's, and is required with `--kube-version`). A budget the manifests do not satisfy and
# whose inputs are unchanged is reported `unchanged`, with no chart pulled; one the manifests
# satisfy is judged as always. Every budget is judged in full, as without `--base`, when this
# guard, the shared rendering library, the exception file, the renderer's sources or a file an
# exception row is pinned to (the production cluster configuration among them) differ from the
# base, or when the base tree cannot be rendered: no change to how budgets are judged is ever
# taken on trust from the base.
#
# Usage: guard-pdb-selector-match.sh [--base <git-revision>] [--kube-version <version> [--base-kube-version <version>]] <k8s-root>
#
# Exit codes:
#   0  every rendered PodDisruptionBudget selects a rendered workload, carries a reviewed exception,
#      or (with --base) is unchanged since the base
#   1  at least one PodDisruptionBudget selects no rendered workload, or an exception row is stale,
#      names a producer that does not hold, or was verified at a version that has since moved
#   2  cannot check: bad usage, a missing root, tool or exceptions file, a render or parse failure,
#      a budget that cannot be decided, a malformed exception row, or an anti-vacuity failure.
#      Budgets found detached in the same run are still listed.

set -uo pipefail

die() {
  printf 'guard-pdb-selector-match: %s\n' "$*" >&2
  exit 2
}

usage="usage: $0 [--base <git-revision>] [--kube-version <version> [--base-kube-version <version>]] <k8s-root>"
base=''
kube_version=''
base_kube_version=''
while [ "$#" -gt 0 ]; do
  case $1 in
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
if [ -n "$base" ] && [ -n "$kube_version" ]; then
  [ -n "$base_kube_version" ] || die "--base-kube-version is required when comparing an explicit Kubernetes version"
fi
if [ -n "$base_kube_version" ]; then
  if [ -z "$base" ] || [ -z "$kube_version" ]; then
    die "--base-kube-version requires --base and --kube-version"
  fi
fi
root="${1%/}"
[ -d "$root/clusters" ] || die "'$root/clusters' is not a directory"
for tool in jq kubectl yq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done
if [ -n "$base" ]; then
  command -v git >/dev/null 2>&1 || die "git is required for --base but not installed"
fi
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || die "cannot resolve the script directory"
# shellcheck source=scripts/flux-render.lib.sh
source "$script_dir/flux-render.lib.sh"

exceptions_file="${PDB_SELECTOR_EXCEPTIONS:-$script_dir/pdb-selector-exceptions.tsv}"
[ -f "$exceptions_file" ] ||
  die "exceptions file '$exceptions_file' not found — refusing to run without the reviewed disposition list"

scratch="$(mktemp -d)" || die "cannot create a scratch directory"
trap 'rm -rf "$scratch"' EXIT

# Charts are pulled anonymously, with no ambient Helm or Docker configuration, by the controller
# renderer, which also renders them. A failed pull is tried PDB_GUARD_PULL_ATTEMPTS times.
flux_render_init "$scratch"
flux_render_pull_policy "${PDB_GUARD_PULL_ATTEMPTS:-3}" "${PDB_GUARD_PULL_BACKOFF:-2}" \
  PDB_GUARD_PULL_ATTEMPTS PDB_GUARD_PULL_BACKOFF
controller_helm="${CONTROLLER_HELM:-}"

tab="$flux_render_tab"

field() { # <row> <n>
  printf '%s\n' "$1" | awk -F '\t' -v n="$2" '{ print $n }'
}

# A malformed row is exit 2: a row this guard cannot read is a disposition nobody can audit, and
# ignoring it would let a detached budget pass on a row that says nothing.
dns_label='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
dns_name='[a-z0-9]([-a-z0-9.]*[a-z0-9])?'
: >"$scratch/exceptions.jsonl"
lineno=0
while IFS= read -r row || [ -n "$row" ]; do
  lineno=$((lineno + 1))
  case $row in '' | '#'*) continue ;; esac
  columns="$(printf '%s\n' "$row" | awk -F '\t' '{ print NF }')"
  [ "$columns" = 7 ] ||
    die "$exceptions_file:$lineno: a row has seven tab-separated columns (cluster, budget, producer, pin, verified version, unrelated workloads, reason), this one has $columns"
  row_cluster="$(field "$row" 1)"
  budget="$(field "$row" 2)"
  producer="$(field "$row" 3)"
  pin="$(field "$row" 4)"
  verified="$(field "$row" 5)"
  unrelated="$(field "$row" 6)"
  reason="$(field "$row" 7)"
  printf '%s' "$row_cluster" | grep -Eq "^$dns_label\$" ||
    die "$exceptions_file:$lineno: column 1 must name the cluster the row applies to, got '$row_cluster'"
  [ "$row_cluster" != base ] ||
    die "$exceptions_file:$lineno: column 1 names 'base', which is the template the cluster overlays share, not a cluster"
  printf '%s' "$budget" | grep -Eq "^$dns_label/$dns_name\$" ||
    die "$exceptions_file:$lineno: column 2 must name a PodDisruptionBudget as <namespace>/<name>, got '$budget'"
  printf '%s' "$producer" | grep -Eq "^(external:$dns_label|[A-Z][A-Za-z0-9]*/$dns_label/$dns_name)\$" ||
    die "$exceptions_file:$lineno: '$budget' names no producer (column 3 must be <Kind>/<namespace>/<name> of a rendered object, or external:<what>, got '$producer')"
  printf '%s' "$pin" | grep -Eq "^(HelmRelease/$dns_label/$dns_name|file:[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*:([.][A-Za-z_][A-Za-z0-9_-]*)+)\$" ||
    die "$exceptions_file:$lineno: '$budget' names no version pin (column 4 must be HelmRelease/<namespace>/<name>, whose chart version is read, or file:<path>:<.dotted.path> beside the k8s root, got '$pin')"
  printf '%s' "$verified" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._+-]*$' ||
    die "$exceptions_file:$lineno: '$budget' carries no verified version (column 5, the value of the pin when its labels were verified, got '$verified')"
  printf '%s' "$unrelated" | grep -Eq "^(-|[A-Z][A-Za-z0-9]*/$dns_name(,[A-Z][A-Za-z0-9]*/$dns_name)*)\$" ||
    die "$exceptions_file:$lineno: '$budget' names no reviewed-unrelated workloads (column 6 must be '-' for none, or <Kind>/<name>[,<Kind>/<name>...] of workloads rendered into the budget's namespace, got '$unrelated')"
  [ -n "$reason" ] || die "$exceptions_file:$lineno: '$budget' carries no reason (column 7)"
  [ "$(jq -r --arg cluster "$row_cluster" --arg key "$budget" 'select(.cluster == $cluster and .key == $key) | .key' \
    "$scratch/exceptions.jsonl" | grep -c .)" = 0 ] ||
    die "$exceptions_file:$lineno: '$budget' is listed more than once for cluster '$row_cluster'"
  jq -cn --arg cluster "$row_cluster" --arg key "$budget" --arg producer "$producer" --arg pin "$pin" \
    --arg verified "$verified" --arg unrelated "$unrelated" --arg reason "$reason" \
    '{cluster: $cluster, key: $key, producer: $producer, pin: $pin, verified: $verified, reason: $reason,
      unrelated: (if $unrelated == "-" then [] else ($unrelated | split(",")) end)}' \
    >>"$scratch/exceptions.jsonl" || die "$exceptions_file:$lineno: cannot record the row for '$budget'"
done <"$exceptions_file"
jq -cs '.' "$scratch/exceptions.jsonl" >"$scratch/exceptions.json" || die "cannot read the exception rows"

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
release_record="$flux_render_release_inputs"'
  . as $all
  | [ .[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
      and (.metadata.namespace // "") == $ns and .metadata.name == $name) ]
  | if length != 1 then error("expected exactly one HelmRelease \($ns)/\($name), found \(length)") else .[0] end
  | release_inputs($all)'

# The Canaries among rendered objects that sit beside a budget: only those change a verdict. One a
# chart renders that names no namespace takes the namespace the chart installs into.
# shellcheck disable=SC2016 # jq variables, not shell expansions
canaries='[ .[] | select(.kind == "Canary" and ((.apiVersion // "") | test("^flagger[.]app/")))
  | {namespace: (.metadata.namespace // $namespace), name: (.metadata.name // ""),
     kind: (.spec.targetRef.kind // ""), target: (.spec.targetRef.name // ""), origin: $origin, mode: $mode}
  | select(.namespace as $ns | any($pdbs[0][]; .namespace == $ns)) ]'

# Decides every PodDisruptionBudget of one cluster. Input: {cluster, pdbs, candidates, canaries,
# releases, objects, exceptions, selectorLabels}. Output: one {status, key, row, text} per budget,
# status being ok, excepted, finding or unknown, and row whether an exception row was applied to
# it (accepted or refused).
# shellcheck disable=SC2016 # jq variables, not shell expansions
verdict='
  def modes: ["install", "upgrade"];
  def workload_kinds: ["Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Rollout"];
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
  # How many requirements that NAME a label the pods must carry (matchLabels, In, Exists) a label
  # set satisfies. NotIn and DoesNotExist hold for every workload that lacks the label, so they
  # say nothing about whether a workload is the one a budget was written for.
  def named($labels):
    [ ((.matchLabels // {}) | to_entries[] | ($labels[.key] != null and $labels[.key] == (.value | tostring))),
      ((.matchExpressions // [])[] | select(.operator == "In" or .operator == "Exists") | . as $e
        | ($labels | has($e.key)) and ($e.operator == "Exists"
            or any((($e.values // []) | map(tostring))[]; . == $labels[$e.key]))) ]
    | map(select(.)) | length;
  def describe:
    [ ((.matchLabels // {}) | to_entries[] | "\(.key)=\(.value)"),
      ((.matchExpressions // [])[] | "\(.key) \(.operator)"
        + (if ((.values // []) | length) > 0 then " (" + (.values | map(tostring) | join(",")) + ")" else "" end)) ]
    | if length == 0 then "{} (every pod in the namespace)" else join(", ") end;

  . as $d
  # Flagger: per Canary and render, the target it scales to 0 and the primary it creates, or why
  # that cannot be derived. A Canary a chart renders exists in the render it came from.
  | [ $d.canaries[] | . as $c | modes[] as $m | select($c.mode == "any" or $c.mode == $m)
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
          {status: "ok", key: $key, row: false,
           text: "\($label) selects \($renders[0].hits[0].kind) \($renders[0].hits[0].name) (\($renders[0].hits[0].origin))"}
        else
          $missing[0] as $m
          | (if $p.selector == null then 0 else ($p.selector | (.matchLabels // {} | length) + (.matchExpressions // [] | length)) end) as $total
          | ([ live($p.namespace; $m)[], idle($p.namespace; $m)[] ]
              | map(. as $w | . + {score: (if $p.selector == null then 0 else ($p.selector | satisfied($w.labels) | map(select(.)) | length) end)})
              | sort_by([(if .idle != null then 1 else 0 end), -.score, .kind, .name])) as $near
          | ([ $near[] | select(.idle != null and .score == $total and $p.selector != null) ]) as $idle_hits
          | (if $p.selector == null then "has no selector, and a budget without one selects no pod"
             else "selects no workload rendered into namespace \($p.namespace)"
               + (if ($missing | length) == 1 then " in the \($m) render" else "" end) end) as $headline
          | ((if $p.selector == null then "" else "\n    selector: \($p.selector | describe)" end)
             + (if ($idle_hits | length) > 0 then
                  "\n    it matches only \($idle_hits[0].kind) \($idle_hits[0].name), which Flagger Canary \($idle_hits[0].idle) keeps at 0 replicas between rollouts; the pods that serve belong to \($idle_hits[0].name)-primary"
                else "" end)
             + (if ($near | length) == 0 then "\n    nearest:  no workload is rendered into namespace \($p.namespace)"
                else "\n    nearest:  " + ([ $near[0:3][] | "\(.kind) \(.name) (\(.origin)): \(.labels | pairs) — \(.score) of \($total) selector requirement(s) match"
                  + (if .idle != null then ", scaled to 0 by Flagger" else "" end) ] | join("\n              ")) end)) as $rest
          | "\($label) \($headline)\($rest)" as $detail
          | ([ $d.releases[] | select(.namespace == $p.namespace and (.rendered | not))
                | "HelmRelease \(.id) cannot be rendered (\(.reason))" ]
             + ([ $flagger[] | select(.unknown != null and .namespace == $p.namespace) | .unknown ] | unique)) as $blind
          # The rendered workloads this budget was plausibly written for: in any render that does
          # not satisfy it, one that carries at least one label the selector names.
          | ([ $missing[] as $mm | (live($p.namespace; $mm)[], idle($p.namespace; $mm)[])
                | . as $w | select($p.selector != null and ($p.selector | named($w.labels)) > 0)
                | {kind, name, origin} ] | unique) as $judged
          | ([ $d.exceptions[] | select(.key == $key) ] | .[0]) as $x
          | if $x == null then
              if ($blind | length) > 0 then
                # The cause leads: a reader of an exit 2 needs what blocked the check first.
                {status: "unknown", key: $key, row: false,
                 text: "\($label) cannot be checked: \($blind | join("; "))\n    what could be rendered: it \($headline)\($rest)"}
              else {status: "finding", key: $key, row: false, text: $detail} end
            else
              ($x.producer | split("/")[0]) as $producer_kind
              | ([ $d.releases[] | select(.namespace == $p.namespace and "HelmRelease/" + .id == $x.producer) ] | .[0]) as $release
              | ([ $judged[] | select(. as $w | ($x.unrelated | index("\($w.kind)/\($w.name)")) == null) ]) as $unreviewed
              | ([ $x.unrelated[] | select(. as $u | any($judged[]; "\(.kind)/\(.name)" == $u) | not) ]) as $unneeded
              | (if $p.selector == null then
                   "its exception cannot stand: a budget without a selector selects no pod, whatever builds them"
                 elif ($unreviewed | length) > 0 then
                   "its exception cannot stand: cluster \($d.cluster) renders \($unreviewed[0].kind) \($unreviewed[0].name) (\($unreviewed[0].origin)), which carries part of what this selector names — an exception never stands in for a workload this guard can judge"
                 elif ($unneeded | length) > 0 then
                   "its exception lists \($unneeded[0]) as a reviewed unrelated workload, but cluster \($d.cluster) renders no such workload carrying part of this selector into namespace \($p.namespace): remove it from the row"
                 elif (workload_kinds | index($producer_kind)) != null then
                   "its exception names \($x.producer), a workload this guard reads itself: a rendered workload is judged, never excepted"
                 elif ($x.producer | startswith("external:")) then null
                 elif ($d.objects | index($x.producer)) == null then
                   "its exception names \($x.producer), which cluster \($d.cluster) does not render"
                 elif $producer_kind != "HelmRelease" then null
                 elif $release == null then
                   "its exception names \($x.producer), which is not installed into namespace \($p.namespace)"
                 elif $release.rendered then
                   "its exception names \($x.producer), which renders here: an exception cannot stand in for a rendered mismatch"
                 else null end) as $refusal
              | if $refusal != null then
                  {status: "finding", key: $key, row: true, text: "\($detail)\n    \($refusal)"}
                elif $x.observed != $x.verified then
                  {status: "finding", key: $key, row: true,
                   text: "\($detail)\n    its exception was verified at \($x.verified), and \($x.pin) now reads \($x.observed): verify the pod labels again at the source the row names, then set the verified version of the row to \($x.observed)"}
                else
                  {status: "excepted", key: $key, row: true,
                   text: ("\($label) is excepted: its pods are built by \($x.producer)"
                     + (if $release != null then ", which cannot be rendered (\($release.reason))" else "" end)
                     + " (\($x.reason)); verified at \($x.pin) \($x.verified)")}
                end
            end
        end ]'

# Renders one HelmRelease's chart as an install and an upgrade and applies its post-renderers,
# leaving <work>/install.yaml and <work>/upgrade.yaml. Returns 1, with the reason in
# <work>/reason, when the release cannot be rendered the way Flux does.
render_release() { # <resolved.json> <namespace> <name> <work>
  local resolved="$1" ns="$2" name="$3" work="$4" record pull error ref repo version tgz release namespace
  local mode hook_strategy label="HelmRelease $2/$3"
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
  # Combined evaluates hooks in the same stream, then excludes lifecycle hooks
  # from the ordinary workloads a budget can protect. Missing strategies stay
  # unknown when a post-renderer exists; separate streams are not modeled.
  if ! jq -e '(.release.spec.postRenderStrategy | IN("nohooks", "combined")) or
      (.release.spec.postRenderStrategy == null and ((.release.spec.postRenderers // []) | length == 0))' \
    "$record" >/dev/null; then
    cannot "spec.postRenderStrategy is not modelled (a post-rendered release must set explicit nohooks or combined)"
    return 1
  fi
  hook_strategy="$(jq -r '.release.spec.postRenderStrategy // "nohooks"' "$record")" || die 'cannot read the post-render strategy'
  if ! jq -e '.release.spec.upgrade.preserveValues != true' "$record" >/dev/null; then
    cannot "spec.upgrade.preserveValues needs historical release values"
    return 1
  fi
  if ! jq -e '((.release.spec.chart.spec.valuesFiles // []) | length == 0) and (.release.spec.chart.spec.valuesFile == null)' \
    "$record" >/dev/null; then
    cannot "spec.chart.spec.valuesFiles is not rendered"
    return 1
  fi
  flux_render_values "$record" "$work" "$work/reason" "$label" || return 1

  pull="$(jq -c "$flux_render_chart_source" "$record")" || die "$label: cannot resolve its chart"
  error="$(jq -r '.error // ""' <<<"$pull")"
  if [ -n "$error" ]; then
    cannot "$error"
    return 1
  fi
  ref="$(jq -r '.ref' <<<"$pull")"
  repo="$(jq -r '.repo' <<<"$pull")"
  version="$(jq -r '.version' <<<"$pull")"
  flux_render_ensure_renderer
  tgz="$(flux_render_pull_chart "$work/reason" "$controller_helm" "$ref" "$repo" "$version")" || return 1
  flux_render_refuse_revision "$record" "$tgz" "$work" "$ref${version:+@$version}" "$work/reason" || return 1

  release="$(flux_render_release_name "$record" "$label")" || exit 2
  namespace="$(jq -r '.release.spec.targetNamespace // .release.metadata.namespace' "$record")"

  for mode in install upgrade; do
    if [ "$mode" = upgrade ]; then
      set -- --is-upgrade
    else
      set --
    fi
    if [ -n "$kube_version" ]; then
      set -- "$@" --kube-version "$kube_version"
    fi
    if ! HELM_GUARD_CAPTURE="$work/$mode.postrender-input.yaml" "$controller_helm" template "$release" "$tgz" --namespace "$namespace" "${flux_render_value_args[@]}" "$@" \
      --post-renderer guard-input-capture \
      >"$work/$mode.rendered.yaml" 2>"$work/$mode.err"; then
      cannot "its chart does not render as an $mode: $(tr '\n' ' ' <"$work/$mode.err" | head -c 300)"
      return 1
    fi
    if [ ! -s "$work/$mode.postrender-input.yaml" ]; then
      cannot "the SDK did not capture its $mode post-renderer input"
      return 1
    fi
    mkdir -p "$work/$mode" || die "cannot create '$work/$mode'"
    if [ "$hook_strategy" = combined ]; then
      yq 'select(tag == "!!map")' "$work/$mode.postrender-input.yaml" >"$work/$mode/resources.yaml" ||
        die "HelmRelease $ns/$name: cannot collect its combined $mode render"
    else
      yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$work/$mode.postrender-input.yaml" \
        >"$work/$mode/resources.yaml" 2>"$work/$mode.err" ||
        die "HelmRelease $ns/$name: cannot read its $mode render: $(head -c 500 "$work/$mode.err")"
    fi
    if ! flux_render_apply_post_renderers "$record" "$work/$mode" "$label"; then
      cannot "post-renderer $flux_render_failed_at of $flux_render_failed_of does not apply to its $mode render: $(tr '\n' ' ' <"$work/$mode/error" | head -c 300)"
      return 1
    fi
    if [ "$hook_strategy" = combined ]; then
      yq 'select(tag == "!!map") | select(.metadata.annotations["helm.sh/hook"] == null)' "$work/$mode/resources.yaml" >"$work/$mode.yaml" ||
        die "cannot retain the ordinary $mode workloads of HelmRelease $ns/$name"
    else
      cp "$work/$mode/resources.yaml" "$work/$mode.yaml" || die "cannot keep the $mode render of HelmRelease $ns/$name"
    fi
  done
}

# Renders one cluster overlay of <tree> and reads what its budgets are judged on from the
# manifests: the budgets, the workloads, the Canaries beside a budget, every object and every
# HelmRelease. Leaves <work>/pdb-count, and the rest only when the cluster renders a budget.
read_cluster() { # <tree> <overlay> <work>
  local tree="$1" overlay="$2" work="$3" cluster bad pdbs
  cluster="${overlay##*/}"
  flux_render_cluster "$tree" "$overlay" "$work"
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
  printf '%s\n' "$pdbs" >"$work/pdb-count"
  [ "$pdbs" -gt 0 ] || return 0

  jq -c --arg namespace '' --arg origin 'a rendered manifest' --arg mode any "$workloads" "$work/resolved.json" \
    >"$work/manifest-workloads.json" || die "cannot read the workloads cluster '$cluster' renders"
  jq -c --slurpfile pdbs "$work/pdbs.json" --arg namespace '' --arg origin 'a rendered manifest' --arg mode any \
    "$canaries" "$work/resolved.json" >"$work/canaries.json" || die "cannot read the Canaries cluster '$cluster' renders"
  jq -c '[ .[] | select(.kind != null and .metadata.name != null) | "\(.kind)/\(.metadata.namespace // "")/\(.metadata.name)" ]' \
    "$work/resolved.json" >"$work/objects.json" || die "cannot list the objects cluster '$cluster' renders"
  jq -c '[ .[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/")))
      | {namespace: (.spec.targetNamespace // .metadata.namespace // ""), source: (.metadata.namespace // ""),
         name: .metadata.name, id: "\(.metadata.namespace // "")/\(.metadata.name)"} ]' \
    "$work/resolved.json" >"$work/releases.json" || die "cannot read the HelmReleases cluster '$cluster' renders"
}

# The exception rows of one cluster, each with what its pin reads today in <tree>, into
# <work>/exceptions.json. A pin this guard cannot read is cannot-check: a row whose version nobody
# can compare says nothing.
read_pins() { # <tree> <work> <cluster>
  local tree="$1" work="$2" cluster="$3" exception exception_key pin observed pin_file pin_path
  : >"$work/exceptions.jsonl"
  jq -c --arg cluster "$cluster" '.[] | select(.cluster == $cluster)' "$scratch/exceptions.json" >"$work/rows.jsonl" ||
    die "cannot read the exception rows of cluster '$cluster'"
  while IFS= read -r exception; do
    [ -n "$exception" ] || continue
    exception_key="$(jq -r '.key' <<<"$exception")" || die "cannot read an exception row of cluster '$cluster'"
    pin="$(jq -r '.pin' <<<"$exception")" || die "cannot read the pin of the exception row for '$exception_key'"
    case $pin in
      HelmRelease/*)
        observed="$(jq -r --arg pin "$pin" '[ .[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
              and "HelmRelease/\(.metadata.namespace // "")/\(.metadata.name)" == $pin) ]
            | if length != 1 then error("cluster renders \(length) of it")
              else (.[0].spec.chart.spec.version // "") | if type == "string" then . else tostring end end' \
          "$work/resolved.json" 2>"$work/pin.err")" ||
          die "the exception row for '$exception_key' in cluster '$cluster' is pinned to $pin, whose chart version cannot be read: $(head -c 300 "$work/pin.err")"
        [ -n "$observed" ] ||
          die "the exception row for '$exception_key' in cluster '$cluster' is pinned to $pin, which sets no spec.chart.spec.version"
        ;;
      file:*)
        pin_file="${pin#file:}"
        pin_path="${pin_file##*:}"
        pin_file="${pin_file%:*}"
        [ -f "$tree/../$pin_file" ] ||
          die "the exception row for '$exception_key' in cluster '$cluster' is pinned to '$pin_file' beside '$tree', which does not exist"
        # The path was validated as plain dotted keys when the row was read.
        observed="$(yq -r "$pin_path | select(tag == \"!!str\" or tag == \"!!int\" or tag == \"!!float\")" "$tree/../$pin_file" 2>"$work/pin.err")" ||
          die "the exception row for '$exception_key' in cluster '$cluster' is pinned to $pin, which cannot be read: $(head -c 300 "$work/pin.err")"
        [ -n "$observed" ] ||
          die "the exception row for '$exception_key' in cluster '$cluster' is pinned to $pin, which holds no version"
        ;;
      *) die "the exception row for '$exception_key' in cluster '$cluster' names a pin this guard cannot read: '$pin'" ;;
    esac
    case $observed in
      *\$\{*) die "the exception row for '$exception_key' in cluster '$cluster' is pinned to $pin, which still carries a \${...} substitution" ;;
    esac
    jq -c --arg observed "$observed" '. + {observed: $observed}' <<<"$exception" >>"$work/exceptions.jsonl" ||
      die "cannot record the pin of the exception row for '$exception_key'"
  done <"$work/rows.jsonl"
  jq -cs '.' "$work/exceptions.jsonl" >"$work/exceptions.json" || die "cannot collect the exception rows of cluster '$cluster'"
}

# Everything a budget's verdict is read from, per namespace holding a budget: one line
# `<namespace>\t<canonical JSON>` in <work>/inputs.tsv. Two revisions whose lines for a namespace
# are equal decide its budgets the same way.
# shellcheck disable=SC2016 # jq variables, not shell expansions
budget_inputs="$flux_render_release_inputs"'
  . as $all
  | [ $pdbs[0][] | .namespace ] | unique[] as $ns
  | { cluster: $cluster, kube: $kube, namespace: $ns,
      budgets: ([ $pdbs[0][] | select(.namespace == $ns) ] | sort_by(.name)),
      workloads: ([ $workloads[0][] | select(.namespace == $ns) ] | sort_by(.kind, .name)),
      canaries: ([ $canaries[0][] | select(.namespace == $ns) ] | sort_by(.name)),
      releases: ([ $all[] | select(.kind == "HelmRelease" and ((.apiVersion // "") | test("^helm[.]toolkit[.]fluxcd[.]io/"))
          and (.spec.targetNamespace // .metadata.namespace // "") == $ns) | release_inputs($all) ]
        | sort_by(.release.metadata.namespace, .release.metadata.name)),
      flagger: ([ $all[] | select(.kind == "HelmRelease" and (.spec.chart.spec.chart // "") == "flagger") ]
        | sort_by(.metadata.namespace, .metadata.name)),
      exceptions: ([ $exceptions[0][] | select(.key | startswith($ns + "/")) | . as $x
          | . + {producerRendered: (($objects[0] | index($x.producer)) != null)} ] | sort_by(.key)) }
  | "\($ns)\t\(tojson)"'
read_budget_inputs() { # <work> <cluster> <kube-version>
  jq -r -S --arg cluster "$2" --arg kube "$3" --slurpfile pdbs "$1/pdbs.json" --slurpfile workloads "$1/manifest-workloads.json" \
    --slurpfile canaries "$1/canaries.json" --slurpfile exceptions "$1/exceptions.json" --slurpfile objects "$1/objects.json" \
    "$budget_inputs" "$1/resolved.json" >"$1/inputs.tsv" 2>"$1/inputs.err" ||
    die "cannot read what the budgets of cluster '$2' are judged on: $(head -c 500 "$1/inputs.err")"
}

# --- The base revision -----------------------------------------------------------------------
# With --base, the budgets' inputs at that revision, or the reason every budget is judged in full.
base_usable=0
if [ -n "$base" ]; then
  git -C "$root" rev-parse --verify --quiet "${base}^{commit}" >/dev/null ||
    die "base revision '$base' is not available; the checkout must fetch it"
  toplevel="$(git -C "$root" rev-parse --show-toplevel)" || die "'$root' is not inside a git repository"
  toplevel_real="$(cd "$toplevel" && pwd -P)" || die "cannot resolve '$toplevel'"
  prefix="$(git -C "$root" rev-parse --show-prefix)" || die "'$root' is not inside a git repository"
  prefix="${prefix%/}"
  # Prints the path of <file> inside the repository, or nothing when it lies outside: a file
  # outside is the same file for both revisions.
  in_repository() { # <file>
    local real
    real="$(cd "$(dirname "$1")" && pwd -P)/$(basename "$1")" || return 1
    case $real in "$toplevel_real"/*) printf '%s\n' "${real#"$toplevel_real"/}" ;; esac
  }
  # Succeeds when <path> differs from the base: absent there, changed, or untracked. A failed
  # comparison counts as a change.
  changed_since_base() { # <path inside the repository>
    local diff others
    git -C "$toplevel" cat-file -e "${base}:$1" 2>/dev/null || return 0
    diff="$(git -C "$toplevel" diff --name-only "$base" -- "$1")" || return 0
    others="$(git -C "$toplevel" ls-files --others --exclude-standard -- "$1")" || return 0
    [ -n "$diff$others" ]
  }
  full_reason=''
  # What decides how budgets are judged, rather than what they are: this guard, the library it
  # renders with, its exception rows and the renderer's sources.
  for judge in "$script_dir/guard-pdb-selector-match.sh" "$script_dir/flux-render.lib.sh" "$exceptions_file" \
    "$script_dir/build-controller-helm.sh" "$script_dir/guard-controller-helm-profile.sh" \
    "$script_dir/prepare-helm-render-input-capture.sh" "$script_dir/render-helm-chart"; do
    judge_path="$(in_repository "$judge")" || die "cannot resolve '$judge'"
    [ -n "$judge_path" ] || continue
    if changed_since_base "$judge_path"; then
      full_reason="$judge_path differs from $base"
      break
    fi
  done
  # The files beside the k8s root an exception row is pinned to, the production cluster
  # configuration among them, travel with the base tree.
  base_paths=("${prefix:-.}")
  while IFS= read -r pin_file; do
    [ -n "$pin_file" ] || continue
    [ -f "$root/../$pin_file" ] || continue # the head run refuses the row
    pin_repo_path="$(in_repository "$root/../$pin_file")" || die "cannot resolve '$pin_file'"
    if [ -z "$pin_repo_path" ]; then
      [ -n "$full_reason" ] || full_reason="an exception row is pinned to $pin_file, which is outside the repository"
      continue
    fi
    base_paths+=("$pin_repo_path")
  done < <(jq -r '.[] | .pin | select(startswith("file:")) | ltrimstr("file:") | sub(":[^:]*$"; "")' "$scratch/exceptions.json")
  if [ -z "$full_reason" ]; then
    mkdir -p "$scratch/base-tree" || die "cannot create the base tree directory"
    if ! git -C "$toplevel" archive --format=tar "$base" -- "${base_paths[@]}" >"$scratch/base.tar" 2>"$scratch/base.err"; then
      full_reason="$base holds no $(printf '%s ' "${base_paths[@]}")"
    elif ! tar -x -C "$scratch/base-tree" -f "$scratch/base.tar"; then
      die "cannot unpack the base tree at $base"
    elif ! (
      base_root="$scratch/base-tree${prefix:+/$prefix}"
      [ -d "$base_root/clusters" ] || die "'$base_root/clusters' is not a directory"
      for overlay in "$base_root"/clusters/*/; do
        overlay="${overlay%/}"
        flux_render_is_cluster "$overlay" || continue
        work="$scratch/base/${overlay##*/}"
        read_cluster "$base_root" "$overlay" "$work"
        [ "$(cat "$work/pdb-count")" -gt 0 ] || continue
        read_pins "$base_root" "$work" "${overlay##*/}"
        read_budget_inputs "$work" "${overlay##*/}" "$base_kube_version"
      done
    ) 2>"$scratch/base.err"; then
      full_reason="the tree at $base cannot be rendered ($(tr '\n' ' ' <"$scratch/base.err" | head -c 300))"
    else
      base_usable=1
    fi
  fi
  if [ "$base_usable" = 0 ]; then
    printf 'guard-pdb-selector-match: %s; judging every PodDisruptionBudget in full\n' "$full_reason"
  fi
fi

# --- The tree --------------------------------------------------------------------------------
: >"$scratch/findings"
: >"$scratch/unknown"
: >"$scratch/used"
: >"$scratch/clusters"
clusters=0
total_pdbs=0
total_ok=0
total_excepted=0
total_unchanged=0
total_rendered=0
for overlay in "$root"/clusters/*/; do
  overlay="${overlay%/}"
  flux_render_is_cluster "$overlay" || continue
  cluster="${overlay##*/}"
  clusters=$((clusters + 1))
  printf '%s\n' "$cluster" >>"$scratch/clusters"
  work="$scratch/$cluster"
  read_cluster "$root" "$overlay" "$work"
  pdbs="$(cat "$work/pdb-count")" || die "cannot count the PodDisruptionBudgets cluster '$cluster' renders"
  total_pdbs=$((total_pdbs + pdbs))
  [ "$pdbs" -gt 0 ] || continue

  # Flagger rewrites one of these labels on the primary it creates. The flag's default is modelled;
  # the chart passes -selector-labels only when its selectorLabels value is set.
  # It runs on the manifests' Canaries before any chart is pulled, and again once the charts'
  # own Canaries have joined them.
  check_canaries() {
    local beside unsupported flagger
    beside="$(jq 'length' "$work/canaries.json")" || die "cannot count the Canaries beside a PodDisruptionBudget in cluster '$cluster'"
    [ "$beside" != 0 ] || return 0
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
  }
  check_canaries

  read_pins "$root" "$work" "$cluster"
  # A namespace whose budgets are judged on what they were at the base is not rendered again.
  : >"$work/inputs.tsv"
  if [ "$base_usable" = 1 ]; then
    read_budget_inputs "$work" "$cluster" "$kube_version"
  fi
  : >"$work/unchanged"

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
    # Only a namespace with a chart to pull is worth skipping: one without is judged in full.
    if [ "$base_usable" = 1 ] && [ -s "$work/installed.tsv" ] && [ -f "$scratch/base/$cluster/inputs.tsv" ]; then
      head_inputs="$(awk -F '\t' -v ns="$namespace_needed" '$1 == ns' "$work/inputs.tsv")" ||
        die "cannot read the inputs of namespace '$namespace_needed' in cluster '$cluster'"
      base_inputs="$(awk -F '\t' -v ns="$namespace_needed" '$1 == ns' "$scratch/base/$cluster/inputs.tsv")" ||
        die "cannot read the base inputs of namespace '$namespace_needed' in cluster '$cluster'"
      if [ -n "$head_inputs" ] && [ "$head_inputs" = "$base_inputs" ]; then
        printf '%s\n' "$namespace_needed" >>"$work/unchanged"
        continue
      fi
    fi
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
          # A chart can render the Canary over its own workload. Left unread, the budget would be
          # judged against the target Flagger scales to 0 instead of the primary that serves.
          jq -c --slurpfile pdbs "$work/pdbs.json" --arg namespace "$namespace_needed" \
            --arg origin "HelmRelease $release_ns/$release_name" --arg mode "$mode" \
            "$canaries" "$release_work/$mode.json" >"$release_work/$mode.canaries.json" ||
            die "cannot read the Canaries HelmRelease $release_ns/$release_name renders"
          jq -c -s 'add' "$work/canaries.json" "$release_work/$mode.canaries.json" >"$work/canaries.next.json" ||
            die "cannot add the Canaries HelmRelease $release_ns/$release_name renders"
          mv "$work/canaries.next.json" "$work/canaries.json" ||
            die "cannot keep the Canaries HelmRelease $release_ns/$release_name renders"
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

  # The charts' own Canaries are held to the same limits as the manifests'.
  check_canaries

  decide "$work/candidates.json" "$work/attempted.json" "$work/exceptions.json" "$work/verdict.json"
  # A budget the manifests do not satisfy, in a namespace whose inputs are unchanged since the
  # base, was judged at the base: it is reported unchanged, and keeps the exception row it uses.
  if [ -s "$work/unchanged" ]; then
    jq --rawfile unchanged "$work/unchanged" --arg base "$base" --arg cluster "$cluster" \
      '($unchanged | split("\n") | map(select(. != ""))) as $skipped
       | map(if .status != "ok" and ((.key | split("/")[0]) as $ns | $skipped | index($ns)) != null then
           {status: "unchanged", key, row,
            text: "\($cluster): PodDisruptionBudget \(.key) is unchanged since \($base): nothing it is judged on changed, so the charts of its namespace were not rendered"}
         else . end)' "$work/verdict.json" >"$work/verdict.scoped.json" ||
      die "cannot mark the unchanged PodDisruptionBudgets of cluster '$cluster'"
    mv "$work/verdict.scoped.json" "$work/verdict.json" || die "cannot keep the verdicts of cluster '$cluster'"
  fi
  # Every budget must come back with one of the known verdicts: one the decision dropped would
  # otherwise read as clean.
  counts="$(jq -r '[length] + [("ok", "excepted", "finding", "unknown", "unchanged") as $s | [ .[] | select(.status == $s) ] | length]
      | map(tostring) | join(" ")' "$work/verdict.json")" || die "cannot count the verdicts of cluster '$cluster'"
  read -r decided ok excepted detached undecided unchanged <<<"$counts"
  if [ "$decided" != "$pdbs" ] || [ "$((ok + excepted + detached + undecided + unchanged))" != "$pdbs" ]; then
    die "cluster '$cluster' renders $pdbs PodDisruptionBudget(s) but $((ok + excepted + detached + undecided + unchanged)) of $decided verdict(s) are known — refusing to report the rest as clean"
  fi
  jq -r '.[] | select(.status == "ok") | "  ok        " + .text' "$work/verdict.json" ||
    die "cannot report the PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "excepted") | "  excepted  " + .text' "$work/verdict.json" ||
    die "cannot report the excepted PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "unchanged") | "  unchanged " + .text' "$work/verdict.json" ||
    die "cannot report the unchanged PodDisruptionBudgets of cluster '$cluster'"
  # A row is used when it was applied to a budget, whether it was accepted or refused: a refused
  # row is reported with its reason, not as stale.
  jq -r --arg cluster "$cluster" '.[] | select(.row == true) | "\($cluster)\t\(.key)"' "$work/verdict.json" >>"$scratch/used" ||
    die "cannot record the exception rows cluster '$cluster' uses"
  jq -r '.[] | select(.status == "finding") | .text' "$work/verdict.json" >>"$scratch/findings" ||
    die "cannot record the detached PodDisruptionBudgets of cluster '$cluster'"
  jq -r '.[] | select(.status == "unknown") | .text' "$work/verdict.json" >>"$scratch/unknown" ||
    die "cannot record the undecided PodDisruptionBudgets of cluster '$cluster'"
  total_ok=$((total_ok + ok))
  total_excepted=$((total_excepted + excepted))
  total_unchanged=$((total_unchanged + unchanged))
done

[ "$clusters" -gt 0 ] || die "no cluster overlay under '$root/clusters'"
[ "$total_pdbs" -gt 0 ] ||
  die "no cluster renders any PodDisruptionBudget under '$root' — refusing to report an empty render as clean"

# A row is judged in the one cluster it names. One that was applied to no budget there is stale.
jq -r '.[] | "\(.cluster)\t\(.key)"' "$scratch/exceptions.json" >"$scratch/exception-keys" || die "cannot list the exception rows"
while IFS="$tab" read -r row_cluster key || [ -n "$row_cluster" ]; do
  [ -n "$key" ] || continue
  if ! grep -qxF -- "$row_cluster" "$scratch/clusters"; then
    printf 'stale exception: the row for PodDisruptionBudget %s names cluster %s, which is no overlay under %s/clusters\n' \
      "$key" "$row_cluster" "$root" >>"$scratch/findings"
  elif ! grep -qxF -- "$row_cluster$tab$key" "$scratch/used"; then
    printf 'stale exception: %s: PodDisruptionBudget %s is satisfied by a rendered workload, or cluster %s does not render it\n' \
      "$row_cluster" "$key" "$row_cluster" >>"$scratch/findings"
  fi
done <"$scratch/exception-keys"

summary="$clusters cluster(s), $total_pdbs PodDisruptionBudget(s), $total_rendered HelmRelease chart(s) rendered"
if [ "$base_usable" = 1 ]; then
  summary="$summary, $total_unchanged unchanged since $base"
fi
status=0
if [ -s "$scratch/findings" ]; then
  printf 'guard-pdb-selector-match: PodDisruptionBudgets that select no rendered workload, and exception rows that do not hold:\n' >&2
  sed 's/^/  /' "$scratch/findings" >&2
  printf 'A budget whose selector matches no pod reports expectedPods: 0 and holds back no eviction. Point the selector at the labels of the pod template that serves.\n' >&2
  printf 'A workload nothing here can render is a reviewed row in %s naming the cluster, the budget, what builds its pods, the version pin its labels were verified at and where; remove a stale row, and verify a row again when its pin moves.\n' "$exceptions_file" >&2
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
