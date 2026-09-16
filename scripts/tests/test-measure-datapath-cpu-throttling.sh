#!/usr/bin/env bash
# The workflow-shape assertions match GitHub Actions expressions that must appear verbatim, so
# single-quoted `${...}` literals are intended.
# shellcheck disable=SC2016
# Pin the behaviour of scripts/measure-datapath-cpu-throttling.sh.
#
# WHY THIS EXISTS. The measurement decides whether #3790's datapath CPU limit is reopened, and its
# mistakes are silent: a ratio that counts the wrong container, a delta taken across a replaced pod
# or a restarted container, or a "0%" for a container that has no quota at all. So the arithmetic is
# pinned against hand-computed numbers, each INCONCLUSIVE path differs from the passing fixture in
# one change, and the script's read-only verbs, its public-log hygiene and the workflow's shape are
# asserted.
#
# kubectl and sleep are faked; no cluster, no secrets, no network. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/measure-datapath-cpu-throttling.sh"
readonly workflow="${root_dir}/.github/workflows/measure-datapath-cpu-throttling.yaml"

work_dir="$(mktemp -d)"
readonly work_dir
trap 'rm -rf "${work_dir}"' EXIT

readonly fake_bin="${work_dir}/bin"
mkdir -p "${fake_bin}"

output=''
rc=''

fail() {
  printf 'FAIL: %s\n--- actual output (rc=%s) ---\n%s\n---\n' "$1" "${rc:-?}" "${output:-}" >&2
  exit 1
}

require_text() {
  grep -Fq -- "$1" <<<"${output}" || fail "$2"
}

refute_text() {
  if grep -Fq -- "$1" <<<"${output}"; then
    fail "$2"
  fi
}

require_rc() {
  [[ "${rc}" -eq "$1" ]] || fail "$2 (expected rc=$1)"
}

# Fake kubectl: the first `get nodes` starts the first sample, the second starts the second one.
# Anything but a `get` is recorded as forbidden. A failed read writes an address to stderr, the way a
# real connection error does, so the log assertions cover the error path too.
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/calls.log"
[[ "$1 $2" == "--context admin@prod" ]] || { touch "${FIXTURES}/UNEXPECTED_CONTEXT"; exit 1; }
shift 3
[[ "$1" == "get" ]] || { touch "${FIXTURES}/FORBIDDEN_VERB"; exit 1; }
if [[ "$2" == "nodes" ]]; then
  phase=1
  [[ -f "${FIXTURES}/phase" ]] && phase=2
  printf '%s' "${phase}" >"${FIXTURES}/phase"
  cat "${FIXTURES}/${phase}/nodes"
  exit 0
fi
if [[ "$2" == "--raw" ]]; then
  node="${3#/api/v1/nodes/}"
  node="${node%/proxy/metrics/cadvisor}"
  file="${FIXTURES}/$(cat "${FIXTURES}/phase")/${node}.prom"
  if [[ ! -f "${file}" ]]; then
    printf 'Unable to connect to the server: dial tcp 10.0.0.1:6443\n' >&2
    exit 1
  fi
  cat "${file}"
  exit 0
fi
touch "${FIXTURES}/UNEXPECTED_GET"
exit 1
FAKE
cat >"${fake_bin}/sleep" <<'FAKE'
#!/usr/bin/env bash
printf 'sleep %s\n' "$*" >>"${FIXTURES}/calls.log"
FAKE
chmod +x "${fake_bin}/kubectl" "${fake_bin}/sleep"

# series <file> <namespace> <pod> <container> <periods> <throttled>
series() {
  printf 'container_cpu_cfs_periods_total{container="%s",id="/kubepods/x",image="img",name="n",namespace="%s",pod="%s"} %s 1726500000000\n' \
    "$4" "$2" "$3" "$5" >>"$1"
  printf 'container_cpu_cfs_throttled_periods_total{container="%s",id="/kubepods/x",image="img",name="n",namespace="%s",pod="%s"} %s 1726500000000\n' \
    "$4" "$2" "$3" "$6" >>"$1"
}

