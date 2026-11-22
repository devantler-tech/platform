#!/usr/bin/env bash
# Refuse a change whose Homepage-discovered routes name a group that the policy
# production is admitted against does not allow.
#
# WHY THIS EXISTS
# restrict-homepage-service-groups admits a route that opts into Homepage
# discovery only when it names a declared service group. A deploy is admitted
# against the copy of that policy that is already LIVE, which is the base
# branch's copy, not this change's: Flux applies infrastructure-controllers (the
# OpenBao route, for one) before the infrastructure layer that carries the
# policy, applies the rest of that layer in the same batch as the policy, and
# tenant routes reconcile on their own schedule. A change that adds a group and
# moves a route to it therefore passes every other check, then has the route's
# dry-run denied in production: the Flux layer stops reconciling and the
# merge-group deploy is evicted. That happened to platform#5383 on 2026-10-26.
# The policy header documents the two-deploy rule; this enforces it before
# merge. A group this change adds is not usable by a route in the same change.
#
# A route must also be allowed by this change's own copy, or its next dry-run is
# denied as soon as the change deploys (a group retired while a route still
# names it). scripts/tests/test-restrict-homepage-service-groups.sh pins this
# change's copy to the Homepage layout; nothing else compares real routes with
# either copy.
#
# WHAT IT JUDGES
# Every YAML document under k8s/ that is an HTTPRoute or an Ingress (the two
# kinds the policy matches) with gethomepage.dev/enabled "true", plus every
# Flagger Canary whose spec.service.apex annotations opt in, because Flagger
# copies those onto the HTTPRoute it generates and that route is admitted by the
# same policy.
#
# WHAT IT DOES NOT SEE, BY NAME
#   - A kustomize patch that sets the group without also carrying
#     gethomepage.dev/enabled: "true" (none exists today; a route's annotations
#     live in its own http-route.yaml).
#   - Routes in other repositories, such as tenants. They are admitted against
#     the same live policy, so the two-deploy rule in the policy header still
#     applies to them.
#
# FAIL CLOSED
# Exit 1: a route names a group the base or this change does not allow.
# Exit 2: nothing was proven: a usage error, an unreadable base revision or
# policy copy, a YAML file that does not parse, or zero opted-in routes found
# (an empty walk would otherwise pass every route it never looked at). If the
# policy is ever retired, retire this guard in the same change; if its shape
# changes, declared_groups below must read both the old (base) and new shape for
# that one change.
#
# Usage: guard-homepage-route-groups.sh --base <rev> [--repo <dir>]
#   --base  the commit production is admitted against: origin/main for a pull
#           request, the queue's base_sha for a merge group. The caller fetches
#           it; this script never reaches the network.
#   --repo  the repository to judge (default: the one holding this script).
#           Its working tree is "this change"; --base is read from its git.

set -euo pipefail

readonly POLICY_PATH='k8s/bases/infrastructure/cluster-policies/best-practices/restrict-homepage-service-groups.yaml'

usage() {
  printf '::error::guard-homepage-route-groups: %s\n' "$1" >&2
  printf 'usage: %s --base <rev> [--repo <dir>]\n' "$0" >&2
  exit 2
}

unknown() {
  printf '::error::%s\n' "$1" >&2
  exit 2
}

base=""
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --base)
      [ "$#" -ge 2 ] || usage '--base needs a revision'
      base="$2"
      shift 2
      ;;
    --repo)
      [ "$#" -ge 2 ] || usage '--repo needs a directory'
      repo="$2"
      shift 2
      ;;
    *) usage "unknown argument '$1'" ;;
  esac
done
[ -n "$base" ] || usage '--base is required: the revision production is admitted against'
[ -d "$repo/k8s" ] || unknown "no k8s/ directory under '$repo'"
cd "$repo"

for tool in git yq jq; do
  command -v "$tool" >/dev/null 2>&1 || unknown "$tool is required and was not found on PATH"
done

# declared_groups — read a policy copy on stdin, print its allow-list one per
# line. Mirrors the extraction in test-restrict-homepage-service-groups.sh.
declared_groups() {
  yq -r '.spec.variables[] | select(.name == "declaredGroups") | .expression' |
    grep -o "'[^']*'" | tr -d "'" | sort -u
}

# --- 1. discover every opted-in route in this change --------------------------
files=()
while IFS= read -r -d '' f; do
  files+=("$f")
done < <(find k8s \( -type f -o -type l \) \( -iname '*.yaml' -o -iname '*.yml' \) -print0)
# Never call yq with no files: it would read stdin and hang.
[ "${#files[@]}" -gt 0 ] || unknown 'found no YAML files under k8s/, so no route was judged'

