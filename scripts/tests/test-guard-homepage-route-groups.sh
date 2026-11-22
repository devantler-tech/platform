#!/usr/bin/env bash
# RED/GREEN coverage for scripts/guard-homepage-route-groups.sh.
#
# WHAT IS ACTUALLY BEING PROVED
# The guard's harmful failure mode is passing a route that production will not
# admit: platform#5383 renamed a group and moved a route to it in one change,
# every check was green, and the merge-group deploy was evicted when the route's
# dry-run was denied by the policy still live on main. Each case below is a
# throwaway git repository whose `main` commit is what production runs, with
# "this change" in the working tree, exactly as CI checks it out. Every refusal
# asserts its exit code AND names the route, the group and which copy of the
# allow-list refused it, so a refusal for some other reason (a parse failure, an
# empty walk) cannot pass as the one under test.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly REPO_ROOT
readonly GUARD="$REPO_ROOT/scripts/guard-homepage-route-groups.sh"
readonly POLICY='k8s/bases/infrastructure/cluster-policies/best-practices/restrict-homepage-service-groups.yaml'
readonly ROUTE='k8s/bases/infrastructure/controllers/openbao/http-route.yaml'

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}
pass() { printf 'ok: %s\n' "$*"; }

[ -x "$GUARD" ] || {
  fail "guard not executable at $GUARD"
  exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Isolated from the caller's git configuration: no signing, hooks or identity.
g() {
  git -C "$repo" -c user.name=fixture -c user.email=fixture@example.invalid \
    -c commit.gpgsign=false -c core.hooksPath=/dev/null "$@"
}

# policy_doc <group>... — the ValidatingPolicy, in the shape the real one has.
policy_doc() {
  local group first=1
  printf 'apiVersion: policies.kyverno.io/v1\nkind: ValidatingPolicy\nmetadata:\n  name: restrict-homepage-service-groups\nspec:\n  variables:\n    - name: declaredGroups\n      expression: >-\n        [\n'
  for group in "$@"; do
    [ "$first" -eq 1 ] || printf ',\n'
    printf "          '%s'" "$group"
    first=0
  done
  printf '\n        ]\n'
}

# route_doc <kind> <name> <enabled> [group] — an HTTPRoute or Ingress.
route_doc() {
  local api=gateway.networking.k8s.io/v1
  [ "$1" = Ingress ] && api=networking.k8s.io/v1
  printf 'apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: %s\n  annotations:\n    gethomepage.dev/enabled: "%s"\n' "$api" "$1" "$2" "$2" "$3"
  [ "$#" -lt 4 ] || printf '    gethomepage.dev/group: %s\n' "$4"
  printf 'spec: {}\n'
}

# canary_doc <name> <enabled> <group> — Flagger copies apex annotations onto the
# HTTPRoute it generates.
canary_doc() {
  printf 'apiVersion: flagger.app/v1beta1\nkind: Canary\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  service:\n    port: 80\n    apex:\n      annotations:\n        gethomepage.dev/enabled: "%s"\n        gethomepage.dev/group: %s\n' "$1" "$1" "$2" "$3"
}

# write <path> — stdin into the fixture's working tree.
write() {
  mkdir -p "$repo/$(dirname "$1")"
  cat >"$repo/$1"
}

# fresh_repo <case> — `main` allows Kubernetes and Secrets Management and runs
# the OpenBao route on Secrets Management. origin/main points at it, as after
# CI's explicit fetch. Every case varies one thing from here.
repo=""
fresh_repo() {
  repo="$WORK/$1"
  mkdir -p "$repo"
  g init -q
  g checkout -q -b main
  policy_doc Kubernetes 'Secrets Management' | write "$POLICY"
  route_doc HTTPRoute openbao true 'Secrets Management' | write "$ROUTE"
  g add k8s
  g commit -q -m base
  g update-ref refs/remotes/origin/main HEAD
}

out=""
rc=0
run_guard() {
  rc=0
  out="$(bash "$GUARD" --repo "$repo" "$@" 2>&1)" || rc=$?
}

# expect <case> <exit-code> <must-contain>... — runs nothing; judges the last run.
# Needles are matched with `case`, never `printf | grep -q`, so a SIGPIPE under
# pipefail cannot misreport a correct result.
expect() {
  local name="$1" want="$2" needle
  shift 2
  if [ "$rc" -ne "$want" ]; then
    fail "$name: expected exit $want, got $rc: $out"
    return
  fi
  for needle in "$@"; do
    case "$out" in
      *"$needle"*) ;;
      *)
        fail "$name: output does not name '$needle': $out"
        return
        ;;
    esac
  done
  pass "$name"
}