# The passing fixture: two nodes, one pod of each DaemonSet per node. Hand-computed deltas:
#   cilium-agent   periods (1000+1000) throttled (30+0)  -> 1.500%, worst pod 3.000%  ABOVE-1%
#   cilium-envoy   periods (1000+1000) throttled (5+5)   -> 0.500%, worst 0.500%      within-1%
#   tetragon       periods (200+200)   throttled 0       -> 0.000%
#   csi containers periods (10+10)     throttled 0       -> 0.000%
# Decoys that must NOT be counted: the csi CONTROLLER's liveness-probe (a Deployment pod name) with
# heavy throttling, a cilium-agent container outside kube-system, and a malformed-suffix pod.
fixture() {
  local dir="${work_dir}/case-$1" phase node side mult
  rm -rf "${dir}"
  for phase in 1 2; do
    mkdir -p "${dir}/${phase}"
    printf 'node-a node-b' >"${dir}/${phase}/nodes"
    for node in node-a node-b; do
      side="${node#node-}"
      mult=$phase
      local f="${dir}/${phase}/${node}.prom"
      printf '# HELP container_cpu_cfs_periods_total Number of elapsed enforcement period intervals.\n' >"${f}"
      if [[ "${side}" == 'a' ]]; then
        series "${f}" kube-system "cilium-${side}aaaa" cilium-agent "$((1000 * mult))" "$(((phase - 1) * 30))"
      else
        series "${f}" kube-system "cilium-${side}aaaa" cilium-agent "$((1000 * mult))" 7
      fi
      series "${f}" kube-system "cilium-envoy-${side}bbbb" cilium-envoy "$((1000 * mult))" "$((5 * mult))"
      series "${f}" kube-system "tetragon-${side}cccc" tetragon "$((200 * mult))" 0
      series "${f}" kube-system "tetragon-${side}cccc" export-stdout "$((200 * mult))" "$((200 * mult))"
      series "${f}" kube-system "hcloud-csi-node-${side}dddd" csi-node-driver-registrar "$((10 * mult))" 0
      series "${f}" kube-system "hcloud-csi-node-${side}dddd" liveness-probe "$((10 * mult))" 0
      series "${f}" kube-system "hcloud-csi-node-${side}dddd" hcloud-csi-driver "$((10 * mult))" 0
      series "${f}" kube-system "hcloud-csi-controller-6f7d9c5b8-${side}eeee" liveness-probe "$((100 * mult))" "$((100 * mult))"
      series "${f}" other "cilium-${side}ffff" cilium-agent "$((100 * mult))" "$((100 * mult))"
      series "${f}" kube-system "cilium-${side}ggggggg" cilium-agent "$((100 * mult))" "$((100 * mult))"
      printf 'container_cpu_usage_seconds_total{container="cilium-agent",namespace="kube-system",pod="cilium-%saaaa"} 5\n' "${side}" >>"${f}"
    done
  done
  printf '%s' "${dir}"
}

run() { # <fixture-dir> [args...]
  local dir="$1"
  shift
  rm -f "${dir}/phase" "${dir}/calls.log"
  touch "${dir}/calls.log"
  set +e
  output="$(PATH="${fake_bin}:${PATH}" FIXTURES="${dir}" GITHUB_STEP_SUMMARY='' bash "${script}" "$@" 2>&1)"
  rc=$?
  set -e
  [[ ! -f "${dir}/FORBIDDEN_VERB" && ! -f "${dir}/UNEXPECTED_GET" && ! -f "${dir}/UNEXPECTED_CONTEXT" ]] ||
    fail 'the script issued something other than a read on the pinned context'
}

no_names() {
  local name
  for name in node-a node-b aaaa bbbb cccc dddd eeee 10.0.0.1; do
    refute_text "${name}" "the public log names a node, pod or address (${name})"
  done
}

