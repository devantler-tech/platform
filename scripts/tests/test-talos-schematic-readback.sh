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
if ! printf '%s\n' "$filters" | yq -e '.talos[] | select(. == "scripts/talos-boot-image-drift-accepted.tsv")' - >/dev/null; then
  printf 'a change to the accepted boot-image drift record must run the Talos readback test in PR CI\n' >&2
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
trap 'rm -f "$tmp_dir/kubectl" "$tmp_dir/nodes.json" "$tmp_dir/changed.json" "$tmp_dir/first-used" "$tmp_dir/accepted.tsv"; rmdir "$tmp_dir"' EXIT
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

# A reviewed record may accept ONE static node that an earlier failed deploy left on another
# image (#4665). Everything below runs against a record file of the test's own.
other_schematic=$(printf '%064d' 0 | tr 0 a)
accepted="$tmp_dir/accepted.tsv"
header=$'node\tschematic\tissue\texpires'
record() {
  printf '%s\n' "$header" >"$accepted"
  (($# == 0)) || printf '%s\n' "$@" >>"$accepted"
}
row() {
  printf '%s\t%s\t%s\t%s' "$1" "$2" "${3:-devantler-tech/platform#4665}" "${4:-2026-10-22}"
}
drift_on() {
  jq --argjson index "$1" --arg schematic "$2" \
    '.items[$index].metadata.annotations["extensions.talos.dev/schematic"] = $schematic' \
    "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
}
run_with_record() {
  MOCK_NODES_FILE="$1" KUBECTL_BIN="$mock_kubectl" TALOS_READBACK_ACCEPTED_DRIFT_FILE="$accepted" \
    TALOS_READBACK_TODAY="${2:-2026-10-08}" \
    TALOS_READBACK_ATTEMPTS=1 TALOS_READBACK_INTERVAL_SECONDS=0 bash "$readback"
}
# A run that must succeed and say one thing, and must not say another. Arguments: description,
# nodes file, text the output must contain, text it must not contain.
assert_passes_saying() {
  local description=$1 output
  if ! output=$(run_with_record "$2" 2>&1); then
    printf 'readback refused %s: %s\n' "$description" "$output" >&2
    exit 1
  fi
  if [[ "$output" != *"$3"* || ( -n "$4" && "$output" == *"$4"* ) ]]; then
    printf 'readback passed %s without saying the right thing: %s\n' "$description" "$output" >&2
    exit 1
  fi
}
# A run that must be refused for its own reason. Arguments: description, nodes file, the text
# the refusal must contain, and optionally the date the run believes it is.
assert_refused_saying() {
  local description=$1 output status=0
  output=$(run_with_record "$2" "${4:-2026-10-08}" 2>&1) || status=$?
  if [[ "$status" != 1 ]]; then
    printf 'readback returned %s for %s: %s\n' "$status" "$description" "$output" >&2
    exit 1
  fi
  if [[ "$output" != *"$3"* ]]; then
    printf 'readback refused %s for another reason: %s\n' "$description" "$output" >&2
    exit 1
  fi
}

# The recorded node on the recorded image passes, as a warning and never as the plain pass.
record "$(row prod-worker-1 "$other_schematic")"
drift_on 3 "$other_schematic"
assert_passes_saying 'the recorded node on the recorded image' "$tmp_dir/changed.json" \
  'PASS-WITH-ACCEPTED-DRIFT: 1 of 7 Talos nodes runs a recorded boot image' 'PASS: all Talos nodes'
assert_passes_saying 'the recorded node on the recorded image' "$tmp_dir/changed.json" \
  '::warning::prod-worker-1 runs the recorded boot image' ''

# The record covers that node and that image, nothing else.
drift_on 4 "$other_schematic"
assert_refused_saying 'drift on a node the record does not name' "$tmp_dir/changed.json" \
  'did not converge'
drift_on 3 "$(printf '%064d' 0 | tr 0 b)"
assert_refused_saying 'the recorded node on an image the record does not name' "$tmp_dir/changed.json" \
  'did not converge'
jq --arg schematic "$other_schematic" \
  '.items[3].metadata.annotations["extensions.talos.dev/schematic"] = $schematic |
   .items[4].metadata.annotations["extensions.talos.dev/schematic"] = $schematic' \
  "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_refused_saying 'a second node on the recorded image' "$tmp_dir/changed.json" 'did not converge'

# Every other requirement still binds the recorded node.
jq --arg schematic "$other_schematic" \
  '.items[3].metadata.annotations["extensions.talos.dev/schematic"] = $schematic |
   .items[3].status.conditions[0].status = "False"' "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_refused_saying 'the recorded node while NotReady' "$tmp_dir/changed.json" 'did not converge'
jq --arg schematic "$other_schematic" \
  '.items[3].metadata.annotations["extensions.talos.dev/schematic"] = $schematic |
   .items[3].spec.unschedulable = true' "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_refused_saying 'the recorded node while cordoned' "$tmp_dir/changed.json" 'did not converge'
jq --arg schematic "$other_schematic" \
  '.items[3].metadata.annotations["extensions.talos.dev/schematic"] = $schematic |
   .items[3].status.nodeInfo.osImage = "Talos (v1.13.9)"' "$tmp_dir/nodes.json" >"$tmp_dir/changed.json"
assert_refused_saying 'the recorded node on another Talos version' "$tmp_dir/changed.json" \
  'did not converge'

# The record lapses on its date, and says so.
drift_on 3 "$other_schematic"
assert_refused_saying 'an expired record' "$tmp_dir/changed.json" \
  'the accepted boot-image drift for prod-worker-1 expired on 2026-10-22' 2026-10-23
assert_passes_saying 'a record on its last day' "$tmp_dir/changed.json" 'PASS-WITH-ACCEPTED-DRIFT' ''

# A record nobody needs any more is reported, and the pass is the plain one.
assert_passes_saying 'a record the node no longer needs' "$tmp_dir/nodes.json" \
  '::notice::the accepted boot-image drift for prod-worker-1 is no longer needed' 'PASS-WITH-ACCEPTED-DRIFT'
assert_passes_saying 'a record the node no longer needs' "$tmp_dir/nodes.json" 'PASS: all Talos nodes' ''

# A record that cannot be read as exactly one reviewed row is refused before any node is read.
drift_on 3 "$other_schematic"
invalid='invalid accepted boot-image drift record'
record "$(row prod-worker-1 "$other_schematic")" "$(row prod-worker-2 "$other_schematic")"
assert_refused_saying 'a record with two rows' "$tmp_dir/changed.json" "$invalid: more than one row"
record "$(row autoscale-cx43-example "$other_schematic")"
assert_refused_saying 'a record for an autoscaled node' "$tmp_dir/changed.json" "$invalid: node"
record "$(row prod-worker-1 not-a-schematic)"
assert_refused_saying 'a record with a malformed image' "$tmp_dir/changed.json" "$invalid: schematic"
record "$(row prod-worker-1 "$desired_schematic")"
assert_refused_saying 'a record naming the expected image' "$tmp_dir/changed.json" "$invalid: schematic"
record "$(row prod-worker-1 "$other_schematic" 'see the chat')"
assert_refused_saying 'a record with no issue' "$tmp_dir/changed.json" "$invalid: issue"
record "$(row prod-worker-1 "$other_schematic" devantler-tech/platform#4665 someday)"
assert_refused_saying 'a record with no date' "$tmp_dir/changed.json" "$invalid: expires"
record "$(row prod-worker-1 "$other_schematic" devantler-tech/platform#4665 2026-10-22)"$'\textra'
assert_refused_saying 'a record with a fifth field' "$tmp_dir/changed.json" "$invalid: fields"
printf '%s\n' $'node\tschematic\texpires' "$(row prod-worker-1 "$other_schematic")" >"$accepted"
assert_refused_saying 'a record with another header' "$tmp_dir/changed.json" "$invalid: header"
: >"$accepted"
assert_refused_saying 'an empty record file' "$tmp_dir/changed.json" "$invalid: header"

# A record with a header and no row accepts nothing.
record
assert_refused_saying 'drift with an empty record' "$tmp_dir/changed.json" 'did not converge'
assert_passes_saying 'healthy nodes with an empty record' "$tmp_dir/nodes.json" 'PASS: all Talos nodes' ''

# The record in the repository is one this check can read.
MOCK_NODES_FILE="$tmp_dir/nodes.json" KUBECTL_BIN="$mock_kubectl" TALOS_READBACK_TODAY=2026-10-08 \
  TALOS_READBACK_ATTEMPTS=1 TALOS_READBACK_INTERVAL_SECONDS=0 bash "$readback" >/dev/null

printf 'PASS: production Talos version, schematic, and node health are read back fail-closed\n'
