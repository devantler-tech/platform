#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
readback="$root_dir/scripts/verify-kubelet-config-readback.sh"
deploy_action="$root_dir/.github/actions/deploy-prod/action.yml"
ci_workflow="$root_dir/.github/workflows/ci.yaml"

[[ -x "$readback" ]] || {
  printf 'kubelet settings readback is missing or not executable\n' >&2
  exit 1
}

if ! grep -Fq 'scripts/verify-kubelet-config-readback.sh' "$deploy_action" ||
  ! grep -Fq "'scripts/tests/test-kubelet-config-readback.sh'" "$ci_workflow" ||
  ! grep -Fq 'bash scripts/tests/test-kubelet-config-readback.sh' "$ci_workflow"; then
  printf 'CI and deployment must execute the kubelet settings readback contract\n' >&2
  exit 1
fi

stability_line=$(grep -nF './scripts/wait-for-prod-api-stability.sh' "$deploy_action" | cut -d: -f1)
readback_line=$(grep -nF './scripts/verify-kubelet-config-readback.sh' "$deploy_action" | cut -d: -f1)
if [[ -z "$stability_line" || -z "$readback_line" ]] || (( stability_line >= readback_line )); then
  printf 'kubelet settings readback must follow cluster update and API stability\n' >&2
  exit 1
fi

# The deploy step must stay observe-only until the switch is made on purpose,
# and must run only after a successful update and a stable API.
step=$(yq -o=json -I=0 '.runs.steps[] | select(.run == "./scripts/verify-kubelet-config-readback.sh")' "$deploy_action")
if [[ "$(jq -s 'length' <<<"$step")" != 1 ]] ||
  ! jq -e '.env.KUBELET_READBACK_ENFORCE == "false" and
    (.if | contains("steps.cluster_update.outcome == \u0027success\u0027") and
           contains("steps.wait_prod_api_stability.outcome == \u0027success\u0027"))' <<<"$step" >/dev/null; then
  printf 'the deploy must run the kubelet readback once, observe-only, after a successful update\n' >&2
  exit 1
fi

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

# The mock answers the two reads the check makes and nothing else. A node's
# reply is the file named after it; a missing file is a refused read.
mock_kubectl="$tmp_dir/kubectl"
cat >"$mock_kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2 $3" == "--context admin@prod --request-timeout=15s" ]] || exit 64
shift 3
if [[ "$*" == "get nodes -o json" ]]; then
  [[ -z "${MOCK_NODES_FAIL:-}" ]] || exit 1
  if [[ -n "${MOCK_FIRST_DIR:-}" && ! -f "$MOCK_DIR/first-used" ]]; then
    cat "$MOCK_FIRST_DIR/nodes.json"
  else
    cat "$MOCK_DIR/nodes.json"
  fi
  exit 0