# 1. The passing fixture measures the hand-computed ratios.
dir="$(fixture pass)"
run "${dir}" --context admin@prod --window-seconds 60
require_rc 0 'the passing fixture must be MEASURED'
require_text 'Verdict: MEASURED' 'no MEASURED verdict'
require_text '| cilium/cilium-agent | 2 | 2000 | 30 | 1.500 | 3.000 | ABOVE-1% |' 'agent ratio wrong'
require_text '| cilium-envoy/cilium-envoy | 2 | 2000 | 10 | 0.500 | 0.500 | within-1% |' 'envoy ratio wrong'
require_text '| tetragon/tetragon | 2 | 400 | 0 | 0.000 | 0.000 | within-1% (not gated) |' 'tetragon ratio wrong (or export-stdout counted)'
require_text '| hcloud-csi-node/liveness-probe | 2 | 20 | 0 | 0.000 | 0.000 | within-1% (not gated) |' 'the csi controller liveness-probe was counted'
grep -Fxq 'sleep 60' "${dir}/calls.log" || fail 'the window was not slept'
no_names

# 2. Summary output goes to the step summary too.
summary="${work_dir}/summary.md"
rm -f "${dir}/phase"
PATH="${fake_bin}:${PATH}" FIXTURES="${dir}" GITHUB_STEP_SUMMARY="${summary}" bash "${script}" --context admin@prod >/dev/null 2>&1 ||
  fail 'the summary run failed'
grep -Fq 'Verdict: MEASURED' "${summary}" || fail 'the verdict did not reach the step summary'
grep -Fxq 'sleep 600' "${dir}/calls.log" || fail 'the default window is not 600 seconds'

# 3-8. Each INCONCLUSIVE path, one change away from the passing fixture.
expect_inconclusive() { # <name> <reason-fragment>
  run "${dir}" --context admin@prod --window-seconds 60
  require_rc 3 "$1: must be INCONCLUSIVE"
  require_text "Verdict: INCONCLUSIVE — $2" "$1: inconclusive for the wrong reason"
  refute_text 'Verdict: MEASURED' "$1: also claimed MEASURED"
  no_names
}

dir="$(fixture node-added)"
printf 'node-a node-b node-c' >"${dir}/2/nodes"
cp "${dir}/2/node-b.prom" "${dir}/2/node-c.prom"
sed -i.bak 's/-b\([a-z]\{4\}\)"/-c\1"/' "${dir}/2/node-c.prom"
expect_inconclusive node-added 'the node set changed'

dir="$(fixture pod-replaced)"
sed -i.bak 's/cilium-envoy-bbbbb/cilium-envoy-bzzzz/' "${dir}/2/node-b.prom"
expect_inconclusive pod-replaced 'the measured pod set changed'

dir="$(fixture counter-reset)"
sed -i.bak 's/\(container_cpu_cfs_periods_total{container="tetragon".*pod="tetragon-acccc"}\) 400/\1 3/' "${dir}/2/node-a.prom"
expect_inconclusive counter-reset 'a counter went backwards'

