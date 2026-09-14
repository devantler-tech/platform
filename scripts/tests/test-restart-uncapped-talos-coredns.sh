#!/usr/bin/env bash

# Talos admits CoreDNS before the kube-system LimitRange exists, so a fresh prod
# cluster runs it uncapped (#3567). Pin that the DR rebuild restarts CoreDNS when —
# and only when — a running pod lacks a CPU limit, that it never restarts anything
# else, and that every unreadable input fails closed without restarting.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/restart-uncapped-talos-coredns.sh"
readonly dr_workflow="${root_dir}/.github/workflows/dr-rebuild.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${script}" ]] || fail 'restart-uncapped-talos-coredns.sh must be an executable script'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

readonly fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CALL_LOG}"
args="$*"
case "${args}" in
*"get limitrange"*)
  if [[ "${LR_FAIL:-0}" == "1" ]]; then
    printf 'Error from server (NotFound): limitranges "default-limitrange" not found\n' >&2
    exit 1
  fi
  printf '%s' "${LR_CPU-2}"
  ;;
*"get deployment"*)
  if [[ "${DEPLOY_FAIL:-0}" == "1" ]]; then
    printf 'Error from server (NotFound): deployments.apps "coredns" not found\n' >&2
    exit 1
  fi
  printf '{"spec":{"selector":{"matchLabels":{"k8s-app":"kube-dns"}}}}'
  ;;
*"rollout restart"*)
  : >"${STATE_DIR}/restarted"
  ;;
*"rollout status"*)
  printf 'deployment "coredns" successfully rolled out\n'
  ;;
*"get pods"*)
  if [[ "${PODS_FAIL:-0}" == "1" ]]; then
    printf 'the server could not find the requested resource\n' >&2
    exit 1
  fi
  if [[ -f "${STATE_DIR}/restarted" ]]; then
    printf '%s' "${PODS_AFTER}"
  else
    printf '%s' "${PODS_BEFORE}"
  fi
  ;;
*)
  printf 'unexpected kubectl call: %s\n' "${args}" >&2
  exit 99
  ;;
esac
FAKE_KUBECTL
chmod +x "${fake_kubectl}"

readonly capped='{"items":[{"metadata":{"name":"coredns-a"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"cpu":"2","memory":"170Mi"}}}]}},{"metadata":{"name":"coredns-b"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"cpu":"2","memory":"170Mi"}}}]}}]}'
readonly uncapped='{"items":[{"metadata":{"name":"coredns-a"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"memory":"170Mi"}}}]}},{"metadata":{"name":"coredns-b"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"cpu":"2","memory":"170Mi"}}}]}}]}'
# After a restart the old uncapped pod can still be Terminating next to capped new ones.
readonly capped_with_terminating='{"items":[{"metadata":{"name":"coredns-old","deletionTimestamp":"2026-09-14T00:00:00Z"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"memory":"170Mi"}}}]}},{"metadata":{"name":"coredns-c"},"spec":{"containers":[{"name":"coredns","resources":{"limits":{"cpu":"2","memory":"170Mi"}}}]}}]}'
readonly no_pods='{"items":[]}'

case_dir=''
status=0
output=''

# run_case <name> [VAR=value ...] — runs the script against the fake with a fresh state.
run_case() {
  local name="$1"
  shift
  case_dir="${tmp_dir}/${name}"
  mkdir -p "${case_dir}"
  : >"${case_dir}/calls"
  set +e
  output="$(env -i PATH="${PATH}" HOME="${HOME}" \
    KUBECTL="${fake_kubectl}" CALL_LOG="${case_dir}/calls" STATE_DIR="${case_dir}" \
    PODS_BEFORE="${capped}" PODS_AFTER="${capped}" "$@" \
    "${script}" 2>&1)"
  status=$?
  set -e
}

restarts() { grep -c 'rollout restart' "${case_dir}/calls" || true; }

expect_status() { # <case> <expected>
  [[ "${status}" -eq "$2" ]] || fail "$1: expected exit $2, got ${status}: ${output}"
}

expect_no_restart() { # <case>
  [[ "$(restarts)" -eq 0 ]] || fail "$1: restarted CoreDNS but must not have"
}

# 1. Healthy cluster: already capped ⇒ nothing restarted (negative control).
run_case healthy PODS_BEFORE="${capped}"
expect_status healthy 0
expect_no_restart healthy

# 2. Fresh cluster: an uncapped pod ⇒ exactly one restart of coredns in kube-system,
#    waited on, and the terminating old pod does not count against the result.
run_case fresh PODS_BEFORE="${uncapped}" PODS_AFTER="${capped_with_terminating}"
expect_status fresh 0
[[ "$(restarts)" -eq 1 ]] || fail "fresh: expected exactly one restart, got $(restarts)"
grep -qxF -- '--context admin@prod -n kube-system rollout restart deployment/coredns' "${case_dir}/calls" ||
  fail 'fresh: the restart must target kube-system deployment/coredns on admin@prod and nothing else'
grep -qF -- 'rollout status deployment/coredns' "${case_dir}/calls" ||
  fail 'fresh: the restart must be waited on'
grep -qF -- '-l k8s-app=kube-dns' "${case_dir}/calls" ||
  fail "fresh: pods must be selected by the Deployment's own selector"

# 3. Every call is pinned to the prod context.
if grep -v -- '^--context admin@prod ' "${case_dir}/calls" | grep -q .; then
  fail 'every kubectl call must pass --context admin@prod'
fi

# 4. Restart did not cap it ⇒ exit 1, not success.
run_case still_uncapped PODS_BEFORE="${uncapped}" PODS_AFTER="${uncapped}"
expect_status still_uncapped 1

# 5–9. Every unreadable or unjudgeable input fails closed, without restarting.
run_case no_limitrange LR_FAIL=1 PODS_BEFORE="${uncapped}"
expect_status no_limitrange 2
expect_no_restart no_limitrange

run_case no_default_cpu LR_CPU='' PODS_BEFORE="${uncapped}"
expect_status no_default_cpu 2
expect_no_restart no_default_cpu

run_case no_deployment DEPLOY_FAIL=1 PODS_BEFORE="${uncapped}"
expect_status no_deployment 2
expect_no_restart no_deployment

run_case pods_unreadable PODS_FAIL=1
expect_status pods_unreadable 2
expect_no_restart pods_unreadable

run_case no_pods PODS_BEFORE="${no_pods}"
expect_status no_pods 2
expect_no_restart no_pods

# 10. Wiring: DR runs it after Flux settles, and CI runs this test when either half changes.
awk '/name: ⏳ Wait for Flux to settle/ { settled = 1 }
  settled && /scripts\/restart-uncapped-talos-coredns\.sh/ { found = 1 }
  END { exit found ? 0 : 1 }' "${dr_workflow}" ||
  fail 'dr-rebuild.yaml must run restart-uncapped-talos-coredns.sh after Flux settles'
grep -qF -- "- 'scripts/restart-uncapped-talos-coredns.sh'" "${ci_workflow}" ||
  fail 'ci.yaml must run this test when the script changes'
grep -qF -- "- 'scripts/tests/test-restart-uncapped-talos-coredns.sh'" "${ci_workflow}" ||
  fail 'ci.yaml must run this test when the test changes'
grep -qF -- 'run: bash scripts/tests/test-restart-uncapped-talos-coredns.sh' "${ci_workflow}" ||
  fail 'ci.yaml must run this test'

printf 'restart-uncapped-talos-coredns: all cases passed\n'
