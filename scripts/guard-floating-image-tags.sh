#!/usr/bin/env bash
#
# Fail when a rendered workload runs a container image by a floating tag (#3755).
#
# THE RULE THIS ENFORCES: every container image in the rendered cluster overlays
# either carries a digest (`@sha256:`) or names a version-shaped tag — digits
# separated by dots, optionally led by `v` and followed by a `-` or `+` suffix
# (`1.27.0`, `v2.0.0-rc.1`, `17.2-alpine`). Every other tag is treated as a moving
# reference: branch and channel names such as `main`, `latest`, `stable`, `canary`
# or `release` can all be repointed upstream. An image with no tag at all is
# `latest`, so it floats too. A tag that is not version-shaped but is known not to
# move goes through the reviewed exception list, never through a name list here.
#
# Why this matters (#3515). Upstream kubelet-serving-cert-approver v0.11.1 moved its
# deployment image to `:main` with `imagePullPolicy: Always`. A moving tag lets an
# upstream push change what production runs with no PR, no review and no digest, and
# it makes image signature verification check whatever the tag points at today. The
# Kyverno `disallow-latest-tag` policy does not cover this: it matches only `latest`
# and missing tags, runs in Audit, and excludes most system namespaces.
#
# 🔴 WHY THE RENDER ROOTS ARE READ FROM THE CLUSTER OVERLAYS, NOT LISTED HERE.
#
# `kubectl kustomize k8s/clusters/<cluster>` renders only Flux Kustomizations; the
# workloads live under the `spec.path` each of them names. A hard-coded list of those
# paths is only as complete as the day it was written, so a layer added later would
# not be read and its images would report clean. The guard renders every cluster
# overlay under `<root>/clusters` and follows the paths its Flux Kustomizations name,
# so a new layer or cluster is covered without editing the guard. `clusters/base` is
# the template the overlays share: its paths still carry placeholders, so it is not
# a cluster and is skipped.
#
# 🔴 WHY EACH PATH IS RENDERED THROUGH ITS FLUX KUSTOMIZATION, NOT ON ITS OWN.
#
# Flux applies `spec.images`, `spec.patches`, `spec.components`, `spec.targetNamespace`,
# `spec.namePrefix` and `spec.nameSuffix` on top of the directory it builds. Rendering
# the bare directory would scan output production never receives: a Flux-level
# `images:` entry retagging a pinned image to `latest` would pass. So each path is
# built through a generated wrapper Kustomization carrying those same fields. Component
# paths are relative to `spec.path`, as Flux resolves them, and must stay inside the
# root. The deprecated `patchesStrategicMerge` and `patchesJson6902` fields are refused
# as cannot-check rather than ignored.
#
# ⚠️ WHAT THIS DOES NOT SEE. A HelmRelease is rendered by Flux, not by Kustomize, so
# an image a chart chooses is invisible here, and so are workloads delivered by a
# nested Flux Kustomization or an OCI artifact from another repository. `postBuild`
# substitution is not performed: an image still carrying `${...}` is cannot-check.
# Only the core workload kinds (Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet,
# ReplicationController, Job, CronJob) are read.
#
# ⚠️ ANTI-VACUITY. No cluster overlay, an overlay that names no Flux Kustomization,
# or a cluster whose render yields no workload image at all is exit 2. A selector
# that matched nothing is indistinguishable from a clean tree.
#
# ⚠️ AN IMAGE THIS CANNOT CLASSIFY IS cannot-check, NEVER clean. An empty image, one
# still carrying a Flux `${...}` substitution, or a digest that is not a SHA-256
# cannot be judged from the render.
#
# A floating image is allowed only by a reviewed row in
# `scripts/floating-image-tag-exceptions.tsv` naming the workload, the image, a
# tracking issue and the reason. A row that no longer matches a floating image is
# itself a violation, so the list cannot quietly become where floating tags gather.
#
# Exit codes:
#   0  every rendered image is digest-pinned or names a version-shaped tag
#   1  at least one floating image is unexcepted, or an exception row is stale
#   2  cannot check: bad usage, missing root or tool, a render or parse failure,
#      an unclassifiable image, a malformed exception row, or an anti-vacuity failure

set -uo pipefail

die() {
  printf 'guard-floating-image-tags: %s\n' "$*" >&2
  exit 2
}

