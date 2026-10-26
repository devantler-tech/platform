#!/usr/bin/env bash
# Exercise the required aggregate's real policy-result boundary. Removing its
# prerequisite or accepting a selected non-success must fail this regression.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
workflow="${root}/.github/workflows/ci.yaml"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

gate="$(yq -o=json '.jobs.ci-required-checks' "${workflow}")"
policy="$(yq -o=json '.jobs.validate-cluster-policies' "${workflow}")"
if ! jq -e --arg selection "github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'" '
  .needs == ["changes"] and .if == $selection
' <<<"${policy}" >/dev/null; then
  echo 'FAIL: independent policy job scheduling changed' >&2
  exit 1
fi

# Exercise the authored matrix routing, not a second list of validation commands.
# Every non-setup step must run in exactly one declared suite. An omitted or
# duplicate suite, a misspelled condition, or an unconditional regression would
# otherwise lose coverage or run the same tests repeatedly.
cat >"${work}/partition.jq" <<'JQ'
  def partition($job; $groups; $setup):
    ($job.strategy.matrix.suite // []) as $suites |
    [$job.steps[] | select(.name as $name | ($setup | index($name)) == null)] as $checks |
    ($job["runs-on"] == "ubuntu-latest") and
    ($job.permissions == {contents: "read"}) and
    (($job["continue-on-error"] // false) == false) and
    ($job.strategy["fail-fast"] == false) and
    ($job.strategy["max-parallel"] == 3) and
    (($job.strategy.matrix // {} | keys) == ["suite"]) and
    (($suites | sort) == ($groups | sort)) and
    (($suites | unique | length) == ($suites | length)) and
    all($setup[]; . as $name |
      ([$job.steps[] | select(.name == $name and .if == null)] | length) == 1) and
    all($job.steps[]; ((.["continue-on-error"] // false) == false)) and
    all($checks[]; . as $step |
      ([$suites[] | select($step.if == ("matrix.suite == \u0027" + . + "\u0027"))] | length) == 1) and
    all($suites[]; . as $suite |
      ([$checks[] | select(.if == ("matrix.suite == \u0027" + $suite + "\u0027"))] | length) > 0);
  partition(.jobs["validate-cluster-policies"];
    ["fixtures", "tenant", "services", "placement", "storage", "observability"];
    ["📑 Checkout", "⚙️ Setup Go", "📌 Install the reviewed YAML parser",
     "⚖️ Read Kyverno CLI version", "⚖️ Install Kyverno CLI"])
JQ

# partition_result distinguishes a rejected matrix from an unreadable routing observation.
partition_result() {
  local input="$1" result=0
  jq -e -f "${work}/partition.jq" "$input" >"${work}/partition-result" || result=$?
  case "$result" in
    0 | 1) return "$result" ;;
    *)
      echo 'FAIL: CI partition observation could not be evaluated' >&2
      return 2
      ;;
  esac
}

yq -o=json '.' "$workflow" >"${work}/workflow.json"
if ! partition_result "${work}/workflow.json"; then
  echo 'FAIL: CI matrices must retain exactly one scheduled copy of every validation step' >&2
  exit 1
fi

# reject_partition requires each broken routing to fail for a policy verdict, not a parser error.
reject_partition() {
  local name="$1" mutation="$2" result=0
  jq "$mutation" "${work}/workflow.json" >"${work}/mutated.json"
  partition_result "${work}/mutated.json" || result=$?
  if [ "$result" -ne 1 ]; then
    printf 'FAIL: %s must reject incomplete routing, got %s\n' "$name" "$result" >&2
    exit 1
  fi
}

job=validate-cluster-policies
reject_partition "missing suite" "del(.jobs[\"$job\"].strategy.matrix.suite[0])"
reject_partition "duplicate suite" \
  ".jobs[\"$job\"].strategy.matrix.suite += [.jobs[\"$job\"].strategy.matrix.suite[0]]"
reject_partition "unknown suite" ".jobs[\"$job\"].strategy.matrix.suite += [\"unknown\"]"
reject_partition "early cancellation" ".jobs[\"$job\"].strategy[\"fail-fast\"] = true"
reject_partition "ignored failure" ".jobs[\"$job\"][\"continue-on-error\"] = true"
reject_partition "unselected regression" \
  "(.jobs[\"$job\"].steps[] | select(.if != null) | .if) = \"matrix.suite == 'unknown'\""
reject_partition "repeated regression" \
  "(.jobs[\"$job\"].steps[] | select(.if != null)) |= del(.if)"

echo 'PASS: the policy matrix preserves one scheduled copy of every regression; 7 broken routings refused'

jq -e '
  .jobs["validate-publication-generator"] as $job |
  ($job.permissions == {contents: "read"}) and
  ($job | has("if") | not) and
  (($job["continue-on-error"] // false) == false) and
  all($job.steps[]; .if == null and ((.["continue-on-error"] // false) == false)) and
  ([$job.steps[] | select(.name == "🔐 Validate the approved-revisions generator"
    and .if == null and (.run | type == "string" and length > 0))] | length) == 1 and
  (.jobs["ci-required-checks"].needs | index("validate-publication-generator") != null) and
  (.jobs["deploy-prod"].needs | index("validate-publication-generator") != null)
' "${work}/workflow.json" >/dev/null
generator_step="$(jq -c '[.steps[] | select(.name == "🔐 Require publication generator validation")]' <<<"${gate}")"
jq -e 'length == 1 and .[0].env == {
  GENERATOR_RESULT: "${{ needs.validate-publication-generator.result }}"
} and (.[0] | has("if") | not)
  and ((.[0]["continue-on-error"] // false) == false)
  and (.[0].run | type == "string" and length > 0)' <<<"${generator_step}" >/dev/null
jq -r '.[0].run' <<<"${generator_step}" >"${work}/generator-gate.sh"
for result in success failure cancelled skipped pending unknown ''; do
  actual=0
  GENERATOR_RESULT="$result" bash -e "${work}/generator-gate.sh" >"${work}/generator-output" 2>&1 || actual=$?
  expected=1
  [ "$result" != success ] || expected=0
  if [ "$actual" -ne "$expected" ]; then
    printf 'FAIL: generator result=%s expected=%s actual=%s\n' "$result" "$expected" "$actual" >&2
    exit 1
  fi
done
echo 'PASS: the generator is required before deployment and every non-success result is refused'
if ! jq -e '.needs | index("validate-cluster-policies") != null' <<<"${gate}" >/dev/null; then
  echo 'FAIL: required aggregate must wait for the independent policy job' >&2
  exit 1
fi
step="$(jq -c '[.steps[] | select(.name == "⚖️ Require selected policy validation")]' <<<"${gate}")"
jq -e 'length == 1 and (.[0].run | type == "string" and length > 0)' <<<"${step}" >/dev/null
jq -r '.[0].run' <<<"${step}" >"${work}/gate.sh"
# Check the authored boundary mappings, not only an isolated copy of its shell.
jq -e '.[0].env == {
  EVENT_NAME: "${{ github.event_name }}",
  SELECTED: "${{ needs.changes.outputs.k8s }}",
  POLICY_RESULT: "${{ needs.validate-cluster-policies.result }}"
}' <<<"${step}" >/dev/null

# check exercises the actual aggregate decision with controlled upstream job outcomes.
check() {
  local event="$1" selected="$2" result="$3" expected="$4" actual=0
  EVENT_NAME="${event}" SELECTED="${selected}" POLICY_RESULT="${result}" \
    bash -e "${work}/gate.sh" >"${work}/output" 2>&1 || actual=$?
  if [ "${actual}" -ne "${expected}" ]; then
    printf 'FAIL: event=%s selected=%s result=%s expected=%s actual=%s\n' \
      "${event}" "${selected}" "${result}" "${expected}" "${actual}" >&2
    cat "${work}/output" >&2
    exit 1
  fi
}

check pull_request true success 0
for result in failure cancelled skipped pending unknown ''; do
  check pull_request true "${result}" 1
done
check pull_request false skipped 0
check pull_request '' skipped 1
check pull_request unexpected skipped 1
check merge_group true skipped 0
check merge_group false skipped 0

echo 'PASS: selected policy validation must succeed; intentional non-selection is accepted'

jq -e --arg selection "github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'" '
  . as $workflow |
  .jobs["validate-manifest-regressions"] as $job |
  ($job.needs == ["changes"]) and ($job.if == $selection) and
  ($job.permissions == {contents: "read"}) and
  (($job["continue-on-error"] // false) == false) and
  (["🐤 Validate staged OpenBao standby repair",
    "🔒 Validate Crossplane provider egress policy",
    "✉️ Prove native mail accounts and recipient restrictions",
    "🔐 Validate Kyverno review admission boundaries",
    "🗺️ Validate every unscanned namespace is reviewed",
    "🔐 Validate the shared-publish-workflow pin guard",
    "🩺 Validate declarative Coroot application health"] | all(.[];
      . as $name |
      ([$job.steps[] | select(.name == $name and
        .if == "needs.changes.outputs.k8s == '\''true'\''" and
        ((.["continue-on-error"] // false) == false) and
        (.run | type == "string" and length > 0))] | length) == 1 and
      ([$workflow.jobs.validate.steps[]? | select(.name == $name)] | length) == 0)) and
  (.jobs["ci-required-checks"].needs | index("validate-manifest-regressions") != null)
' "${work}/workflow.json" >/dev/null
regression_step="$(jq -c '[.steps[] | select(.name == "🧪 Require selected manifest regressions")]' <<<"${gate}")"
jq -e 'length == 1 and .[0].env == {
  EVENT_NAME: "${{ github.event_name }}",
  SELECTED: "${{ needs.changes.outputs.k8s }}",
  REGRESSION_RESULT: "${{ needs.validate-manifest-regressions.result }}"
} and (.[0] | has("if") | not)
  and ((.[0]["continue-on-error"] // false) == false)
  and (.[0].run | type == "string" and length > 0)' <<<"${regression_step}" >/dev/null
jq -r '.[0].run' <<<"${regression_step}" >"${work}/regression-gate.sh"
for event in pull_request merge_group; do
  for selected in true false unknown ''; do
    for result in success failure cancelled skipped pending unknown ''; do
      expected=1 actual=0
      case "$event:$selected:$result" in
        pull_request:true:success | pull_request:false:skipped | merge_group:*:skipped) expected=0 ;;
      esac
      EVENT_NAME="$event" SELECTED="$selected" REGRESSION_RESULT="$result" \
        bash -e "${work}/regression-gate.sh" >"${work}/regression-output" 2>&1 || actual=$?
      if [ "$actual" -ne "$expected" ]; then
        printf 'FAIL: manifest regressions event=%s selected=%s result=%s expected=%s actual=%s\n' \
          "$event" "$selected" "$result" "$expected" "$actual" >&2
        exit 1
      fi
    done
  done
done
echo 'PASS: all seven manifest regressions run once and selected non-success outcomes are refused'
