#!/usr/bin/env bash
# Fails when a kyverno test fixture cannot detect a broken policy.
#
# `kyverno test` reports a row whose rule never evaluated the resource as
# REASON=Excluded and counts it as passing whatever the row declared. A rule
# that matches nothing, or one that was deleted or renamed, therefore leaves
# the suite green (#3145, #3152, #3392). A declared resource that does not
# exist gets no row at all, so it drops out of the run the same way. Three
# checks close those gaps:
#   1. every rule a fixture names exists in a policy that fixture loads, so a
#      deleted or renamed rule fails even where only `skip` rows name it;
#   2. every Excluded row declares `result: skip`, the way a fixture marks a
#      resource the rule is meant to leave alone;
#   3. every resource a results entry names produced a row.
#
# Usage: validate-kyverno-fixture-evaluation.sh [tests-dir]
# Exit: 0 every fixture evaluates what it declares; 1 a finding or a failing
# kyverno test; 2 the fixtures or kyverno's output could not be read.
set -euo pipefail

tests_dir="${1:-tests}"
if [ ! -d "${tests_dir}" ]; then
  echo "::error::tests directory not found: ${tests_dir}" >&2
  exit 2
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

findings=0
finding() {
  echo "::error::$*"
  findings=$((findings + 1))
}

mapfile -t test_files < <(find "${tests_dir}" -type f -name kyverno-test.yaml | sort)
if [ "${#test_files[@]}" -eq 0 ]; then
  echo "::error::no kyverno-test.yaml found under ${tests_dir}" >&2
  exit 2
fi

# Check 1: every policy and rule a fixture names exists in what it loads.
for test_file in "${test_files[@]}"; do
  dir="$(dirname "${test_file}")"
  : >"${work}/policies"
  : >"${work}/rules"
  while IFS= read -r policy_path; do
    if [ ! -f "${dir}/${policy_path}" ]; then
      finding "${test_file}: loads ${policy_path}, which does not exist"
      continue
    fi
    yq -r 'select(.metadata.name != null) | .metadata.name' "${dir}/${policy_path}" >>"${work}/policies"
    # shellcheck disable=SC2016 # $p is a yq variable
    yq -r 'select(.kind == "ClusterPolicy" or .kind == "Policy") | .metadata.name as $p | .spec.rules[].name | $p + "|" + .' \
      "${dir}/${policy_path}" >>"${work}/rules"
  done < <(yq -r '.policies[]' "${test_file}")

  while IFS=$'\t' read -r policy rule; do
    if [ -z "${rule}" ]; then
      grep -qxF -- "${policy}" "${work}/policies" ||
        finding "${test_file}: names policy ${policy}, which no policy file it loads defines. fix: point the row at the policy's current name."
      continue
    fi
    # Kyverno names the rules it generates for Pod controllers after the rule they come from.
    base="${rule#autogen-cronjob-}"
    base="${base#autogen-}"
    grep -qxF -- "${policy}|${base}" "${work}/rules" ||
      finding "${test_file}: names rule ${rule} of policy ${policy}, which the policies it loads do not define. fix: rename the rows to the rule's current name, or remove them if the rule was removed on purpose."
  done < <(yq -r '.results[] | [.policy, (.rule // "")] | @tsv' "${test_file}" | sort -u)
done

# Check 2: every Excluded row declares `result: skip`.
if ! kyverno test "${tests_dir}" --remove-color -o json >"${work}/output" 2>&1; then
  cat "${work}/output"
  echo "::error::kyverno test ${tests_dir} failed"
  exit 1
fi

# kyverno prints "Loading test  ( <file> ) ..." and then that file's rows as a
# JSON array whose brackets sit alone on their own lines.
awk -v dir="${work}" '
  /^Loading test  \( .* \) \.\.\.$/ {
    file = $0
    sub(/^Loading test  \( /, "", file)
    sub(/ \) \.\.\.$/, "", file)
    n++
    printf "%s\n", file > (dir "/section-" n ".file")
    close(dir "/section-" n ".file")
    next
  }
  $0 == "[]" && n > 0 { printf "[]\n" > (dir "/section-" n ".json"); close(dir "/section-" n ".json"); next }
  $0 == "[" && n > 0 { capture = 1 }
  capture { print > (dir "/section-" n ".json") }
  $0 == "]" && capture { capture = 0; close(dir "/section-" n ".json") }
