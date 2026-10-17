#!/usr/bin/env bash
# Report Kubescape scan records whose scanned object no longer exists (#3697).
#
# THE PROBLEM. A scan record is not proof that its object exists. Two upstream components remove a
# record after its object is deleted, and neither covers this cluster:
#   - the operator deletes a record when it sees the object's deletion, but only for the kinds its
#     continuous-scanning watch names. That watch is deliberately empty here (see
#     `continuousScanning` in the kubescape HelmRelease), and an event it misses is never revisited;
#   - the storage service's periodic cleanup compares records with the live cluster, but only for
#     Pods, CronJobs, DaemonSets, Deployments, Jobs, ReplicaSets and StatefulSets. That list is
#     hard-coded, in the pinned v0.0.297 and in v0.0.348 alike, and every other kind is skipped.
# So a record of any other kind (RBAC, ServiceAccounts, Secrets, ConfigMaps, Services, policies,
# host data, ...) outlives its object indefinitely. Anything that reads the scan set as an inventory
# then counts findings against objects that are gone, and answers "does X still exist?" with yes.
#
# WHAT IT DOES — read-only: `api-resources` and `get` only; nothing is created, changed or deleted.
#   1. `get <records> -A -o json`        the scan records. A LIST carries identities only; Kubescape
#                                        strips `spec.controls` from it.
#   2. `api-resources`                   the kinds the cluster serves, with their preferred version
#                                        and scope.
#   3. `get <resource> [-A]`             one list per kind the records name, as a TABLE. kubectl asks
#                                        the API server for names and columns only, so no object
#                                        body — in particular no Secret data — is requested.
# Each record is then compared with the list of its kind.
#
# THE OBJECT A RECORD DESCRIBES is the one its `kubescape.io/wlid` annotation names:
# `wlid://cluster-<c>/namespace-<ns>/<kind>-<name>`. The `kubescape.io/workload-name` label is not
# used, because a label cannot hold every object name (`system:auth-delegator` is stored with the
# colon replaced). The API group comes from the `kubescape.io/workload-api-group` label and the
# version from the cluster, since a record can name a version that is no longer served. A record
# whose label kind differs from its wlid kind describes an RBAC subject; its wlid names the binding
# that grants the subject, and the record is checked against that binding.
#
# EVERY RECORD ENDS IN ONE OF THREE CLASSES
#   live      the list of its kind was read and holds the object.
#   orphaned  the list of its kind was read and does not hold the object.
#   unknown   nothing was observed either way: the list failed (a credential that may not list
#             Secrets leaves every Secret record here), the cluster does not serve the kind or
#             serves it under more than one name, or the record carries no usable reference.
# A record is never called orphaned because a read failed.
#
# WHAT IT PRINTS. Kinds, counts and the verdict — safe for a public log. kubectl's stderr is
# discarded, because a refusal names the caller and the API endpoint. `--list <class>` writes that
# class's records to stdout, one per line and tab-separated:
#   <record namespace> <record name> <kind> <object namespace> <object name>
# and moves the report to stderr. No field is ever empty — a cluster-scoped object has `-` for its
# namespace — so a shell `read` splitting on tabs cannot shift the columns. Those rows are
# object-level detail: keep them out of issues, pull requests and workflow logs. `--list live` is
# the set to hydrate when a posture count must cover only objects that exist
# (docs/kubescape-exception-oracle.md).
#
# EXIT CODES
#   0  CLEAN     every record was checked and describes an object that exists
#   1  ORPHANED  at least one record describes an object that is gone (others may be unknown)
#   2  UNKNOWN   no orphan was found but something could not be checked, nothing could be read, or
#                the arguments were wrong — never a pass
#
# Bash 3.2 compatible so it runs on a maintainer's macOS as well as CI.
set -euo pipefail

readonly record_group='spdx.softwarecomposition.kubescape.io'
readonly request_timeout='60s'

usage() {
  printf 'Usage: %s --context <kube-context> [--resource scans|summaries] [--list live|orphaned|unknown]\n' \
    "$(basename "$0")"
}

usage_error() {
  usage >&2
  exit 2
}

context=''
resource='scans'
list=''
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --context | --resource | --list)
      if [[ "$#" -lt 2 || -z "$2" ]]; then
        usage_error
      fi
      case "$1" in
        --context) context="$2" ;;
        --resource) resource="$2" ;;
        --list) list="$2" ;;
      esac
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage_error
      ;;
  esac
done

