#!/usr/bin/env bash
#
# Fail when a rendered workload runs a container image by a floating tag (#3755).
#
# THE RULE THIS ENFORCES: every container image in the rendered cluster overlays
# either carries a digest (`@sha256:`) or names a tag other than a moving branch or
# channel name — `main`, `master`, `latest`, `edge`, `nightly`, `dev` or `develop`.
# An image with no tag at all is `latest`, so it floats too.
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
# ⚠️ WHAT THIS DOES NOT SEE. A HelmRelease is rendered by Flux, not by Kustomize, so
# an image a chart chooses is invisible here, and so are workloads delivered by a
# nested Flux Kustomization or an OCI artifact from another repository. Only the core
# workload kinds (Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob)
# are read.
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
#   0  every rendered image is digest-pinned or names a non-floating tag
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

exceptions_file="${FLOATING_IMAGE_TAG_EXCEPTIONS:-$(dirname "$0")/floating-image-tag-exceptions.tsv}"
[ -f "$exceptions_file" ] ||
  die "exceptions file '$exceptions_file' not found — refusing to run without the reviewed disposition list"

scratch="$(mktemp -d)" || die "cannot create a scratch directory"
trap 'rm -rf "$scratch"' EXIT

tab="$(printf '\t')"

field() { # <row> <n>
  printf '%s\n' "$1" | awk -F '\t' -v n="$2" '{ print $n }'
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
  case $tag in
    '' | main | master | latest | edge | nightly | dev | develop) printf 'floating' ;;
    *) printf 'versioned' ;;
  esac
}

# One line per container: `<Kind>/<namespace>/<name> <image>`. Kubernetes names and
# image references contain no spaces, so a third field means the render is not what
# this guard understands.
# shellcheck disable=SC2016 # `$d` is a yq variable, not a shell expansion
extract='select(.kind == "Pod" or .kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet" or .kind == "ReplicaSet" or .kind == "Job" or .kind == "CronJob")
  | . as $d
  | [ (.spec.containers // [])[], (.spec.initContainers // [])[], (.spec.ephemeralContainers // [])[],
      (.spec.template.spec.containers // [])[], (.spec.template.spec.initContainers // [])[], (.spec.template.spec.ephemeralContainers // [])[],
      (.spec.jobTemplate.spec.template.spec.containers // [])[], (.spec.jobTemplate.spec.template.spec.initContainers // [])[] ]
  | .[] | $d.kind + "/" + ($d.metadata.namespace // "-") + "/" + ($d.metadata.name // "-") + " " + (.image // "")'

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
  yq -N 'select(.kind == "Kustomization" and ((.apiVersion // "") | test("^kustomize[.]toolkit[.]fluxcd[.]io/"))) | (.spec.path // "")' \
    "$scratch/$cluster.root.yaml" >"$scratch/$cluster.paths" 2>"$scratch/$cluster.paths.err" ||
    die "cannot read the Flux Kustomizations rendered by '$overlay': $(head -c 500 "$scratch/$cluster.paths.err")"

  paths=0
  images=0
  while IFS= read -r path || [ -n "$path" ]; do
    [ -n "$path" ] || die "a Flux Kustomization rendered by '$overlay' has no spec.path"
    path="${path#./}"
    case $path in
      /* | .. | ../* | */.. | */../*) die "Flux path '$path' named by '$overlay' leaves '$root'" ;;
    esac
    [ -d "$root/$path" ] || die "Flux path '$path' named by '$overlay' does not exist under '$root'"
    paths=$((paths + 1))
    rendered="$scratch/$cluster.$paths"

    kubectl kustomize "$root/$path" >"$rendered.yaml" 2>"$rendered.err" ||
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
  done <"$scratch/$cluster.paths"

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
