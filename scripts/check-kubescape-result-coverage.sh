#!/usr/bin/env bash
#
# Compare the live Kubescape result objects against the set of objects that SHOULD have one,
# per surface, and fail on every missing or stale result (#4262, part of #3156).
#
# The expected sets are defined in docs/kubescape-result-coverage.md. In short:
#
#   posture        every Deployment, StatefulSet, DaemonSet and CronJob in a scanned namespace
#                  has a workloadconfigurationscan that carries control results.
#   vulnerability  every image a long-lived container is running (keyed by image digest, not by
#                  container count) has a vulnerabilitymanifestsummary scanned within
#                  --vuln-max-age-days.
#   runtime        every running ReplicaSet, StatefulSet revision and DaemonSet revision has BOTH
#                  an applicationprofile and a networkneighborhood, each `completed/complete`.
#                  A profile still learning inside its learning period is PENDING, not a failure.
#
# A scanned namespace is every live namespace not listed in
# scripts/kubescape-unscanned-namespaces.tsv, the same reviewed list the scan-scope guard ties to
# the operator's excludeNamespaces.
#
# READ-ONLY. It issues only `kubectl get`. Every result object is read through the aggregated
# storage API, which strips `.spec` on a LIST, so control results are read back by NAME.
#
# FAIL CLOSED. An empty or failed read of any input is UNKNOWN (exit 2), never a clean pass: an
# empty result collection and an empty expected set both compare "clean" against nothing.
#
# Usage:
#   check-kubescape-result-coverage.sh [--context <kube-context>] [--vuln-max-age-days N]
#   check-kubescape-result-coverage.sh --from-dir <dir> --now <epoch-seconds> [...]
#
# --from-dir evaluates previously captured JSON instead of reading a cluster (used by the tests);
# the directory holds the files named in INPUTS below, each a Kubernetes `List`.
#
# Output: one line per problem on stdout (MISSING / STALE / PARTIAL / EMPTY), PENDING and ORPHAN
# lines for information, then one COVERAGE line per surface.
#
# Exit codes:
#   0  every expected object has a current result
#   1  at least one result is missing or stale; each is named
#   2  cannot check: bad usage, a failed or empty read, or unparseable input

set -uo pipefail

die() {
  printf 'check-kubescape-result-coverage: %s\n' "$*" >&2
  exit 2
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" ||
  die "could not resolve the repository root"

context=''
from_dir=''
now=''
vuln_max_age_days=7
reviewed="${repo_root}/scripts/kubescape-unscanned-namespaces.tsv"

while [ "$#" -gt 0 ]; do
  case "$1" in
  --context) context="${2:?--context needs a value}" && shift 2 ;;
  --from-dir) from_dir="${2:?--from-dir needs a value}" && shift 2 ;;
  --now) now="${2:?--now needs a value}" && shift 2 ;;
  --vuln-max-age-days) vuln_max_age_days="${2:?--vuln-max-age-days needs a value}" && shift 2 ;;
  --reviewed-list) reviewed="${2:?--reviewed-list needs a value}" && shift 2 ;;
  *) die "unknown argument: $1" ;;
  esac
done

case "${vuln_max_age_days}" in '' | *[!0-9]*) die "--vuln-max-age-days must be a whole number" ;; esac
[ -z "${now}" ] && now="$(date +%s)"
case "${now}" in *[!0-9]*) die "--now must be epoch seconds" ;; esac
command -v jq >/dev/null 2>&1 || die "jq is required but not installed"
[ -f "${reviewed}" ] || die "reviewed unscanned-namespace list not found: ${reviewed}"

# namespaces.json      kubectl get namespaces
# workloads.json       kubectl get deployments,statefulsets,daemonsets,cronjobs -A
# pods.json            kubectl get pods -A
# posture.json         workloadconfigurationscans, each read back BY NAME (spec intact)
# posture-list.json    kubectl get workloadconfigurationscans -A  (names only; spec is stripped)
# vulnerability.json   kubectl get vulnerabilitymanifestsummaries -A
# profiles.json        kubectl get applicationprofiles -A
# neighborhoods.json   kubectl get networkneighborhoods -A
readonly INPUTS='namespaces workloads pods posture-list posture vulnerability profiles neighborhoods'