# expect_absent <case> <must-not-contain> — the last run must not say this.
expect_absent() {
  case "$out" in
    *"$2"*) fail "$1: output unexpectedly names '$2': $out" ;;
    *) pass "$1" ;;
  esac
}

# --- GREEN ---------------------------------------------------------------------

fresh_repo allowed-on-main
run_guard --base origin/main
expect 'a route on a group allowed on main passes' 0 \
  "ok       HTTPRoute openbao/openbao  'Secrets Management'  ($ROUTE)" \
  'All 1 Homepage routes name a group allowed by both the base origin/main'

# The merge-group shape: the queue hands over a commit, not a branch.
run_guard --base "$(g rev-parse HEAD)"
expect 'a commit as the base (merge-group base_sha) passes the same route' 0 'All 1 Homepage routes'

# The second deploy of the two-deploy rule: the group is live, so moving the
# route to it now is admitted.
fresh_repo second-deploy
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
g commit -q -am 'add Vault'
g update-ref refs/remotes/origin/main HEAD
route_doc HTTPRoute openbao true Vault | write "$ROUTE"
run_guard --base origin/main
expect 'a route moving to a group main already allows passes' 0 "ok       HTTPRoute openbao/openbao  'Vault'"

fresh_repo disabled-route
route_doc HTTPRoute collector false 'Not A Group' | write k8s/bases/apps/collector/http-route.yaml
canary_doc umami false 'Not A Group' | write k8s/bases/apps/umami/canary.yaml
run_guard --base origin/main
expect 'routes and canaries that do not opt in are not judged' 0 'All 1 Homepage routes'
expect_absent 'a disabled route is not listed' 'collector'

# --- RED: production would not admit the route ---------------------------------

# platform#5383's shape: add the group and move the route in one change.
fresh_repo group-only-the-change-adds
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
route_doc HTTPRoute openbao true Vault | write "$ROUTE"
for base in origin/main "$(g rev-parse HEAD)"; do
  run_guard --base "$base"
  expect "a route moving to a group only the change adds is refused (base $base)" 1 \
    "::error file=$ROUTE::HTTPRoute openbao/openbao names gethomepage.dev/group 'Vault', which the live policy (the base $base," \
    'two-deploy rule' \
    '1 of 1 Homepage routes name a group that production would not admit'
  expect_absent "the change's own copy is not blamed (base $base)" "which this change's"
done

fresh_repo group-the-change-removes
policy_doc Kubernetes | write "$POLICY"
run_guard --base origin/main
expect "a route on a group the change removes is refused" 1 \
  "::error file=$ROUTE::HTTPRoute openbao/openbao names gethomepage.dev/group 'Secrets Management', which this change's $POLICY does not allow"
expect_absent 'the base is not blamed for a group it allows' 'which the live policy'

fresh_repo ingress
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
route_doc Ingress legacy true Vault | write k8s/bases/apps/legacy/ingress.yaml
run_guard --base origin/main
expect 'an Ingress on a group only the change adds is refused' 1 \
  "Ingress legacy/legacy names gethomepage.dev/group 'Vault', which the live policy"

fresh_repo canary
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
canary_doc umami true Vault | write k8s/bases/apps/umami/canary.yaml
run_guard --base origin/main
expect "an opted-in Canary's generated route on a new group is refused" 1 \
  "Canary umami/umami names gethomepage.dev/group 'Vault', which the live policy"