fi
[[ "$1 $2" == "get --raw" && "$3" == /api/v1/nodes/*/proxy/configz && $# -eq 3 ]] || exit 64
node=${3#/api/v1/nodes/}
node=${node%/proxy/configz}
dir=$MOCK_DIR
if [[ -n "${MOCK_FIRST_DIR:-}" && ! -f "$MOCK_DIR/first-used" ]]; then
  dir=$MOCK_FIRST_DIR
  [[ "$node" != "${MOCK_LAST_NODE:-}" ]] || : >"$MOCK_DIR/first-used"
fi
[[ -f "$dir/$node.json" ]] || { printf 'Error from server (Forbidden)\n' >&2; exit 1; }
cat "$dir/$node.json"
MOCK
chmod +x "$mock_kubectl"

nodes=(prod-control-plane-2 prod-worker-1 autoscale-cx43-example)

# What a healthy kubelet reports for the settings the repository declares
# today, plus fields the check must ignore.
healthy='{"kubeletconfig":{
  "systemReserved":{"memory":"256Mi","cpu":"50m"},
  "kubeReserved":{"memory":"256Mi"},
  "evictionSoft":{"memory.available":"500Mi"},
  "evictionSoftGracePeriod":{"memory.available":"1m30s"},
  "evictionMinimumReclaim":{"memory.available":"200Mi"},
  "evictionMaxPodGracePeriod":60,
  "mergeDefaultEvictionSettings":true,
  "evictionHard":{"memory.available":"100Mi","nodefs.available":"10%","nodefs.inodesFree":"5%","imagefs.available":"15%","imagefs.inodesFree":"5%"},
  "imageGCHighThresholdPercent":75,
  "imageGCLowThresholdPercent":70,
  "maxPods":110}}'
# The exact shape #3137 found on two autoscaler nodes.
drifted=$(jq '.kubeletconfig.mergeDefaultEvictionSettings = false
  | .kubeletconfig.evictionHard = {"memory.available":"100Mi"}' <<<"$healthy")

new_fixture() { # <dir>: every node healthy
  mkdir -p "$1"
  printf '%s\n' "${nodes[@]}" | jq -R '{metadata: {name: .}}' | jq -s '{items: .}' >"$1/nodes.json"
  for node in "${nodes[@]}"; do printf '%s\n' "$healthy" >"$1/$node.json"; done
}

run() { # <fixture dir> [env assignments...]: sets rc and output
  local dir=$1
  shift
  rc=0
  output=$(env KUBECTL_BIN="$mock_kubectl" MOCK_DIR="$dir" \
    KUBELET_READBACK_ATTEMPTS=1 KUBELET_READBACK_INTERVAL_SECONDS=0 "$@" "$readback" 2>&1) || rc=$?
}

expect() { # <name> <exit code> <fragment>...
  local name=$1 want=$2
  shift 2
  if [[ "$rc" -ne "$want" ]]; then
    printf 'FAIL %s: expected exit %s, got %s:\n%s\n' "$name" "$want" "$rc" "$output" >&2
    exit 1
  fi
  for fragment in "$@"; do
    # A here-string, not a pipe: grep -q stops at the first match.
    if ! grep -qF -- "$fragment" <<<"$output"; then
      printf 'FAIL %s: missing "%s":\n%s\n' "$name" "$fragment" "$output" >&2
      exit 1
    fi
  done
  printf 'ok: %s\n' "$name"
}

new_fixture "$tmp_dir/healthy"
run "$tmp_dir/healthy"
expect 'every node matches the declared settings' 0 \
  'PASS: 3 node(s), 1 of them autoscaler-provisioned' 'mergeDefaultEvictionSettings'

# Keeping the defaults means their values must match too. Presence alone
# accepts disabled/changed eviction, and must include image-filesystem inodes.
for signal in nodefs.available nodefs.inodesFree imagefs.available imagefs.inodesFree; do
  new_fixture "$tmp_dir/missing-$signal"
  jq --arg signal "$signal" 'del(.kubeletconfig.evictionHard[$signal])' <<<"$healthy" \
    >"$tmp_dir/missing-$signal/autoscale-cx43-example.json"
  run "$tmp_dir/missing-$signal"
  expect "missing default $signal is drift" 1 "evictionHard: live has no $signal threshold"

  for threshold in '0%' '1%' ''; do
    new_fixture "$tmp_dir/changed-$signal"
    jq --arg signal "$signal" --arg threshold "$threshold" \
      '.kubeletconfig.evictionHard[$signal] = $threshold' <<<"$healthy" \
      >"$tmp_dir/changed-$signal/autoscale-cx43-example.json"
    run "$tmp_dir/changed-$signal"
    expect "changed default $signal=$threshold is drift" 1 \
      "autoscale-cx43-example: evictionHard: $signal: declared"
    if grep -qF 'PASS:' <<<"$output"; then
      printf 'FAIL: changed eviction threshold reported PASS\n' >&2
      exit 1
    fi
  done
done

run "$tmp_dir/changed-imagefs.inodesFree" KUBELET_READBACK_ENFORCE=false
expect 'observe-only reports a changed default as a warning rather than PASS' 0 \
  '::warning title=Kubelet settings readback (observe-only)::' \
  'evictionHard: imagefs.inodesFree: declared'
if grep -qF 'PASS:' <<<"$output"; then
  printf 'FAIL: observe-only drift reported PASS\n' >&2
  exit 1
fi

# An explicitly declared threshold overrides the inherited Linux default.
mkdir -p "$tmp_dir/talos-custom/cluster"
cp "$root_dir"/talos/cluster/*.yaml "$tmp_dir/talos-custom/cluster/"
printf 'machine:\n  kubelet:\n    extraConfig:\n      evictionHard:\n        imagefs.inodesFree: "7%%"\n' >"$tmp_dir/talos-custom/cluster/zz-custom-eviction.yaml"
new_fixture "$tmp_dir/custom"
for node in "${nodes[@]}"; do
  jq '.kubeletconfig.evictionHard["imagefs.inodesFree"] = "7%"' <<<"$healthy" \
    >"$tmp_dir/custom/$node.json"
done
run "$tmp_dir/custom" TALOS_CONFIG_DIR="$tmp_dir/talos-custom"
expect 'declared custom disk threshold overrides the inherited default' 0 'PASS: 3 node(s)'

jq '.kubeletconfig.evictionHard["imagefs.inodesFree"] = "5%"' <<<"$healthy" \
  >"$tmp_dir/custom/autoscale-cx43-example.json"
run "$tmp_dir/custom" TALOS_CONFIG_DIR="$tmp_dir/talos-custom"
expect 'a live default cannot hide drift from an explicit custom threshold' 1 \
  'evictionHard: imagefs.inodesFree: declared "7%", live "5%"'

new_fixture "$tmp_dir/drift"
printf '%s\n' "$drifted" >"$tmp_dir/drift/autoscale-cx43-example.json"
run "$tmp_dir/drift"
expect 'the drift #3137 found on an autoscaler node fails and names it' 1 \
  'autoscale-cx43-example: mergeDefaultEvictionSettings: declared true, live false' \
  'autoscale-cx43-example: evictionHard: live has no nodefs.available threshold' \
  'autoscale-cx43-example: evictionHard: live has no imagefs.available threshold'
if grep -qF 'prod-worker-1:' <<<"$output"; then
  printf 'FAIL: a healthy node was reported beside the drifted one:\n%s\n' "$output" >&2
  exit 1
fi

new_fixture "$tmp_dir/value"
jq '.kubeletconfig.evictionSoft["memory.available"] = "100Mi"' <<<"$healthy" >"$tmp_dir/value/prod-worker-1.json"
run "$tmp_dir/value"
expect 'a changed value inside a declared map fails' 1 \
  'prod-worker-1: evictionSoft: declared {"memory.available":"500Mi"}, live {"memory.available":"100Mi"}'

new_fixture "$tmp_dir/absent"
jq 'del(.kubeletconfig.imageGCHighThresholdPercent)' <<<"$healthy" >"$tmp_dir/absent/prod-worker-1.json"
run "$tmp_dir/absent"
expect 'a declared setting the kubelet does not report fails' 1 \
  'prod-worker-1: imageGCHighThresholdPercent: declared 75, live null'

# The reproduction in #3137 printed nothing on a refused read. That must
# never read as a pass.
new_fixture "$tmp_dir/refused"
rm "$tmp_dir/refused/autoscale-cx43-example.json"
run "$tmp_dir/refused"
expect 'a node whose kubelet cannot be read fails' 1 \
  'autoscale-cx43-example: kubelet configuration could not be read'

# jq prints nothing for an empty document, so an empty reply must be refused
# before it is compared.
new_fixture "$tmp_dir/blank"
: >"$tmp_dir/blank/prod-worker-1.json"
printf '   \n' >"$tmp_dir/blank/prod-control-plane-2.json"
run "$tmp_dir/blank"
expect 'an empty or blank reply fails' 1 \
  'prod-worker-1: kubelet configuration reply is empty' \
  'prod-control-plane-2: kubelet configuration reply is empty'

new_fixture "$tmp_dir/garbage"
printf 'not json\n' >"$tmp_dir/garbage/prod-worker-1.json"
run "$tmp_dir/garbage"
expect 'a reply that is not JSON fails' 1 'prod-worker-1: kubelet configuration reply is not valid JSON'

new_fixture "$tmp_dir/shapeless"
printf '{}\n' >"$tmp_dir/shapeless/prod-worker-1.json"
run "$tmp_dir/shapeless"
expect 'a reply without a kubelet configuration fails' 1 'prod-worker-1: no kubeletconfig in the reply'

mkdir "$tmp_dir/empty"
printf '{"items": []}\n' >"$tmp_dir/empty/nodes.json"
run "$tmp_dir/empty"
expect 'a cluster reporting no nodes fails' 1 'the cluster reported no nodes'

mkdir "$tmp_dir/unreadable"
printf 'not json\n' >"$tmp_dir/unreadable/nodes.json"
run "$tmp_dir/unreadable"
expect 'an unreadable node list fails' 1 'Kubernetes nodes could not be read from context admin@prod'

run "$tmp_dir/healthy" MOCK_NODES_FAIL=1
expect 'a failed node list read fails' 1 'Kubernetes nodes could not be read from context admin@prod'

mkdir "$tmp_dir/hostile"
printf '{"items": [{"metadata": {"name": "a/../../b"}}]}\n' >"$tmp_dir/hostile/nodes.json"
run "$tmp_dir/hostile"
expect 'a node name that is not a node name is never queried' 1 'unexpected node name; refusing to query it'

# A replacement node that is not answering yet converges within the window.
new_fixture "$tmp_dir/late-first"
rm "$tmp_dir/late-first/autoscale-cx43-example.json"
new_fixture "$tmp_dir/late"
run "$tmp_dir/late" MOCK_FIRST_DIR="$tmp_dir/late-first" MOCK_LAST_NODE=autoscale-cx43-example \
  KUBELET_READBACK_ATTEMPTS=3
expect 'a node that answers on a later attempt passes' 0 'PASS: 3 node(s)'
[[ -f "$tmp_dir/late/first-used" ]] || {
  printf 'FAIL: the late-node case never served its first, incomplete reading\n' >&2
  exit 1
}

# Drift cannot improve by waiting, so it is reported on the first reading.
run "$tmp_dir/drift" KUBELET_READBACK_ATTEMPTS=3
expect 'drift is reported without waiting out the retry window' 1 'did not pass after 1 attempt(s)'
run "$tmp_dir/refused" KUBELET_READBACK_ATTEMPTS=3
expect 'an unread node is retried until the last attempt' 1 'did not pass after 3 attempt(s)'

# Observe-only: the finding is reported as a warning and the deploy stays green.
run "$tmp_dir/drift" KUBELET_READBACK_ENFORCE=false
expect 'observe-only reports drift as a warning and exits 0' 0 \
  '::warning title=Kubelet settings readback (observe-only)::' \
  'autoscale-cx43-example: mergeDefaultEvictionSettings: declared true, live false'
run "$tmp_dir/healthy" KUBELET_READBACK_ENFORCE=false
expect 'observe-only still passes a healthy cluster without a warning' 0 'PASS: 3 node(s)'
if grep -qF '::warning' <<<"$output"; then
  printf 'FAIL: a healthy observe-only run printed a warning:\n%s\n' "$output" >&2
  exit 1
fi
run "$tmp_dir/healthy" KUBELET_READBACK_ENFORCE=maybe
expect 'an unknown enforce value is refused' 2 'invalid KUBELET_READBACK_ENFORCE'

# Nothing declared means nothing was compared; that is not a pass.
mkdir -p "$tmp_dir/talos-none/cluster"
printf 'machine:\n  network: {}\n' >"$tmp_dir/talos-none/cluster/other.yaml"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-none"
expect 'no declared kubelet settings is refused' 2 'nothing to compare'

mkdir -p "$tmp_dir/talos-missing"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-missing"
expect 'a missing patch directory is refused' 2 'no cluster-wide Talos patches found'

mkdir -p "$tmp_dir/talos-role/cluster" "$tmp_dir/talos-role/workers"
cp "$root_dir/talos/cluster/evict-pods-before-oom.yaml" "$tmp_dir/talos-role/cluster/"
printf 'machine:\n  kubelet:\n    extraConfig:\n      maxPods: 200\n' >"$tmp_dir/talos-role/workers/more-pods.yaml"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-role"
expect 'a per-role kubelet setting is refused, not half-compared' 2 'are not modelled by this check'

# Observe-only must not fail the deploy on a refusal either: a red step skips
# the steps after it.
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-role" KUBELET_READBACK_ENFORCE=false
expect 'observe-only reports a refusal as a warning and exits 0' 0 \
  '::warning title=Kubelet settings readback (observe-only)::' 'are not modelled by this check'
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-missing" KUBELET_READBACK_ENFORCE=false
expect 'observe-only reports a missing patch directory as a warning' 0 '::warning'

# A role file with several documents and no kubelet settings is not a refusal.
mkdir -p "$tmp_dir/talos-multi/cluster" "$tmp_dir/talos-multi/workers"
cp "$root_dir"/talos/cluster/*.yaml "$tmp_dir/talos-multi/cluster/"
printf 'machine:\n  network: {}\n---\nmachine:\n  sysctls: {}\n' >"$tmp_dir/talos-multi/workers/two-documents.yaml"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-multi"
expect 'a multi-document role file without kubelet settings passes' 0 'PASS: 3 node(s)'
printf 'machine:\n  network: {}\n---\nmachine:\n  kubelet:\n    extraConfig:\n      maxPods: 200\n' >"$tmp_dir/talos-multi/workers/two-documents.yaml"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-multi"
expect 'a kubelet setting in a later document of a role file is refused' 2 'are not modelled by this check'
printf 'machine: [unclosed\n' >"$tmp_dir/talos-multi/workers/two-documents.yaml"
run "$tmp_dir/healthy" TALOS_CONFIG_DIR="$tmp_dir/talos-multi"
expect 'a role file that cannot be parsed is refused, not read as empty' 2 'to rule out per-role kubelet settings'

printf 'All kubelet settings readback cases passed.\n'