# Only workload kinds carry a posture result the expected set can ask for; the other ~2500 stored
# objects (Secrets, RBAC, Services, ...) are never read back, which keeps a live run to a few calls.
readonly POSTURE_KINDS='.items[] | select(.metadata.labels["kubescape.io/workload-kind"] | IN("Deployment", "StatefulSet", "DaemonSet", "CronJob"))'

work="$(mktemp -d)" || die "could not create a temporary directory"
trap 'rm -rf "${work}"' EXIT

kc() {
  if [ -n "${context}" ]; then kubectl --context "${context}" "$@"; else kubectl "$@"; fi
}

fetch() {
  local name="$1"
  shift
  kc get "$@" -o json >"${work}/${name}.json" 2>"${work}/${name}.err" ||
    die "reading ${name} failed: $(head -c 300 "${work}/${name}.err")"
}

if [ -n "${from_dir}" ]; then
  for name in ${INPUTS}; do
    [ -f "${from_dir}/${name}.json" ] || die "--from-dir is missing ${name}.json"
    cp "${from_dir}/${name}.json" "${work}/${name}.json" || die "could not copy ${name}.json"
  done
else
  command -v kubectl >/dev/null 2>&1 || die "kubectl is required but not installed"
  fetch namespaces namespaces
  fetch workloads deployments,statefulsets,daemonsets,cronjobs -A
  fetch pods pods -A
  fetch posture-list workloadconfigurationscans -A
  fetch vulnerability vulnerabilitymanifestsummaries -A
  fetch profiles applicationprofiles -A
  fetch neighborhoods networkneighborhoods -A

  # A LIST of this aggregated API returns `.spec.controls` as null on every object, which reads
  # exactly like "no control results". A GET naming several objects keeps the spec, so read the
  # posture objects of the workload kinds back by name, one namespace and at most 50 names per call.
  printf '{"items":[]}\n' >"${work}/posture.json"
  jq -r "${POSTURE_KINDS} | [.metadata.namespace, .metadata.name] | @tsv" "${work}/posture-list.json" |
    sort | awk -F'\t' '
      $1 != ns || n == 50 { if (line != "") print line; ns = $1; n = 0; line = $1 }
      { line = line "\t" $2; n++ }
      END { if (line != "") print line }' >"${work}/batches" ||
    die "could not group posture objects for reading by name"
  while IFS=$'\t' read -r -a batch; do
    [ "${#batch[@]}" -ge 2 ] || continue
    kc -n "${batch[0]}" get workloadconfigurationscans "${batch[@]:1}" -o json >"${work}/batch.json" \
      2>"${work}/batch.err" || die "reading posture objects in ${batch[0]} failed: $(head -c 300 "${work}/batch.err")"
    if ! jq -s '{items: (.[0].items + (.[1].items // [.[1]]))}' "${work}/posture.json" "${work}/batch.json" \
      >"${work}/posture.next" || ! mv "${work}/posture.next" "${work}/posture.json"; then
      die "could not merge posture objects read from ${batch[0]}"
    fi
  done <"${work}/batches"
fi

for name in ${INPUTS}; do
  jq -e '.items | type == "array"' "${work}/${name}.json" >/dev/null 2>&1 ||
    die "${name}.json is not a Kubernetes List"
done
for name in namespaces workloads pods posture-list vulnerability profiles neighborhoods; do
  [ "$(jq '.items | length' "${work}/${name}.json")" -gt 0 ] ||
    die "${name} read back empty; an empty read is UNKNOWN, never clean"
done
listed="$(jq "[${POSTURE_KINDS}] | length" "${work}/posture-list.json")"
read_back="$(jq '.items | length' "${work}/posture.json")"
[ "${listed}" = "${read_back}" ] ||
  die "posture objects: ${listed} listed but ${read_back} read back by name"

