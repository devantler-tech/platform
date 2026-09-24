#!/usr/bin/env bash
#
# Fail when a rendered CiliumClusterwideNetworkPolicy would silently turn on
# default-deny.
#
# THE RULE THIS ENFORCES: in every spec of every rendered clusterwide policy, a
# direction (ingress or egress) that carries deny rules and NO allow rules must
# set `enableDefaultDeny.<direction>` explicitly — `false` to subtract only the
# denied destinations, `true` to accept that everything else is revoked too.
#
# Why. Cilium turns on default-deny for every direction a policy has rules in,
# and deny rules count. A clusterwide policy carrying only an `egressDeny` and no
# `enableDefaultDeny` therefore does not subtract one destination: it revokes ALL
# egress from every endpoint it selects that has no allow policy of its own. That
# took cluster DNS, the node autoscaler and several controllers down on prod on
# 2026-08-27 (#3404, #3407). The per-policy test asserted what the document said,
# not what it did, so it passed throughout. This guard asserts the consequence,
# across every clusterwide policy, so a NEW policy cannot reintroduce it (#3408).
#
# A direction with allow rules is left alone: default-deny there is the ordinary
# allow-list meaning of the policy, not a side effect.
#
# Usage:
#   guard-cilium-clusterwide-default-deny.sh <repo-root>
#       Render every Flux entrypoint under <repo-root>/k8s and check the output.
#   guard-cilium-clusterwide-default-deny.sh --rendered <file>...
#       Check already-rendered multi-document YAML (used by the test).
#
# 🔴 SELECT ON PARSED `.kind` AND `.apiVersion`, NEVER ON TEXT. An apiVersion this
# guard cannot decide is exit 2, so deleting one line cannot hide a policy.
#
# ⚠️ ANTI-VACUITY: finding NO clusterwide policy at all is exit 2, not exit 0.
# "Nothing to check" must never render as "checked and clean".
#
# Exit codes:
#   0  every clusterwide policy found states its default-deny intent
#   1  at least one direction would silently default-deny — the defect
#   2  cannot check: bad usage, a render that failed, unparseable YAML, an
#      undecidable apiVersion or spec shape, or no clusterwide policy found

set -uo pipefail

readonly name='guard-cilium-clusterwide-default-deny'

die() {
  printf '%s: %s\n' "$name" "$*" >&2
  exit 2
}

command -v yq >/dev/null 2>&1 || die "yq is required but not installed"
command -v jq >/dev/null 2>&1 || die "jq is required but not installed"

tmp_dir="$(mktemp -d)" || die "could not create a temporary directory"
trap 'rm -rf "${tmp_dir}"' EXIT

rendered=()
if [ "${1:-}" = "--rendered" ]; then
  shift
  [ "$#" -ge 1 ] || die "usage: $0 --rendered <file>..."
  for file in "$@"; do
    [ -f "$file" ] || die "rendered file '$file' does not exist"
    rendered+=("$file")
  done
else
  [ "$#" -eq 1 ] || die "usage: $0 <repo-root> | --rendered <file>..."
  k8s="$1/k8s"
  [ -d "$k8s" ] || die "'$k8s' is not a directory"

  # The Flux entrypoints (k8s/clusters/base/*): per provider the infrastructure,
  # controllers and apps layers, and per cluster the bootstrap layer. A missing
  # entrypoint is cannot-check, never skipped, so a rename cannot drop a layer.
  roots=()
  for provider in docker hetzner; do
    roots+=("providers/${provider}/infrastructure" "providers/${provider}/infrastructure/controllers" "providers/${provider}/apps")
  done
  for cluster in local prod; do
    roots+=("clusters/${cluster}/bootstrap")
  done

  index=0
  for root in "${roots[@]}"; do
    [ -f "${k8s}/${root}/kustomization.yaml" ] ||
      die "Flux entrypoint '${root}' has no kustomization.yaml — refusing to report a tree it did not render"
    out="${tmp_dir}/render-${index}.yaml"
    kubectl kustomize "${k8s}/${root}" >"$out" 2>"${tmp_dir}/render-${index}.err" ||
      die "rendering '${root}' failed: $(head -c 400 "${tmp_dir}/render-${index}.err")"
    rendered+=("$out")
    index=$((index + 1))
  done
fi

# One JSON document per line, mappings only (a JSON-patch file is a top-level array).
json="${tmp_dir}/docs.json"
: >"$json"
for file in "${rendered[@]}"; do
  yq eval -o=json -I=0 'select(tag == "!!map")' "$file" >>"$json" 2>/dev/null ||
    die "could not parse '$file' — refusing to report output it could not read"
done

