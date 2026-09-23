#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
readback="$root_dir/scripts/verify-talos-schematic-readback.sh"
deploy_action="$root_dir/.github/actions/deploy-prod/action.yml"
ci_workflow="$root_dir/.github/workflows/ci.yaml"

[[ -x "$readback" ]] || {
  printf 'production Talos schematic readback is missing or not executable\n' >&2
  exit 1
}

if ! grep -Fq 'scripts/verify-talos-schematic-readback.sh' "$deploy_action" ||
  ! grep -Fq "'scripts/tests/test-talos-schematic-readback.sh'" "$ci_workflow" ||
  ! grep -Fq 'bash scripts/tests/test-talos-schematic-readback.sh' "$ci_workflow"; then
  printf 'CI and deployment must execute the Talos schematic readback contract\n' >&2
  exit 1
fi

filters=$(yq -r '.jobs.changes.steps[] | select(.id == "filter") | .with.filters' "$ci_workflow")
if ! printf '%s\n' "$filters" | yq -e '.talos[] | select(. == ".github/actions/deploy-prod/**")' - >/dev/null; then
  printf 'changes to the production deploy action must run the Talos readback test in PR CI\n' >&2
  exit 1
fi

update_line=$(grep -nF './scripts/run-ksail-prod-with-pull-auth.sh cluster update' "$deploy_action" | cut -d: -f1)
stability_line=$(grep -nF './scripts/wait-for-prod-api-stability.sh' "$deploy_action" | cut -d: -f1)
readback_line=$(grep -nF './scripts/verify-talos-schematic-readback.sh' "$deploy_action" | cut -d: -f1)
if [[ -z "$update_line" || -z "$stability_line" || -z "$readback_line" ]] ||
  (( update_line >= stability_line || stability_line >= readback_line )); then
  printf 'schematic readback must follow cluster update and API stability\n' >&2
  exit 1
fi

tmp_dir=$(mktemp -d)
trap 'rm -f "$tmp_dir/kubectl" "$tmp_dir/nodes.json" "$tmp_dir/changed.json" "$tmp_dir/first-used"; rmdir "$tmp_dir"' EXIT
mock_kubectl="$tmp_dir/kubectl"
cat >"$mock_kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == "${MOCK_EXPECTED_ARGS:---context admin@prod --request-timeout=15s get nodes -o json}" ]] || exit 64
if [[ -n "${MOCK_FIRST_NODES_FILE:-}" && ! -f "$MOCK_FIRST_USED_MARKER" ]]; then
  : >"$MOCK_FIRST_USED_MARKER"
  cat "$MOCK_FIRST_NODES_FILE"
  exit 0
fi
cat "$MOCK_NODES_FILE"
MOCK
chmod +x "$mock_kubectl"

if command -v sha256sum >/dev/null 2>&1; then
  desired_schematic=$(sha256sum "$root_dir/talos/factory-schematic.yaml" | awk '{print $1}')
else
  desired_schematic=$(shasum -a 256 "$root_dir/talos/factory-schematic.yaml" | awk '{print $1}')
fi
desired_version=$(yq eval '.spec.cluster.talos.version' "$root_dir/ksail.prod.yaml")

jq -n --arg schematic "$desired_schematic" --arg version "$desired_version" '
  def node($name): {
    metadata: {name: $name, annotations: {"extensions.talos.dev/schematic": $schematic}},
    spec: {unschedulable: false},
    status: {nodeInfo: {osImage: ("Talos (" + $version + ")")},
             conditions: [{type: "Ready", status: "True"}]}
  };
  {items: [
    node("prod-control-plane-2"), node("prod-control-plane-4"), node("prod-control-plane-5"),
    node("prod-worker-1"), node("prod-worker-2"), node("prod-worker-3"),
    node("autoscale-cx43-example")
  ]}
' >"$tmp_dir/nodes.json"

run_readback() {
  MOCK_NODES_FILE="$1" KUBECTL_BIN="$mock_kubectl" \
    TALOS_READBACK_ATTEMPTS=1 TALOS_READBACK_INTERVAL_SECONDS=0 bash "$readback"
}

run_readback "$tmp_dir/nodes.json"
MOCK_NODES_FILE="$tmp_dir/nodes.json" \
  MOCK_EXPECTED_ARGS='--context oidc@prod --request-timeout=15s get nodes -o json' \
  KUBE_CONTEXT=oidc@prod KUBECTL_BIN="$mock_kubectl" \
  TALOS_READBACK_ATTEMPTS=1 TALOS_READBACK_INTERVAL_SECONDS=0 bash "$readback"

assert_rejected() {
  local description=$1
  if run_readback "$tmp_dir/changed.json" >/dev/null 2>&1; then
    printf 'readback accepted %s\n' "$description" >&2
    exit 1
  fi
}

jq '.items[4].metadata.annotations["extensions.talos.dev/schematic"] = "stale-extension-only"' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a stale static-node schematic'

jq '.items[6].metadata.annotations["extensions.talos.dev/schematic"] = "stale-extension-only"' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a stale autoscaled-node schematic'

jq '.items[0].status.nodeInfo.osImage = "Talos (v1.13.9)"' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a stale Talos version'

jq '.items[0].status.conditions[0].status = "False"' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a NotReady node'

MOCK_NODES_FILE="$tmp_dir/nodes.json" MOCK_FIRST_NODES_FILE="$tmp_dir/changed.json" \
  MOCK_FIRST_USED_MARKER="$tmp_dir/first-used" KUBECTL_BIN="$mock_kubectl" \
  TALOS_READBACK_ATTEMPTS=2 TALOS_READBACK_INTERVAL_SECONDS=0 bash "$readback"

jq '.items[0].spec.unschedulable = true' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a cordoned node'

jq 'del(.items[5])' "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_rejected 'a missing static worker'

printf 'PASS: production Talos version, schematic, and node health are read back fail-closed\n'