# The reviewed list: one `<namespace><TAB><reason>` row per unscanned namespace, `#` comments.
unscanned="$(awk -F'\t' '/^[[:space:]]*#/ || NF == 0 { next } { print $1 }' "${reviewed}" |
  jq -R . | jq -sc .)" || die "could not parse ${reviewed}"
[ "$(jq 'length' <<<"${unscanned}")" -gt 0 ] || die "${reviewed} lists no namespaces"

jq -n -r \
  --argjson unscanned "${unscanned}" \
  --argjson now "${now}" \
  --argjson vuln_max_age "$((vuln_max_age_days * 86400))" \
  --slurpfile namespaces "${work}/namespaces.json" \
  --slurpfile workloads "${work}/workloads.json" \
  --slurpfile pods "${work}/pods.json" \
  --slurpfile posture "${work}/posture.json" \
  --slurpfile vulnerability "${work}/vulnerability.json" \
  --slurpfile profiles "${work}/profiles.json" \
  --slurpfile neighborhoods "${work}/neighborhoods.json" \
  -f /dev/stdin >"${work}/report" <<'JQ' || die "evaluating the captured objects failed"
def scanned: [$namespaces[0].items[].metadata.name] - $unscanned;
def in_scope: .metadata.namespace as $ns | scanned | index($ns) != null;
def epoch: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601;
def digest: capture("(?<d>sha256:[0-9a-f]{64})").d // null;

# ---- posture --------------------------------------------------------------------------------
( [ $workloads[0].items[] | select(in_scope)
    | select(.kind | IN("Deployment", "StatefulSet", "DaemonSet", "CronJob"))
    | "\(.kind)/\(.metadata.namespace)/\(.metadata.name)" ] | unique ) as $want