# Emits one TSV row per finding:
#   UNDECIDABLE <policy> <reason>
#   VIOLATION   <policy> <spec | specs[i]> <direction>
#   CHECKED     <policy>
# shellcheck disable=SC2016  # $name, $api, ... are jq variables; the shell must not expand them.
jq_program='
  select(.kind == "CiliumClusterwideNetworkPolicy")
  | (.metadata.name // "<unnamed>") as $name
  | (.apiVersion // "") as $api
  | (($api | type) == "string") as $isString
  # Three cases: a cilium.io/<version> policy is checked; a policy of another API
  # group is not Cilium and is skipped; anything else (absent, empty, not a
  # string, or cilium.io with no version) cannot be decided.
  | (if $isString and ($api | test("^cilium\\.io/.+")) then "check"
    elif $isString and $api != "" and ($api | test("^cilium\\.io/?$") | not) then "other"
    else "undecidable"
    end) as $scope
  | if $scope == "other" then empty
    elif $scope == "undecidable" then
      ["UNDECIDABLE", $name, "apiVersion \"\($api | tostring)\" does not name a cilium.io version"] | @tsv
    elif ((.spec // null) == null) and ((.specs // null) == null) then
      ["UNDECIDABLE", $name, "no spec or specs"] | @tsv
    elif (.specs // null) != null and ((.specs | type) != "array") then
      ["UNDECIDABLE", $name, "specs is not a list"] | @tsv
    else
      # Cilium applies the rules in BOTH fields when both are present, so check both and
      # label each rule by where it lives: `spec`, or `specs[i]`.
      ((if (.spec // null) != null then [{at: "spec", rule: .spec}] else [] end)
       + ((.specs // []) | to_entries | map({at: "specs[\(.key)]", rule: .value}))) as $rules
      | (["CHECKED", $name] | @tsv),
        ($rules[]
         | .at as $at
         | .rule as $spec
         | if ($spec | type) != "object" then
             ["UNDECIDABLE", $name, "\($at) is not a mapping"] | @tsv
           else
             ("ingress", "egress") as $dir
             | ($spec["\($dir)Deny"] // []) as $deny
             | ($spec[$dir] // []) as $allow
             | select(($deny | length) > 0 and ($allow | length) == 0)
             | select((($spec.enableDefaultDeny // {}) | type) != "object"
                      or (($spec.enableDefaultDeny // {}) | has($dir) | not))
             | ["VIOLATION", $name, $at, $dir] | @tsv
           end)
    end
'
if ! rows="$(jq -r "$jq_program" "$json" 2>"${tmp_dir}/jq.err")"; then
  die "could not evaluate the rendered policies: $(head -c 400 "${tmp_dir}/jq.err")"
fi

checked=0
violations=0
undecidable=0
while IFS=$'\t' read -r kind policy a b; do
  case "$kind" in
    CHECKED) checked=$((checked + 1)) ;;
    VIOLATION)
      violations=$((violations + 1))
      printf 'VIOLATION CiliumClusterwideNetworkPolicy/%s %s: %sDeny rules with no %s allow rules and no enableDefaultDeny.%s\n' \
        "$policy" "$a" "$b" "$b" "$b" >&2
      ;;
    UNDECIDABLE)
      undecidable=$((undecidable + 1))
      printf 'UNDECIDABLE CiliumClusterwideNetworkPolicy/%s: %s\n' "$policy" "$a" >&2
      ;;
    '') ;;
    *) die "unexpected evaluator output '$kind'" ;;
  esac
done <<<"$rows"

[ "$undecidable" -eq 0 ] ||
  die "$undecidable clusterwide polic(y/ies) could not be decided — this output is unverified"

[ "$checked" -gt 0 ] ||
  die "no CiliumClusterwideNetworkPolicy found in the rendered output — nothing was verified"

if [ "$violations" -gt 0 ]; then
  cat >&2 <<'FIX'

Cilium turns on default-deny for every direction a policy has rules in, deny
rules included. A clusterwide policy with only deny rules in a direction therefore
revokes ALL traffic in that direction from every endpoint it selects that has no
allow policy of its own — not just the destinations it names (#3404, #3407).

State the intent on each spec listed above:

  enableDefaultDeny:
    egress: false   # subtract only the denied destinations (the usual intent)

or set it to `true` if revoking everything else is really what you want.
FIX
  printf '%s: %d direction(s) across %d clusterwide polic(y/ies) would silently default-deny\n' \
    "$name" "$violations" "$checked" >&2
  exit 1
fi

printf '%s: OK — %d clusterwide polic(y/ies), every deny-only direction states enableDefaultDeny\n' "$name" "$checked"
exit 0