[[ -n "${context}" ]] || usage_error
case "${resource}" in
  scans) records='workloadconfigurationscans' ;;
  summaries) records='workloadconfigurationscansummaries' ;;
  *) usage_error ;;
esac
case "${list}" in
  '' | live | orphaned | unknown) ;;
  *) usage_error ;;
esac

# Exit 1 means ORPHANED, so a failure before anything was read must not leave with it.
work_dir="$(mktemp -d)" || exit 2
readonly work_dir

# The report goes to stdout, or to stderr when stdout carries a record list.
report() {
  if [[ -n "${list}" ]]; then
    printf '%s\n' "$1" >&2
  else
    printf '%s\n' "$1"
  fi
}

# A run that stops before it reaches a verdict proved nothing. Bash 3.2 can hand an EXIT trap a zero
# status for an aborted script, so reaching a verdict is recorded explicitly and anything else
# leaves as UNKNOWN.
finished=''
# Invoked by the EXIT trap below. CI's shellcheck reports that as SC2317, newer releases as SC2329.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local status=$?
  rm -rf "${work_dir}"
  if [[ -z "${finished}" ]]; then
    report 'Verdict: UNKNOWN — the report stopped before reaching a verdict'
    status=2
  fi
  exit "${status}"
}
trap cleanup EXIT

finish() {
  finished=true
  exit "$1"
}

unknown() {
  report "Verdict: UNKNOWN — $1"
  finish 2
}

kube() {
  kubectl --context "${context}" --request-timeout="${request_timeout}" "$@" 2>/dev/null </dev/null
}