# A later document in a .yml file is judged like a first one.
fresh_repo later-document
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
{
  printf -- '---\n# a leading document that is not a route\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: extra\n---\n'
  route_doc HTTPRoute extra true Vault
} | write k8s/bases/apps/extra/resources.yml
run_guard --base origin/main
expect 'a route in a later document of a .yml file is judged' 1 \
  "HTTPRoute extra/extra names gethomepage.dev/group 'Vault', which the live policy"

fresh_repo missing-group
route_doc HTTPRoute openbao true | write "$ROUTE"
run_guard --base origin/main
expect 'an opted-in route that names no group is refused' 1 \
  "HTTPRoute openbao/openbao names gethomepage.dev/group '', which the live policy"

# --- platform#5383, replayed (#5390) ------------------------------------------
# The real allow-lists and route groups of the transition that evicted the merge
# group on 2026-10-26, read from each commit's policy (the ClusterPolicy shape of
# the time, carried over into today's shape) and its OpenBao and Policy Reporter
# routes. The other routes kept their groups throughout.
readonly ROUTE_POLICY_REPORTER='k8s/bases/infrastructure/policy-reporter/http-route.yaml'
# 735cd49d6b, the base of #5383 (2241e1c25 had the same list).
readonly GROUPS_BEFORE_5383='Kubernetes|Observability|Security|Diagnostics|Device Management|Infrastructure as Code|Cloud|Network|Home|Finance|Analytics|Data|Business|Products|Clients|Personal'
# 911843445a, #5383 as first queued: Security replaced in the same change.
readonly GROUPS_5383_FIRST='Kubernetes|Observability|Analytics|Secrets Management|Governance|Diagnostics|Device Management|Infrastructure as Code|Cloud|Network|Home|Finance|Data|Business & Clients|Products|Clients|Personal'
# b1bca2c80c, #5383 as merged (and 6da2e00896, the base of #5399): the new
# groups added beside Security.
readonly GROUPS_5383_MERGED='Kubernetes|Observability|Analytics|Security|Secrets Management|Governance|Diagnostics|Device Management|Infrastructure as Code|Cloud|Network|Home|Finance|Data|Business & Clients|Products|Clients|Personal'
# 476fd5c41f, #5399: the routes moved and Security retired.
readonly GROUPS_5399='Kubernetes|Observability|Analytics|Secrets Management|Governance|Diagnostics|Device Management|Infrastructure as Code|Cloud|Network|Home|Finance|Data|Business & Clients|Products|Personal'

# groups_policy <a|b|c> — policy_doc over a |-separated allow-list.
groups_policy() {
  local IFS='|'
  # shellcheck disable=SC2086 # split on | into one argument per group, by design.
  policy_doc $1
}

# replay_repo <case> <base-groups> <head-groups> <openbao-group> <policy-reporter-group>
# main runs <base-groups> with both routes on Security, as before #5383; the
# working tree is the change under test.
replay_repo() {
  repo="$WORK/$1"
  mkdir -p "$repo"
  g init -q
  g checkout -q -b main
  groups_policy "$2" | write "$POLICY"
  route_doc HTTPRoute openbao true Security | write "$ROUTE"
  route_doc HTTPRoute policy-reporter true Security | write "$ROUTE_POLICY_REPORTER"
  g add k8s
  g commit -q -m base
  g update-ref refs/remotes/origin/main HEAD
  groups_policy "$3" | write "$POLICY"
  route_doc HTTPRoute openbao true "$4" | write "$ROUTE"
  route_doc HTTPRoute policy-reporter true "$5" | write "$ROUTE_POLICY_REPORTER"
}

replay_repo replay-5383-first "$GROUPS_BEFORE_5383" "$GROUPS_5383_FIRST" 'Secrets Management' Governance
run_guard --base origin/main
expect 'platform#5383 as first queued is refused for both routes it moved' 1 \
  "::error file=$ROUTE::HTTPRoute openbao/openbao names gethomepage.dev/group 'Secrets Management', which the live policy" \
  "::error file=$ROUTE_POLICY_REPORTER::HTTPRoute policy-reporter/policy-reporter names gethomepage.dev/group 'Governance', which the live policy" \
  '2 of 2 Homepage routes name a group that production would not admit'