| ( [ $posture[0].items[] | . as $o | .metadata.labels as $l
      | { key: "\($l["kubescape.io/workload-kind"])/\($l["kubescape.io/workload-namespace"])/\($l["kubescape.io/workload-name"])",
          controls: (($o.spec.controls // {}) | length) } ] ) as $have
| ( $have | map({ (.key): .controls }) | add // {} ) as $controls
| ( [ $want[] | select($controls[.] == null) | "MISSING posture \(.)" ] ) as $missing
| ( [ $want[] | select($controls[.] == 0) | "EMPTY posture \(.) no control results" ] ) as $empty
| ( [ $have[].key | select(test("^(Deployment|StatefulSet|DaemonSet|CronJob)/"))
      | select(. as $k | $want | index($k) == null) | "ORPHAN posture \(.)" ] | unique ) as $orphan
| ($missing + $empty)[], $orphan[],
  "COVERAGE posture expected=\($want | length) current=\($want | length - ($missing + $empty | length)) missing=\($missing | length) empty=\($empty | length) orphan=\($orphan | length)",

# ---- vulnerability --------------------------------------------------------------------------
( [ $pods[0].items[] | select(in_scope) | select(.status.phase == "Running")
    | select(any(.metadata.ownerReferences[]?; .kind == "Job") | not)
    | .status.containerStatuses[]? | { digest: (.imageID | digest), image: .image } ]
  | map(select(.digest != null)) | unique_by(.digest) ) as $images
| ( [ $vulnerability[0].items[] | .metadata.annotations as $a
      | { digest: (($a["kubescape.io/image-id"] // "") | digest),
          at: (($a["kubescape.io/timestamp"] // "0") | tonumber) } ]
  | map(select(.digest != null)) | group_by(.digest) | map({ (.[0].digest): (map(.at) | max) }) | add // {} ) as $scanned_at
| ( [ $images[] | select($scanned_at[.digest] == null) | "MISSING vulnerability \(.image) \(.digest)" ] ) as $missing
| ( [ $images[] | select($scanned_at[.digest] != null and ($now - $scanned_at[.digest]) > $vuln_max_age)
      | "STALE vulnerability \(.image) \(.digest) scanned \($scanned_at[.digest] | todate)" ] ) as $stale
| ($missing + $stale)[],
  "COVERAGE vulnerability expected=\($images | length) current=\($images | length - ($missing + $stale | length)) missing=\($missing | length) stale=\($stale | length)",

# ---- runtime --------------------------------------------------------------------------------
( [ $pods[0].items[] | select(in_scope) | select(.status.phase == "Running")
    | . as $p | (.metadata.ownerReferences // [] | map(select(.controller == true)) | first) as $o
    | select($o != null)
    | if $o.kind == "ReplicaSet" then "replicaset-\($o.name)"
      elif $o.kind == "StatefulSet" then "statefulset-\($p.metadata.labels["controller-revision-hash"] // "")"
      elif $o.kind == "DaemonSet" then "daemonset-\($o.name)-\($p.metadata.labels["controller-revision-hash"] // "")"
      else empty end
    | select(test("-$") | not)
    | { key: "\($p.metadata.namespace)/\(.)", since: ($p.status.startTime // $p.metadata.creationTimestamp | epoch) } ]
  | group_by(.key) | map({ key: .[0].key, since: (map(.since) | min) }) ) as $want
| ( def index_of($list): [ $list[0].items[] | { key: "\(.metadata.namespace)/\(.metadata.name)",
        value: { status: (.metadata.annotations["kubescape.io/status"] // ""),
                 completion: (.metadata.annotations["kubescape.io/completion"] // ""),
                 period: (.metadata.labels["kubescape.io/learning-period"] // "24h") } } ] | from_entries;
    { ap: index_of($profiles), nn: index_of($neighborhoods) } ) as $idx
| def seconds: capture("^(?<n>[0-9]+)(?<u>[smh])$") as $c
    | ($c.n | tonumber) * ({ s: 1, m: 60, h: 3600 }[$c.u]);
  ( [ $want[] | . as $w | [ "ap", "nn" ][] as $kind | $idx[$kind][$w.key] as $r
      | { key: $w.key, since: $w.since,
          kind: ({ ap: "applicationprofile", nn: "networkneighborhood" }[$kind]), r: $r } ] ) as $checks
| ( [ $checks[] | select(.r == null) | "MISSING runtime \(.key) \(.kind)" ] ) as $missing
| ( [ $checks[] | select(.r != null and .r.status == "completed" and .r.completion != "complete")
      | "PARTIAL runtime \(.key) \(.kind) completion=\(.r.completion)" ] ) as $partial
| ( [ $checks[] | select(.r != null and .r.status != "completed")
      | select(($now - .since) > ((.r.period | seconds) // 86400))
      | "STALE runtime \(.key) \(.kind) status=\(.r.status) past its \(.r.period) learning period" ] ) as $stale
| ( [ $checks[] | select(.r != null and .r.status != "completed")
      | select(($now - .since) <= ((.r.period | seconds) // 86400))
      | "PENDING runtime \(.key) \(.kind) status=\(.r.status) still learning" ] ) as $pending
| ( [ $missing[], $partial[], $stale[] | capture("^[A-Z]+ runtime (?<k>[^ ]+)").k ] | unique ) as $failed
| ($missing + $partial + $stale)[], $pending[],
  "COVERAGE runtime expected=\($want | length) current=\($want | length - ($failed | length) - ([ $pending[] | capture("^PENDING runtime (?<k>[^ ]+)").k ] - $failed | unique | length)) failing=\($failed | length) missing=\($missing | length) partial=\($partial | length) stale=\($stale | length) pending=\($pending | length)"
JQ

cat "${work}/report"
for surface in posture vulnerability runtime; do
  line="$(grep "^COVERAGE ${surface} " "${work}/report")" || die "no ${surface} result was computed"
  case "${line}" in *' expected=0 '*) die "${surface}: the expected set is empty; that is UNKNOWN, never clean" ;; esac
done
if grep -qE '^(MISSING|STALE|PARTIAL|EMPTY) ' "${work}/report"; then
  exit 1
fi
exit 0
