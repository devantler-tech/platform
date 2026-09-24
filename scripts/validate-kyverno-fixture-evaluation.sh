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
#      deleted or renamed rule fails even where only `skip` rows name it, and an
#      autogen rule the policy never generates fails too;
#   2. every Excluded row declares `result: skip`, the way a fixture marks a
#      resource the rule is meant to leave alone;
#   3. every resource a results entry names produced a row, and every trigger
#      of a generated object produced its own.
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

# Kyverno's default set of Pod controllers it generates `autogen-*` rules for.
default_autogen_controllers="DaemonSet,Deployment,Job,StatefulSet,ReplicaSet,ReplicationController,CronJob"

# Check 1: every fixture declares results, and every policy and rule it names exists in
# what it loads.
# kyverno prints no results for a fixture that declares none, so only the others are
# expected in its output.
with_results=0
for test_file in "${test_files[@]}"; do
  dir="$(dirname "${test_file}")"
  if [ "$(yq '.results // [] | length' "${test_file}")" -eq 0 ]; then
    finding "${test_file}: declares no results, so it asserts nothing. fix: restore the results the fixture exists to check, or remove the fixture."
    continue
  fi
  with_results=$((with_results + 1))
  : >"${work}/policies"
  : >"${work}/rules"
  while IFS= read -r policy_path; do
    if [ ! -f "${dir}/${policy_path}" ]; then
      finding "${test_file}: loads ${policy_path}, which does not exist"
      continue
    fi
    yq -r 'select(.metadata.name != null) | .metadata.name' "${dir}/${policy_path}" >>"${work}/policies"
    # One line per rule: policy, rule, the kinds its match selects, the controllers Kyverno
    # generates autogen rules for, and whether the policy disables autogen outright because a
    # rule generates, mutates with a JSON patch, or filters a match or exclude by name, names,
    # selector, annotations or Pod mixed with other kinds. Kyverno decides that for the whole
    # policy, so one such rule disables autogen for every rule in it.
    # shellcheck disable=SC2016 # $p, $c, $generates, $patches and $filters are yq variables
    yq -r 'select(.kind == "ClusterPolicy" or .kind == "Policy")
      | .metadata.name as $p
      | (.metadata.annotations["pod-policies.kyverno.io/autogen-controllers"] // "'"${default_autogen_controllers}"'") as $c
      | ([.spec.rules[] | select(.generate != null)] | length > 0) as $generates
      | ([.spec.rules[] | select(.mutate.patchesJson6902 != null or ((.mutate.foreach // []) | any_c(.patchesJson6902 != null)))] | length > 0) as $patches
      | ([.spec.rules[] | (.match, .exclude) | select(. != null)
          | (.resources, ((.any // [])[] | .resources), ((.all // [])[] | .resources)) | select(. != null)
          | select((.name // "") != "" or ((.names // []) | length) > 0 or .selector != null or .annotations != null
              or (((.kinds // []) | length) > 1 and ((.kinds // []) | any_c(. == "Pod" or test("/Pod$")))))] | length > 0) as $filters
      | .spec.rules[]
      | [$p, .name, ([.match.resources.kinds[]?, .match.any[]?.resources.kinds[]?, .match.all[]?.resources.kinds[]?] | join(",")), $c, $generates, $patches, $filters]
      | join("|")' "${dir}/${policy_path}" >>"${work}/rules"
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
    line="$(awk -F'|' -v p="${policy}" -v r="${base}" '$1 == p && $2 == r { print; exit }' "${work}/rules")"
    if [ -z "${line}" ]; then
      finding "${test_file}: names rule ${rule} of policy ${policy}, which the policies it loads do not define. fix: rename the rows to the rule's current name, or remove them if the rule was removed on purpose."
      continue
    fi
    [ "${base}" != "${rule}" ] || continue
    # An autogen rule exists only when Kyverno generates it: the base rule matches Pods and
    # the policy's autogen controllers cover that variant.
    kinds="$(printf '%s' "${line}" | cut -d'|' -f3)"
    controllers="$(printf '%s' "${line}" | cut -d'|' -f4)"
    case ",${kinds}," in
      *,Pod,* | *,v1/Pod,* | */Pod,*) ;;
      *)
        finding "${test_file}: names rule ${rule}, but ${policy}/${base} does not match Pods, so Kyverno generates no ${rule}. fix: name the rule the fixture actually exercises."
        continue
        ;;
    esac
    IFS='|' read -r generates patches filters <<<"$(printf '%s' "${line}" | cut -d'|' -f5-7)"
    autogen_off=""
    [ "${generates}" != true ] || autogen_off="a rule generates resources"
    [ "${patches}" != true ] || autogen_off="${autogen_off:+${autogen_off}; }a rule mutates with patchesJson6902"
    [ "${filters}" != true ] || autogen_off="${autogen_off:+${autogen_off}; }a match or exclude filters by name, names, selector, annotations or Pod mixed with other kinds"
    if [ -n "${autogen_off}" ]; then
      finding "${test_file}: names rule ${rule}, but ${policy} gets no autogen rules because ${autogen_off}. fix: name the rule the fixture actually exercises."
      continue
    fi
    # `autogen-cronjob-*` needs CronJob among the controllers; `autogen-*` needs any other.
    generated=0
    IFS=',' read -ra listed <<<"${controllers}"
    for controller in "${listed[@]}"; do
      case "${rule}:${controller}" in
        autogen-cronjob-*:CronJob) generated=1 ;;
        autogen-cronjob-*:*) ;;
        *:CronJob | *:none | *:) ;;
        *) generated=1 ;;
      esac
    done
    [ "${generated}" -eq 1 ] ||
      finding "${test_file}: names rule ${rule}, but ${policy} generates no such rule (autogen-controllers: ${controllers}). fix: name the rule the fixture actually exercises."
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
  # A generate row names the GENERATED object, not the resource that triggered it, so each
  # generate entry carries the name its expected generated resource declares.
  : >"${work}/generated-names"
  while IFS= read -r generated_file; do
    name="$(yq -r 'select(.metadata.name != null) | .metadata.name' "$(dirname "${test_file}")/${generated_file}" 2>/dev/null | head -n1)" || name=""
    if [ -z "${name}" ]; then
      echo "::error::${test_file}: could not read a name from generatedResource ${generated_file}" >&2
      exit 2
    fi
    printf '%s\t%s\n' "${generated_file}" "${name}" >>"${work}/generated-names"
  done < <(yq -r '.results[] | .generatedResource // ""' "${test_file}" | sort -u | grep -v '^$' || true)
  yq -o=json '.results' "${test_file}" |
    jq --rawfile names "${work}/generated-names" '
      ($names | split("\n") | map(select(. != "") | split("\t") | {(.[0]): .[1]}) | add // {}) as $n
      | map(if .generatedResource then .generatedName = $n[.generatedResource] else . end)' >"${work}/declared.json"

  # Whether a row answers declared entry $e for its resource $r. A row names its resource
  # <apiVersion>/<Kind>/<namespace>/<name>, with an empty namespace for cluster-scoped kinds;
  # a generate row names only the generated object, so it is matched by that name.
  # shellcheck disable=SC2016 # $e and $r are jq variables
  matcher='
    def answers($e; $r):
      .POLICY == $e.policy and (.RULE // "") == ($e.rule // "")
      and ((.RESOURCE | split("/")) as $parts
        | if $e.generatedResource != null then $parts[-1] == $e.generatedName
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

  # Triggers that generate the same fixed-name object share its name, so one row answers the
  # name match above for all of them while kyverno drops a trigger it never loaded. Each
  # trigger produces its own row, so the rows for that object must number the triggers.
  while IFS=$'\t' read -r policy rule name declared got; do
    finding "${test_file}: ${policy}/${rule} declares ${declared} triggers that generate ${name}, but kyverno ran assertions for only ${got} of them. fix: correct each trigger's name or namespace so it matches a resource the test loads."
  done < <(jq -r --slurpfile rows "${rows}" '
    [.[] | select(.generatedResource != null)
      | {policy, rule: (.rule // ""), name: .generatedName, n: ((.resources // []) | length)}]
    | group_by([.policy, .rule, .name])[]
    | {policy: .[0].policy, rule: .[0].rule, name: .[0].name, n: (map(.n) | add)} as $g
    | ([$rows[0][] | select(.POLICY == $g.policy and (.RULE // "") == $g.rule
        and (.RESOURCE | split("/") | last) == $g.name)] | length) as $got
    | select($got > 0 and $got < $g.n)
    | [$g.policy, $g.rule, $g.name, $g.n, $got] | @tsv' "${work}/declared.json")
done

if [ "${sections}" -ne "${with_results}" ]; then
  cat "${work}/output"
  echo "::error::kyverno reported ${sections} test files with results, expected ${with_results}" >&2
  exit 2
fi

if [ "${findings}" -gt 0 ]; then
  echo "${findings} kyverno fixture row(s) cannot detect a broken policy."
  exit 1
fi
echo "All ${#test_files[@]} kyverno test files evaluate every rule they name."