replay_repo replay-5383-merged "$GROUPS_BEFORE_5383" "$GROUPS_5383_MERGED" Security Security
run_guard --base origin/main
expect 'platform#5383 as merged (groups added, routes left on Security) passes' 0 \
  "ok       HTTPRoute openbao/openbao  'Security'" 'All 2 Homepage routes'

replay_repo replay-5399 "$GROUPS_5383_MERGED" "$GROUPS_5399" 'Secrets Management' Governance
run_guard --base origin/main
expect 'platform#5399 (routes moved once the groups were live, Security retired) passes' 0 \
  "ok       HTTPRoute openbao/openbao  'Secrets Management'" \
  "ok       HTTPRoute policy-reporter/policy-reporter  'Governance'" 'All 2 Homepage routes'

replay_repo replay-5399-early-retirement "$GROUPS_5383_MERGED" "$GROUPS_5399" 'Secrets Management' Security
run_guard --base origin/main
expect 'retiring Security while a route still names it is refused' 1 \
  "HTTPRoute policy-reporter/policy-reporter names gethomepage.dev/group 'Security', which this change's"

# --- UNKNOWN: nothing was proven, so nothing passes ------------------------------

fresh_repo unreadable-base
policy_doc Kubernetes 'Secrets Management' Vault | write "$POLICY"
route_doc HTTPRoute openbao true Vault | write "$ROUTE"
run_guard --base origin/no-such-branch
expect 'an unreadable base fails closed and names the unproven route and group' 2 \
  "::error file=$ROUTE::cannot verify HTTPRoute openbao/openbao (gethomepage.dev/group 'Vault'): base revision 'origin/no-such-branch' is not a commit"
expect_absent 'an unreadable base never reports a pass' 'All 1 Homepage routes'

fresh_repo base-without-policy
g rm -q "$POLICY"
g commit -q -m 'drop the policy'
base_sha="$(g rev-parse HEAD)"
g checkout -q HEAD~1 -- "$POLICY"
run_guard --base "$base_sha"
expect 'a base without the policy fails closed' 2 \
  "cannot verify HTTPRoute openbao/openbao (gethomepage.dev/group 'Secrets Management'): the base $base_sha" \
  "has no readable $POLICY"

fresh_repo base-without-groups
printf 'apiVersion: policies.kyverno.io/v1\nkind: ValidatingPolicy\nmetadata:\n  name: restrict-homepage-service-groups\nspec:\n  variables:\n    - name: somethingElse\n      expression: "[]"\n' | write "$POLICY"
g commit -q -am 'reshape the policy'
base_sha="$(g rev-parse HEAD)"
policy_doc Kubernetes 'Secrets Management' | write "$POLICY"
run_guard --base "$base_sha"
expect 'a base copy whose allow-list cannot be read fails closed' 2 \
  'cannot verify HTTPRoute openbao/openbao' 'found no declaredGroups in the base'

fresh_repo head-without-policy
rm "$repo/$POLICY"
run_guard --base origin/main
expect "a change that deletes the policy fails closed" 2 \
  "this change has no readable $POLICY; if the policy is being retired, retire this guard with it"

fresh_repo zero-routes
route_doc HTTPRoute openbao false 'Secrets Management' | write "$ROUTE"
run_guard --base origin/main
expect 'finding zero opted-in routes fails closed instead of passing vacuously' 2 \
  'found zero routes with gethomepage.dev/enabled "true" under k8s/'
expect_absent 'an empty walk never reports a pass' 'Homepage routes name a group allowed'

fresh_repo unparseable-yaml
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: [unclosed\n' | write k8s/bases/apps/broken/config-map.yaml
run_guard --base origin/main
expect 'a YAML file that does not parse fails closed' 2 'could not parse the YAML under k8s/'

fresh_repo no-base
run_guard
expect 'a missing --base is a usage error, not a pass' 2 '--base is required'

if [ "$failures" -gt 0 ]; then
  printf '%d case(s) failed\n' "$failures" >&2
  exit 1
fi
printf 'guard-homepage-route-groups: every case behaved.\n'