# A file that does not parse fails yq as a whole, so the walk never passes
# routes it could not read.
if ! routes_json="$(yq ea -o=json -I=0 '
  [
    select(tag == "!!map" and (.kind == "HTTPRoute" or .kind == "Ingress")) |
    {"file": filename, "kind": .kind, "namespace": (.metadata.namespace // ""),
     "name": (.metadata.name // ""), "annotations": (.metadata.annotations // {})}
  ] + [
    select(tag == "!!map" and .kind == "Canary") |
    {"file": filename, "kind": "Canary", "namespace": (.metadata.namespace // ""),
     "name": (.metadata.name // ""), "annotations": (.spec.service.apex.annotations // {})}
  ]
' "${files[@]}")"; then
  unknown 'could not parse the YAML under k8s/, so no route was judged'
fi

# <file> TAB <kind> TAB <namespace/name> TAB <group>, one per opted-in route.
# An absent group reads as "", which no allow-list contains: the policy denies
# it by the same rule.
if ! routes="$(printf '%s' "$routes_json" | jq -r '
  .[]
  | select((.annotations["gethomepage.dev/enabled"] // "" | tostring) == "true")
  | [.file, .kind, "\(.namespace)/\(.name)", (.annotations["gethomepage.dev/group"] // "" | tostring)]
  | @tsv
' | sort)"; then
  unknown 'could not read the discovered routes, so no route was judged'
fi

route_count="$(printf '%s\n' "$routes" | grep -c . || true)"
[ "$route_count" -ge 1 ] ||
  unknown 'found zero routes with gethomepage.dev/enabled "true" under k8s/; the walk broke, and passing now would judge nothing'

# --- 2. read both copies of the allow-list ------------------------------------
# cannot_verify <reason> — name every route left unproven, then exit 2.
cannot_verify() {
  local file kind ref group
  while IFS="$(printf '\t')" read -r file kind ref group; do
    printf '::error file=%s::cannot verify %s %s (gethomepage.dev/group %s): %s\n' \
      "$file" "$kind" "$ref" "'$group'" "$1" >&2
  done <<EOF
$routes
EOF
  unknown "$1"
}

if ! base_sha="$(git rev-parse --verify --quiet --end-of-options "${base}^{commit}")"; then
  cannot_verify "base revision '$base' is not a commit in this repository, so the allow-list production is admitted against is unreadable (fetch the base before running)"
fi
if ! base_policy="$(git show "${base_sha}:${POLICY_PATH}" 2>/dev/null)"; then
  cannot_verify "the base ${base} (${base_sha:0:12}) has no readable ${POLICY_PATH}, so the allow-list production is admitted against is unreadable"
fi
if ! base_groups="$(printf '%s\n' "$base_policy" | declared_groups)" || [ -z "$base_groups" ]; then
  cannot_verify "found no declaredGroups in the base ${base} (${base_sha:0:12}) copy of ${POLICY_PATH}"
fi

[ -r "$POLICY_PATH" ] ||
  cannot_verify "this change has no readable ${POLICY_PATH}; if the policy is being retired, retire this guard with it"
if ! head_groups="$(declared_groups <"$POLICY_PATH")" || [ -z "$head_groups" ]; then
  cannot_verify "found no declaredGroups in this change's copy of ${POLICY_PATH}"
fi

# --- 3. judge every route against both ------------------------------------------
# A here-string, not a printf pipe: under pipefail an early `grep -q` exit can
# SIGPIPE the writer and turn a match into a refusal.
allowed() {
  grep -qxF -- "$1" <<<"$2"
}

failures=0
while IFS="$(printf '\t')" read -r file kind ref group; do
  verdict=ok
  if ! allowed "$group" "$base_groups"; then
    verdict=refused
    printf '::error file=%s::%s %s names gethomepage.dev/group %s, which the live policy (the base %s, %s) does not allow. Routes are admitted against that copy until this change is live, and Flux can apply this route first (infrastructure-controllers reconciles before the policy, and the policy layer applies its own routes in the same batch), so the dry-run would be denied and that Flux layer would stop reconciling. Add the group in one change, deploy it, then move the route (two-deploy rule in %s).\n' \
      "$file" "$kind" "$ref" "'$group'" "$base" "${base_sha:0:12}" "$POLICY_PATH" >&2
  fi
  if ! allowed "$group" "$head_groups"; then
    verdict=refused
    printf '::error file=%s::%s %s names gethomepage.dev/group %s, which this change'"'"'s %s does not allow, so its next dry-run is denied once this change deploys.\n' \
      "$file" "$kind" "$ref" "'$group'" "$POLICY_PATH" >&2
  fi
  [ "$verdict" = ok ] || failures=$((failures + 1))
  printf '%-8s %s %s  %s  (%s)\n' "$verdict" "$kind" "$ref" "'$group'" "$file"
done <<EOF
$routes
EOF

if [ "$failures" -gt 0 ]; then
  printf '::error::%d of %d Homepage routes name a group that production would not admit.\n' "$failures" "$route_count" >&2
  exit 1
fi
printf 'All %d Homepage routes name a group allowed by both the base %s (%s) and this change.\n' \
  "$route_count" "$base" "${base_sha:0:12}"