' "${work}/output"

sections=0
for file_marker in "${work}"/section-*.file; do
  [ -e "${file_marker}" ] || break
  sections=$((sections + 1))
  test_file="$(cat "${file_marker}")"
  rows="${file_marker%.file}.json"
  if [ ! -f "${test_file}" ] || [ ! -f "${rows}" ] || ! jq -e 'type == "array"' "${rows}" >/dev/null 2>&1; then
    cat "${work}/output"
    echo "::error::could not read kyverno's results for ${test_file}" >&2
    exit 2
  fi
  yq -o=json '.results' "${test_file}" >"${work}/declared.json"

  # Whether a row answers declared entry $e for its resource $r. A row names its resource
  # <apiVersion>/<Kind>/<namespace>/<name>, with an empty namespace for cluster-scoped kinds;
  # a generate row names only the generated resource, so it is matched by name.
  # shellcheck disable=SC2016 # $e and $r are jq variables
  matcher='
    def answers($e; $r):
      .POLICY == $e.policy and (.RULE // "") == ($e.rule // "")
      and ((.RESOURCE | split("/")) as $parts
        | if $e.generatedResource != null then $parts[-1] == ($r | split("/") | last)
          else ($parts | length) >= 4
            and $parts[-3] == ($e.kind // "" | split("/") | last)
            and (if ($r | contains("/")) then ($parts[-2] + "/" + $parts[-1]) == $r else $parts[-1] == $r end)
          end);'

  # Check 2: an Excluded row must be declared `result: skip`.
  while IFS=$'\t' read -r policy rule resource declared; do
    if [ -z "${declared}" ]; then
      finding "${test_file}: ${policy}/${rule} never evaluated ${resource}, and no row declares an expectation for it. fix: declare result: skip for it if the rule should leave it alone."
    else
      finding "${test_file}: ${policy}/${rule} never evaluated ${resource}, which declares result: ${declared} (kyverno reported it Excluded). fix: make the rule's match select this resource, or declare result: skip if the rule should leave it alone."
    fi
  done < <(jq -r --slurpfile declared "${work}/declared.json" "${matcher}"'
    .[] | select(.REASON == "Excluded") as $row
    | [ $declared[0][] as $e | ($e.resources // [])[] as $r
        | select($row | answers($e; $r)) | $e.result ] | unique as $results
    | select($results != ["skip"])
    | [$row.POLICY, $row.RULE, $row.RESOURCE, ($results | join(","))] | @tsv' "${rows}")

  # Check 3: a declared resource kyverno never loaded produces no row and no failure.
  while IFS=$'\t' read -r policy rule kind resource; do
    finding "${test_file}: ${policy}/${rule} declares ${kind} ${resource}, but kyverno ran no assertion for it. fix: correct the resource name or namespace so it matches a resource the test loads."
  done < <(jq -r --slurpfile rows "${rows}" "${matcher}"'
    .[] as $e | ($e.resources // [])[] as $r
    | select(any($rows[0][]; answers($e; $r)) | not)
    | [$e.policy, ($e.rule // ""), ($e.kind // ""), $r] | @tsv' "${work}/declared.json")
done

if [ "${sections}" -ne "${#test_files[@]}" ]; then
  cat "${work}/output"
  echo "::error::kyverno reported ${sections} test files, expected ${#test_files[@]}" >&2
  exit 2
fi

if [ "${findings}" -gt 0 ]; then
  echo "${findings} kyverno fixture row(s) cannot detect a broken policy."
  exit 1
fi
echo "All ${#test_files[@]} kyverno test files evaluate every rule they name."