# 1. The scan records, as `R <namespace> <name> <label kind> <label group> <wlid>`. The kind and the
#    group are printed in the report, so anything outside the label alphabet is replaced. A wlid
#    holding a control character, a backslash or a blank is dropped, because a list could only
#    ever "fail to find" it: `@tsv` rewrites the first two into a different name, and a blank
#    inside a wlid cannot be told from one that belongs to the reference itself.
# shellcheck disable=SC2016 # jq program, not shell expansion
readonly record_program='
  def labelled($key): (.metadata.labels[$key]? // "") | tostring | gsub("[^A-Za-z0-9_.-]"; "?");
  def reference:
    (.metadata.annotations["kubescape.io/wlid"]? // "" | tostring)
    | if test("[[:cntrl:][:space:]\\\\]") then "" else . end;
  if (.items | type) != "array" then error("the reply holds no record list") else .items[] end
  | ["R",
     (.metadata.namespace // ""),
     (.metadata.name // ""),
     labelled("kubescape.io/workload-kind"),
     labelled("kubescape.io/workload-api-group"),
     reference]
  | @tsv
'
if ! kube get "${records}.${record_group}" -A -o json >"${work_dir}/records.json" ||
  ! jq -r "${record_program}" "${work_dir}/records.json" >"${work_dir}/records.tsv" 2>/dev/null; then
  unknown 'the scan records could not be read, so nothing was checked'
fi
if [[ ! -s "${work_dir}/records.tsv" ]]; then
  unknown 'no scan record was returned, so nothing was checked'
fi

# 2. The served kinds, as `D <group> <kind, lower-case> <resource> <version> <namespaced>`. The
#    SHORTNAMES column may be empty, so the columns are counted from the right. A failed discovery
#    can still print the groups it reached; those are used, and the rest stay unknown.
discovery_complete=true
kube api-resources --no-headers >"${work_dir}/api-resources.txt" || discovery_complete=false
awk '
  NF >= 4 && ($(NF - 1) == "true" || $(NF - 1) == "false") {
    group = ""
    version = $(NF - 2)
    slash = index(version, "/")
    if (slash > 0) {
      group = substr(version, 1, slash - 1)
      version = substr(version, slash + 1)
    }
    printf "D\t%s\t%s\t%s\t%s\t%s\n", group, tolower($NF), $1, version, $(NF - 1)
  }
' "${work_dir}/api-resources.txt" >"${work_dir}/served.tsv"
if [[ ! -s "${work_dir}/served.tsv" ]]; then
  unknown 'the kinds this cluster serves could not be read, so nothing was checked'
fi

# 3. Resolve every record to the list that can confirm it:
#      C <resource.version.group> <namespaced> <kind label> <record ns> <record name> <object ns> <object name>
#    or to the reason it cannot be checked:
#      U <reference|kind> <kind label> <record ns> <record name> <object ns> <object name>
awk -v complete="${discovery_complete}" '
  BEGIN { FS = OFS = "\t" }
  # Splits a wlid into object_ns, kind and object_name; returns 0 when it is not one.
  function parse(wlid,   prefix, rest, at) {
    prefix = "wlid://cluster-"
    if (substr(wlid, 1, length(prefix)) != prefix) return 0
    rest = substr(wlid, length(prefix) + 1)
    at = index(rest, "/")
    if (at == 0 || substr(rest, at, 11) != "/namespace-") return 0
    rest = substr(rest, at + 11)
    at = index(rest, "/")
    if (at == 0) return 0
    object_ns = substr(rest, 1, at - 1)
    rest = substr(rest, at + 1)
    at = index(rest, "-")
    if (at < 2 || at == length(rest) || index(rest, "/") > 0) return 0
    kind = substr(rest, 1, at - 1)
    object_name = substr(rest, at + 1)
    return 1
  }
  $1 == "D" {
    key = $2 SUBSEP $3
    served[key]++
    resource[key] = $4
    version[key] = $5
    namespaced[key] = $6
    if (served[key] == 1) {
      groups[$3]++
      only_group[$3] = $2
    }
    next
  }
  $1 == "R" {
    label = ($4 == "" ? "(no kind label)" : ($5 == "" ? $4 : $4 "." $5))
    if (!parse($6)) {
      print "U", "reference", label, $2, $3, "", ""
      next
    }
    if (tolower($4) == kind) {
      group = $5
    } else if (complete == "true" && groups[kind] == 1) {
      # "The only group serving this kind" is a fact about the whole cluster. After a partial
      # discovery the kind may also be served by a group that was not reached, and an empty list
      # from the one that was would report the record orphaned without its real kind being read.
      group = only_group[kind]
    } else {
      print "U", "kind", label, $2, $3, object_ns, object_name
      next
    }
    key = group SUBSEP kind
    if (served[key] != 1) {
      print "U", "kind", label, $2, $3, object_ns, object_name
      next
    }
    if (namespaced[key] == "true" && object_ns == "") {
      print "U", "reference", label, $2, $3, object_ns, object_name
      next
    }
    print "C", resource[key] "." version[key] "." group, namespaced[key], label, $2, $3, object_ns, object_name
  }
' "${work_dir}/served.tsv" "${work_dir}/records.tsv" >"${work_dir}/resolved.tsv"

# 4. One table read per kind: `S <target>` when it was read, then `L <target> <namespace> <name>` per
#    object. A failed read, or a listing that is not a name table, leaves no `S` line, so its records
#    end as unknown rather than as orphaned. kubectl pads its columns with at least three blanks, so
#    the columns are split on two or more: a name holding a single blank stays whole instead of being
#    indexed by its first word, which would keep the record of a deleted object with that first word
#    as its name alive. The header says how many columns a row can have. A row with more holds a
#    cell with a run of blanks in it, which may be the name, and then nothing in the listing is
#    trusted; a row with fewer only has empty cells after the name, which kubectl prints as nothing.
#    kubectl marks a default class by printing ` (default)` after its name, and that is not part of
#    the name.
: >"${work_dir}/live.tsv"
awk -F '\t' '$1 == "C" { print $2 "\t" $3 }' "${work_dir}/resolved.tsv" | LC_ALL=C sort -u >"${work_dir}/targets.tsv"
while IFS=$'\t' read -r target namespaced; do
  if [[ "${namespaced}" == true ]]; then
    kube get "${target}" -A >"${work_dir}/listing.txt" || continue
  else
    kube get "${target}" >"${work_dir}/listing.txt" || continue
  fi
  if awk -v target="${target}" -v namespaced="${namespaced}" '
    BEGIN { FS = "  +" }
    { sub(/ +$/, "") }
    NR == 1 {
      columns = NF
      if (namespaced == "true" ? ($1 != "NAMESPACE" || $2 != "NAME") : $1 != "NAME") exit 1
      next
    }
    NF > columns { exit 1 }
    {
      name = (namespaced == "true" ? $2 : $1)
      sub(/ \(default\)$/, "", name)
      if ($1 == "" || name == "") exit 1
      if (namespaced == "true") printf "L\t%s\t%s\t%s\n", target, $1, name
      else printf "L\t%s\t\t%s\n", target, name
    }
  ' "${work_dir}/listing.txt" >"${work_dir}/rows.tsv"; then
    printf 'S\t%s\n' "${target}" >>"${work_dir}/live.tsv"
    cat "${work_dir}/rows.tsv" >>"${work_dir}/live.tsv"
  fi
done <"${work_dir}/targets.tsv"

# 5. Classify: `<class> <reason> <kind label> <record ns> <record name> <object ns> <object name>`.
awk '
  BEGIN { FS = OFS = "\t" }
  $1 == "S" { read[$2] = 1; next }
  $1 == "L" { exists[$2 SUBSEP $3 SUBSEP $4] = 1; next }
  $1 == "U" { print "unknown", $2, $3, $4, $5, $6, $7; next }
  $1 == "C" {
    if (!($2 in read)) {
      print "unknown", "list", $4, $5, $6, $7, $8
      next
    }
    object_ns = ($3 == "true" ? $7 : "")
    class = ((($2 SUBSEP object_ns SUBSEP $8) in exists) ? "live" : "orphaned")
    print class, "-", $4, $5, $6, $7, $8
  }
' "${work_dir}/live.tsv" "${work_dir}/resolved.tsv" >"${work_dir}/classified.tsv"

count() {
  awk -F '\t' -v class="$1" '$1 == class { n++ } END { print n + 0 }' "${work_dir}/classified.tsv"
}
total="$(awk 'END { print NR }' "${work_dir}/classified.tsv")"
live="$(count live)"
orphaned="$(count orphaned)"
unchecked="$(count unknown)"

# Every record must have ended in exactly one class; a record that fell out of the pipeline would
# otherwise be missing from the totals without anything saying so.
if [[ "${total}" -ne "$(awk 'END { print NR }' "${work_dir}/records.tsv")" ]] ||
  [[ "${total}" -ne $((live + orphaned + unchecked)) ]]; then
  unknown 'the classified records do not add up to the records read, so the result cannot be trusted'
fi

LC_ALL=C sort -t "$(printf '\t')" -k 3,3 "${work_dir}/classified.tsv" | awk '
  BEGIN {
    FS = "\t"
    why["list"] = "its live objects could not be listed"
    why["kind"] = "the cluster does not serve this kind, or not under one name"
    why["reference"] = "the record carries no usable object reference"
    printf "%-60s %8s %8s %8s %8s\n", "KIND", "RECORDS", "LIVE", "ORPHANED", "UNKNOWN"
  }
  function flush() {
    if (label == "") return
    printf "%-60s %8d %8d %8d %8d%s\n", label, records, n["live"], n["orphaned"], n["unknown"], (notes == "" ? "" : "  (" notes ")")
  }
  $3 != label {
    flush()
    label = $3
    records = n["live"] = n["orphaned"] = n["unknown"] = 0
    notes = ""
  }
  {
    records++
    n[$1]++
    if ($1 == "unknown" && index(notes, why[$2]) == 0) notes = notes (notes == "" ? "" : "; ") why[$2]
  }
  END { flush() }
' >"${work_dir}/table.txt"

report "Kubescape ${records} checked against the objects they describe"
while IFS= read -r line; do
  report "${line}"
done <"${work_dir}/table.txt"
report "Total: records=${total} live=${live} orphaned=${orphaned} unknown=${unchecked}"
if [[ "${discovery_complete}" != true ]]; then
  report 'The served kinds were read only in part, so a record of a kind missing from that read is unknown.'
fi

if [[ -n "${list}" ]]; then
  awk -F '\t' -v class="${list}" '
    function field(value) { return (value == "" ? "-" : value) }
    $1 == class { printf "%s\t%s\t%s\t%s\t%s\n", field($4), field($5), field($3), field($6), field($7) }
  ' "${work_dir}/classified.tsv"
fi

# "1 of 7 scan records describes", "2 of 7 scan records describe".
describes() {
  if [[ "$1" -eq 1 ]]; then
    printf 'describes'
  else
    printf 'describe'
  fi
}

if [[ "${orphaned}" -gt 0 ]]; then
  verdict="Verdict: ORPHANED — ${orphaned} of ${total} scan records $(describes "${orphaned}") an object that no longer exists"
  if [[ "${unchecked}" -gt 0 ]]; then
    verdict="${verdict}; ${unchecked} more could not be checked"
  fi
  report "${verdict}"
  finish 1
fi
if [[ "${unchecked}" -gt 0 ]]; then
  unknown "no orphan was found, but ${unchecked} of ${total} scan records could not be checked"
fi
report "Verdict: CLEAN — all ${total} scan records describe an object that exists"
finish 0