[ "$#" -eq 1 ] || die "usage: $0 <k8s-root>"
root="${1%/}"
[ -d "$root/clusters" ] || die "'$root/clusters' is not a directory"
command -v kubectl >/dev/null 2>&1 || die "kubectl is required but not installed"
command -v yq >/dev/null 2>&1 || die "yq is required but not installed"
root_real="$(cd "$root" && pwd -P)" || die "cannot resolve '$root'"

exceptions_file="${FLOATING_IMAGE_TAG_EXCEPTIONS:-$(dirname "$0")/floating-image-tag-exceptions.tsv}"
[ -f "$exceptions_file" ] ||
  die "exceptions file '$exceptions_file' not found — refusing to run without the reviewed disposition list"

scratch="$(mktemp -d)" || die "cannot create a scratch directory"
trap 'rm -rf "$scratch"' EXIT

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

# A malformed row is exit 2: a row this guard cannot read is a disposition nobody can
# audit, and ignoring it would let a floating image pass on a row that says nothing.
: >"$scratch/excepted"
lineno=0
while IFS= read -r row || [ -n "$row" ]; do
  lineno=$((lineno + 1))
  case $row in '' | '#'*) continue ;; esac
  workload="$(field "$row" 1)"
  image="$(field "$row" 2)"
  issue="$(field "$row" 3)"
  reason="$(field "$row" 4)"
  if [ -z "$workload" ] || [ -z "$image" ]; then
    die "$exceptions_file:$lineno: a row must name a workload (column 1) and an image (column 2)"
  fi
  printf '%s' "$issue" | grep -Eq '^#[0-9]+$' ||
    die "$exceptions_file:$lineno: '$workload' names no tracking issue (column 3 must be #<number>, got '$issue')"
  [ -n "$reason" ] ||
    die "$exceptions_file:$lineno: '$workload' carries no reason (column 4)"
  printf '%s%s%s\n' "$workload" "$tab" "$image" >>"$scratch/excepted"
done <"$exceptions_file"

# Prints one of: pinned, versioned, floating, unclassifiable.
classify() { # <image>
  local image="$1" last tag
  # shellcheck disable=SC2016 # a literal `${`: an unresolved Flux substitution
  case $image in
    '' | *'${'*)
      printf 'unclassifiable'
      return
      ;;
    *@sha256:*)
      if printf '%s' "${image##*@sha256:}" | grep -Eq '^[0-9a-f]{64}$'; then
        printf 'pinned'
      else
        printf 'unclassifiable'
      fi
      return
      ;;
    *@*)
      printf 'unclassifiable'
      return
      ;;
  esac
  # Only the last path segment can carry a tag: a colon in an earlier segment is a
  # registry port, so `localhost:5000/app` is untagged, not tagged `5000/app`.
  last="${image##*/}"
  case $last in
    *:*) tag="${last#*:}" ;;
    *) tag='' ;;
  esac
  # An allow-list of shapes, not a deny-list of names: a channel name nobody thought
  # to list must not pass as a version.
  if printf '%s' "$tag" | grep -Eq '^v?[0-9]+([.][0-9]+)*([-+][0-9A-Za-z][0-9A-Za-z.+_-]*)?$'; then
    printf 'versioned'
  else
    printf 'floating'
  fi
}