dir="$(fixture missing)"
for f in "${dir}"/1/*.prom "${dir}"/2/*.prom; do
  sed -i.bak '/container="hcloud-csi-driver"/d' "${f}"
done
expect_inconclusive missing 'not every container could be measured: hcloud-csi-node/hcloud-csi-driver:no-series'

dir="$(fixture no-periods)"
for f in "${dir}"/2/*.prom; do
  sed -i.bak 's/\(container_cpu_cfs_periods_total{container="cilium-envoy".*}\) [0-9]* /\1 1000 /' "${f}"
done
for f in "${dir}"/1/*.prom; do
  sed -i.bak 's/\(container_cpu_cfs_throttled_periods_total{container="cilium-envoy".*}\) [0-9]* /\1 10 /' "${f}"
done
expect_inconclusive no-periods 'not every container could be measured: cilium-envoy/cilium-envoy:no-periods'

dir="$(fixture read-fails)"
rm "${dir}/2/node-b.prom"
expect_inconclusive read-fails 'the second cAdvisor read failed'

dir="$(fixture duplicate)"
grep 'cilium-envoy-abbbb' "${dir}/1/node-a.prom" >>"${dir}/1/node-b.prom"
grep 'cilium-envoy-abbbb' "${dir}/2/node-a.prom" >>"${dir}/2/node-b.prom"
expect_inconclusive duplicate 'a pod reported the same counter twice'

dir="$(fixture bad-value)"
sed -i.bak 's/\(container_cpu_cfs_periods_total{container="cilium-agent".*pod="cilium-aaaaa"}\) [0-9]* /\1 NaN /' "${dir}/1/node-a.prom"
expect_inconclusive bad-value 'the first cAdvisor read failed'

# 9. Usage errors read nothing.
dir="$(fixture usage)"
for args in '' '--context' '--context admin@prod --window-seconds 59' '--context admin@prod --window-seconds 1801' \
  '--context admin@prod --window-seconds 10m' '--context admin@prod --bogus'; do
  # shellcheck disable=SC2086
  run "${dir}" ${args}
  require_rc 1 "usage error expected for: ${args}"
  [[ ! -s "${dir}/calls.log" ]] || fail "a usage error still called kubectl or sleep: ${args}"
done

# 10. The script issues reads only.
if grep -Eq '(^| )(exec|apply|create|delete|patch|edit|label|annotate|scale|cordon|drain|debug|port-forward|proxy)( |$)' "${script}"; then
  fail 'the script mentions a kubectl verb that is not a read'
fi

# 11. The workflow shape that makes dispatching it acceptable.
[[ -f "${workflow}" ]] || fail 'the workflow is missing'
wf="$(cat "${workflow}")"
output="${wf}"
command -v yq >/dev/null 2>&1 || fail 'yq is required'
[[ "$(yq -r '.on | keys | join(",")' "${workflow}")" == 'workflow_dispatch' ]] ||
  fail 'the workflow must be triggered by workflow_dispatch only'
require_text "if [[ \"\${RUN_REF}\" != 'refs/heads/main' ]]; then" 'the main-branch guard is missing'
require_text "if [[ \"\${CONFIRM}\" != 'read-production-datapath-throttling' ]]; then" 'the confirmation guard is missing'
require_text 'permissions: {}' 'top-level permissions must be empty'
require_text 'contents: read # checkout repository' 'the job token must be read-only'
require_text 'environment: prod' 'the job must use the prod environment'
require_text 'persist-credentials: false' 'checkout must not persist credentials'
require_text 'group: prod-deploy' 'the read must be serialised with deploys'
require_text './scripts/use-prod-stable-api-endpoint.sh >/dev/null' 'the endpoint helper output must be discarded'
require_text './scripts/measure-datapath-cpu-throttling.sh --context admin@prod --window-seconds "${WINDOW_SECONDS}"' 'the script invocation is not pinned'
refute_text '${{ inputs.window_seconds }} --' 'the window input must reach bash through env'
guard_line="$(grep -n 'Require the reviewed main branch' "${workflow}" | head -1 | cut -d: -f1)"
checkout_line="$(grep -n 'actions/checkout@' "${workflow}" | head -1 | cut -d: -f1)"
confirm_line="$(grep -n 'Require explicit confirmation' "${workflow}" | head -1 | cut -d: -f1)"
kube_line="$(grep -n 'secrets.KUBE_CONFIG' "${workflow}" | head -1 | cut -d: -f1)"
[[ -n "${guard_line}" && -n "${checkout_line}" && -n "${confirm_line}" && -n "${kube_line}" ]] || fail 'a guard step is missing'
((guard_line < checkout_line && confirm_line < kube_line)) || fail 'the guards must run before checkout and before the kubeconfig'
timeout_minutes="$(sed -n 's/^ *timeout-minutes: *\([0-9]*\)$/\1/p' "${workflow}" | head -1)"
# Worst case: the longest window plus two samples of kubectl calls at their request timeout.
((timeout_minutes * 60 >= 1800 + 600)) || fail 'the job timeout cannot fit the longest window'

printf 'measure-datapath-cpu-throttling: ratios pinned, 8 inconclusive paths, usage, read-only verbs and workflow shape verified.\n'