# One line per container: `<Kind>/<namespace>/<name> <image>`. Kubernetes names and
# image references contain no spaces, so a third field means the render is not what
# this guard understands.
# shellcheck disable=SC2016 # `$d` is a yq variable, not a shell expansion
extract='select(.kind == "Pod" or .kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet" or .kind == "ReplicaSet" or .kind == "ReplicationController" or .kind == "Job" or .kind == "CronJob")
  | . as $d
  | [ (.spec.containers // [])[], (.spec.initContainers // [])[], (.spec.ephemeralContainers // [])[],
      (.spec.template.spec.containers // [])[], (.spec.template.spec.initContainers // [])[], (.spec.template.spec.ephemeralContainers // [])[],
      (.spec.jobTemplate.spec.template.spec.containers // [])[], (.spec.jobTemplate.spec.template.spec.initContainers // [])[] ]
  | .[] | $d.kind + "/" + ($d.metadata.namespace // "-") + "/" + ($d.metadata.name // "-") + " " + (.image // "")'

# The wrapper carries the Flux fields that change what the directory renders into.
# shellcheck disable=SC2016 # `$s` is a yq variable, not a shell expansion
wrapper='.spec as $s
  | {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": [strenv(RESOURCE)]}
  | with(select($s.images != null); .images = $s.images)
  | with(select($s.patches != null); .patches = $s.patches)
  | with(select($s.targetNamespace != null); .namespace = $s.targetNamespace)
  | with(select($s.namePrefix != null); .namePrefix = $s.namePrefix)
  | with(select($s.nameSuffix != null); .nameSuffix = $s.nameSuffix)'

: >"$scratch/findings"
: >"$scratch/used"
clusters=0
total_images=0
for overlay in "$root"/clusters/*/; do
  overlay="${overlay%/}"
  cluster="${overlay##*/}"
  [ "$cluster" != base ] || continue
  [ -f "$overlay/kustomization.yaml" ] || continue
  clusters=$((clusters + 1))

  kubectl kustomize "$overlay" >"$scratch/$cluster.root.yaml" 2>"$scratch/$cluster.root.err" ||
    die "cannot render cluster overlay '$overlay': $(head -c 500 "$scratch/$cluster.root.err")"
  mkdir -p "$scratch/$cluster.flux" || die "cannot create a scratch directory for '$cluster'"
  # One file per Flux Kustomization, so each is rendered with its own fields.
  # shellcheck disable=SC2016 # `$index` is a yq variable, not a shell expansion
  (cd "$scratch/$cluster.flux" &&
    yq -N -s '"doc-" + $index' \
      'select(.kind == "Kustomization" and ((.apiVersion // "") | test("^kustomize[.]toolkit[.]fluxcd[.]io/")))' \
      "$scratch/$cluster.root.yaml") 2>"$scratch/$cluster.flux.err" ||
    die "cannot read the Flux Kustomizations rendered by '$overlay': $(head -c 500 "$scratch/$cluster.flux.err")"

  paths=0
  images=0
  for doc in "$scratch/$cluster.flux"/doc-*.yml; do
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
    paths=$((paths + 1))
    rendered="$scratch/$cluster.$paths"
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
    yq -N "$extract" "$rendered.yaml" >"$rendered.images" 2>"$rendered.images.err" ||
      die "cannot read workload images rendered from '$root/$path': $(head -c 500 "$rendered.images.err")"

    while read -r workload image extra || [ -n "$workload" ]; do
      [ -n "$workload" ] || continue
      [ -z "$extra" ] || die "unexpected image line for $workload in '$root/$path': '$workload $image $extra'"
      images=$((images + 1))
      case "$(classify "$image")" in
        pinned | versioned) ;;
        floating)
          if grep -qxF -- "$workload$tab$image" "$scratch/excepted"; then
            printf '%s%s%s\n' "$workload" "$tab" "$image" >>"$scratch/used"
          else
            printf '%s: %s runs floating image %s (rendered from %s)\n' "$cluster" "$workload" "$image" "$path" >>"$scratch/findings"
          fi
          ;;
        *) die "cannot classify image '$image' of $workload in cluster '$cluster' (rendered from $path)" ;;
      esac
    done <"$rendered.images"
  done

  # Covers an overlay naming no Flux Kustomization too: with no path, nothing renders.
  [ "$images" -gt 0 ] ||
    die "cluster '$cluster' renders no workload image across $paths Flux path(s) — refusing to report an empty render as clean"
  total_images=$((total_images + images))
done

[ "$clusters" -gt 0 ] || die "no cluster overlay with a kustomization.yaml under '$root/clusters'"

while IFS= read -r entry || [ -n "$entry" ]; do
  [ -n "$entry" ] || continue
  grep -qxF -- "$entry" "$scratch/used" ||
    printf 'stale exception: %s no longer matches a floating image in any rendered workload\n' "$(printf '%s' "$entry" | tr '\t' ' ')" >>"$scratch/findings"
done <"$scratch/excepted"

if [ -s "$scratch/findings" ]; then
  printf 'guard-floating-image-tags: floating image tags found:\n' >&2
  sed 's/^/  /' "$scratch/findings" >&2
  printf 'Pin each image by digest (for example a Kustomize images: entry with digest:), or name a released version tag.\n' >&2
  printf 'A deliberate exception is a reviewed row in %s naming the workload, the image, a tracking issue and the reason; remove a stale row.\n' "$exceptions_file" >&2
  exit 1
fi

printf 'guard-floating-image-tags: %d cluster(s), %d rendered image(s), none floating\n' "$clusters" "$total_images"
exit 0
