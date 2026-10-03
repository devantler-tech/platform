#!/usr/bin/env bash
# Pin the behaviour of scripts/report-kubescape-scan-orphans.sh (#3697).
#
# WHY THIS EXISTS. The report decides whether a Kubescape scan record still describes something, and
# both of its mistakes are silent. Calling a live object's record orphaned invites someone to discount
# a real finding; calling an unread kind clean hides exactly the stale records the report exists to
# find. So each verdict is pinned against a fixture that differs from the clean one in one change,
# every failed or partial read is shown to end as UNKNOWN rather than as a pass, and the script's
# read-only verbs, its metadata-only reads and its public-log hygiene are asserted.
#
# kubectl is faked from fixtures; no cluster, no secrets, no network. Bash 3.2 compatible.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/report-kubescape-scan-orphans.sh"

work_dir="$(mktemp -d)"
readonly work_dir
# A test that exits 0 without having run every case is worse than one that errors. Bash 3.2 can hand
# an EXIT trap a zero status for an aborted script, so completion is recorded explicitly.
finished=''
# Invoked by the EXIT trap below. CI's shellcheck reports that as SC2317, newer releases as SC2329.
# shellcheck disable=SC2317,SC2329
cleanup() {
  local status=$?
  rm -rf "${work_dir}"
  if [[ -z "${finished}" && "${status}" -eq 0 ]]; then
    printf 'test-report-kubescape-scan-orphans: aborted before finishing; reporting failure rather than a clean pass\n' >&2
    status=1
  fi
  exit "${status}"
}
trap cleanup EXIT

readonly fake_bin="${work_dir}/bin"
mkdir -p "${fake_bin}"

output=''
stdout=''
rc=''
case_name=''
# Prepended to PATH for one case, to make a tool the script depends on fail.
path_prefix=''

fail() {
  printf 'FAIL %s: %s\n--- actual output (rc=%s) ---\n%s\n---\n' "${case_name}" "$1" "${rc:-?}" "${output:-}" >&2
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

# Fake kubectl. Every call must carry the context and the bounded request timeout. Only three shapes
# are served, and anything else leaves a marker the assertions look for:
#   api-resources --no-headers                    the served kinds
#   get <records>.<group> -A -o json              the scan records, the only JSON read
#   get <resource> [-A] --no-headers              a live list, as a table and never as objects
# A failed read writes an identity and an address to stderr, the way a real refusal does, so the
# log assertions cover the error path too.
cat >"${fake_bin}/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${FIXTURES}/calls.log"
[[ "$#" -ge 3 && "$1 $2 $3" == "--context scoped@test --request-timeout=60s" ]] || {
  touch "${FIXTURES}/UNBOUNDED_CALL"
  exit 1
}
shift 3
refuse() {
  printf 'Error from server (Forbidden): User "someone@example.test" cannot list at https://10.0.0.1:6443\n' >&2
  exit 1
}
if [[ "$*" == "api-resources --no-headers" ]]; then
  [[ -f "${FIXTURES}/api-resources.txt" ]] || refuse
  cat "${FIXTURES}/api-resources.txt"
  if [[ -f "${FIXTURES}/api-resources.partial" ]]; then
    printf 'error: unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1\n' >&2
    exit 1
  fi
  exit 0
fi
[[ "${1:-}" == "get" ]] || { touch "${FIXTURES}/FORBIDDEN_VERB"; exit 1; }
if [[ "$#" -eq 5 && "$2" == *.spdx.softwarecomposition.kubescape.io && "$3 $4 $5" == "-A -o json" ]]; then
  [[ -f "${FIXTURES}/records/$2.json" ]] || refuse
  cat "${FIXTURES}/records/$2.json"
  exit 0
fi
if [[ "$#" -eq 3 && "$3" == "--no-headers" ]] || [[ "$#" -eq 4 && "$3 $4" == "-A --no-headers" ]]; then
  scope=cluster
  [[ "$#" -eq 4 ]] && scope=namespaced
  [[ -f "${FIXTURES}/live/$2.${scope}" ]] || refuse
  cat "${FIXTURES}/live/$2.${scope}"
  exit 0
fi
touch "${FIXTURES}/UNEXPECTED_READ"
exit 1
FAKE
chmod +x "${fake_bin}/kubectl"

# record <file> <record-namespace> <record-name> <label-kind> <label-group> <wlid|->
# Appends one scan record. Labels carry the kind and group; the wlid annotation carries the object
# reference, and `-` leaves the annotation out.
record() {
  jq -cn --arg ns "$2" --arg name "$3" --arg kind "$4" --arg group "$5" --arg wlid "$6" '
    {metadata: {namespace: $ns, name: $name,
      labels: {"kubescape.io/workload-kind": $kind, "kubescape.io/workload-api-group": $group,
               "kubescape.io/workload-name": "label-name-is-not-the-reference"},
      annotations: (if $wlid == "-" then {} else {"kubescape.io/wlid": $wlid} end)},
     spec: {controls: null}}' >>"$1"
}

readonly scans='workloadconfigurationscans.spdx.softwarecomposition.kubescape.io'
readonly summaries='workloadconfigurationscansummaries.spdx.softwarecomposition.kubescape.io'

# The clean fixture. Every record describes an object that exists:
#   two Deployments in one namespace                 (plain namespaced kind)
#   a ClusterRoleBinding whose name has a colon      (the label cannot hold it; the wlid does)
#   an RBAC subject granted by that binding          (label kind ServiceAccount, wlid names the binding)
#   a Secret                                         (read as a table, never as an object)
#   a KubeletInfo recorded at a version no longer served, whose kind still is
#   a core Node, while longhorn.io also serves a Node kind with other names
fixture() {
  local dir="${work_dir}/case-$1" items
  rm -rf "${dir}"
  mkdir -p "${dir}/records" "${dir}/live"
  cat >"${dir}/api-resources.txt" <<'EOF'
nodes                             no           v1                                     false        Node
secrets                                        v1                                     true         Secret
serviceaccounts                   sa           v1                                     true         ServiceAccount
deployments                       deploy       apps/v1                                true         Deployment
clusterrolebindings                            rbac.authorization.k8s.io/v1           false        ClusterRoleBinding
kubeletinfos                                   hostdata.kubescape.cloud/v1beta1       false        KubeletInfo
nodes                             lhn          longhorn.io/v1beta2                    true         Node
EOF
  items="${dir}/items.jsonl"
  : >"${items}"
  record "${items}" shop deployment-api Deployment apps 'wlid://cluster-test/namespace-shop/deployment-api'
  record "${items}" shop deployment-worker Deployment apps 'wlid://cluster-test/namespace-shop/deployment-worker'
  record "${items}" kubescape clusterrolebinding-system-auth-delegator ClusterRoleBinding rbac.authorization.k8s.io \
    'wlid://cluster-test/namespace-/clusterrolebinding-system:auth-delegator'
  record "${items}" kubescape clusterrolebinding-system-auth-delegator-serviceaccount-api ServiceAccount '' \
    'wlid://cluster-test/namespace-/clusterrolebinding-system:auth-delegator'
  record "${items}" shop secret-api-credentials Secret '' 'wlid://cluster-test/namespace-shop/secret-api-credentials'
  record "${items}" kubescape kubeletinfo-worker-1 KubeletInfo hostdata.kubescape.cloud \
    'wlid://cluster-test/namespace-/kubeletinfo-worker-1'
  record "${items}" kubescape node-worker-1 Node '' 'wlid://cluster-test/namespace-/node-worker-1'
  jq -s '{items: .}' "${items}" >"${dir}/records/${scans}.json"
  cat >"${dir}/live/deployments.v1.apps.namespaced" <<'EOF'
shop      api      2/2   2     2     40d
shop      worker   1/1   1     1     40d
other     api      1/1   1     1     40d
EOF
  printf 'system:auth-delegator   ClusterRole/system:auth-delegator   90d\n' \
    >"${dir}/live/clusterrolebindings.v1.rbac.authorization.k8s.io.cluster"
  printf 'shop   api-credentials   Opaque   2   40d\n' >"${dir}/live/secrets.v1..namespaced"
  printf 'worker-1   40d\n' >"${dir}/live/kubeletinfos.v1beta1.hostdata.kubescape.cloud.cluster"
  printf 'worker-1   Ready   <none>   40d   v1.35.0\n' >"${dir}/live/nodes.v1..cluster"
  printf '%s\n' "${dir}"
}

# run <fixture-dir> [script arguments...]: stdout and stderr together in ${output}, stdout alone in
# ${stdout}, and the exit code in ${rc}.
run() {
  local dir="$1"
  shift
  : >"${dir}/calls.log"
  set +e
  FIXTURES="${dir}" PATH="${path_prefix}${fake_bin}:${PATH}" bash "${script}" "$@" >"${dir}/stdout" 2>"${dir}/stderr"
  rc=$?
  set -e
  stdout="$(cat "${dir}/stdout")"
  output="${stdout}
$(cat "${dir}/stderr")"
}

# Nothing but bounded reads, live objects only ever as tables, and no object-level detail in the
# report: the fixture's namespaces, object names, and the identity and address of a refused read.
assert_safe() {
  local dir="$1" marker live_reads
  for marker in UNBOUNDED_CALL FORBIDDEN_VERB UNEXPECTED_READ; do
    [[ ! -e "${dir}/${marker}" ]] || fail "kubectl was called outside the read-only contract (${marker})"
  done
  [[ -s "${dir}/calls.log" ]] || fail 'no kubectl call was recorded, so the read-only contract was not exercised'
  live_reads="$(grep -v -F -e "${scans}" -e "${summaries}" "${dir}/calls.log" || true)"
  if grep -Eq -- ' -o |--output' <<<"${live_reads}"; then
    fail 'a live object was requested as a full object rather than as a table'
  fi
}

assert_no_object_detail() {
  local needle
  for needle in shop api-credentials auth-delegator worker-1 someone@example.test 10.0.0.1; do
    refute_text "${needle}" "the report prints object-level detail (${needle})"
  done
}

# 1. Clean: every record describes an object that exists.
case_name='clean'
dir="$(fixture clean)"
run "${dir}" --context scoped@test
require_rc 0 'a scan set whose objects all exist must pass'
require_text 'Verdict: CLEAN — all 7 scan records describe an object that exists' 'the clean verdict must count every record'
require_text 'Total: records=7 live=7 orphaned=0 unknown=0' 'the totals must add up'
assert_safe "${dir}"
assert_no_object_detail
[[ "$(grep -c -- ' get ' "${dir}/calls.log")" -eq 6 ]] || fail 'each kind must be listed once, plus the records'

# 2. One deleted object, one change from clean: its record is orphaned and its sibling is not.
case_name='orphaned-deployment'
dir="$(fixture orphaned)"
printf 'shop      api      2/2   2     2     40d\n' >"${dir}/live/deployments.v1.apps.namespaced"
run "${dir}" --context scoped@test
require_rc 1 'a record whose object is gone must fail'
require_text 'Verdict: ORPHANED — 1 of 7 scan records describes an object that no longer exists' 'the orphan must be counted'
require_text 'Total: records=7 live=6 orphaned=1 unknown=0' 'only the deleted object may be orphaned'
assert_safe "${dir}"
assert_no_object_detail
run "${dir}" --context scoped@test --list orphaned
require_rc 1 'listing does not change the verdict'
[[ "${stdout}" == $'shop\tdeployment-worker\tDeployment.apps\tshop\tworker' ]] ||
  fail 'the orphan list must name exactly the record whose object is gone'
run "${dir}" --context scoped@test --list live
[[ "$(grep -c . <<<"${stdout}")" -eq 6 ]] || fail 'the live list must hold every other record'
if grep -Fq 'deployment-worker' <<<"${stdout}"; then
  fail 'an orphaned record must not appear in the live list'
fi
grep -Fq $'shop\tdeployment-api\tDeployment.apps\tshop\tapi' <<<"${stdout}" ||
  fail 'the record of the Deployment that still exists must stay live'

# 3. The same name in another namespace does not keep a record alive.
case_name='other-namespace'
dir="$(fixture other-namespace)"
printf 'other     api      1/1   1     1     40d\nshop      worker   1/1   1     1     40d\n' \
  >"${dir}/live/deployments.v1.apps.namespaced"
run "${dir}" --context scoped@test --list orphaned
require_rc 1 'an object of the same name elsewhere is a different object'
[[ "${stdout}" == $'shop\tdeployment-api\tDeployment.apps\tshop\tapi' ]] ||
  fail 'the namespace must be part of the object reference'

# 4. A deleted binding orphans both its own record and the subject record it granted.
case_name='orphaned-binding'
dir="$(fixture orphaned-binding)"
: >"${dir}/live/clusterrolebindings.v1.rbac.authorization.k8s.io.cluster"
run "${dir}" --context scoped@test
require_rc 1 'records of a deleted binding must fail'
require_text 'Total: records=7 live=5 orphaned=2 unknown=0' 'the binding and its subject record are both orphaned'
require_text 'Verdict: ORPHANED — 2 of 7 scan records describe an object that no longer exists' 'both must be counted'
run "${dir}" --context scoped@test --list orphaned
expected=$'kubescape\tclusterrolebinding-system-auth-delegator\tClusterRoleBinding.rbac.authorization.k8s.io\t-\tsystem:auth-delegator
kubescape\tclusterrolebinding-system-auth-delegator-serviceaccount-api\tServiceAccount\t-\tsystem:auth-delegator'
[[ "${stdout}" == "${expected}" ]] ||
  fail 'a cluster-scoped object must be listed with "-" for its namespace and its exact name'

# 5. A refused list: those records are unknown, never clean and never orphaned.
case_name='refused-secrets'
dir="$(fixture refused)"
rm "${dir}/live/secrets.v1..namespaced"
run "${dir}" --context scoped@test
require_rc 2 'a kind that could not be listed must not pass'
require_text 'Verdict: UNKNOWN — no orphan was found, but 1 of 7 scan records could not be checked' 'the unread record must be counted'
require_text 'Total: records=7 live=6 orphaned=0 unknown=1' 'a refused list is unknown, not orphaned'
require_text 'its live objects could not be listed' 'the report must say why the record is unknown'
assert_safe "${dir}"
assert_no_object_detail
run "${dir}" --context scoped@test --list unknown
[[ "${stdout}" == $'shop\tsecret-api-credentials\tSecret\tshop\tapi-credentials' ]] ||
  fail 'the unknown list must name the record that could not be checked'

# 6. An orphan outranks an unknown, and the unknown is still reported.
case_name='orphan-and-refused'
dir="$(fixture orphan-and-refused)"
rm "${dir}/live/secrets.v1..namespaced"
printf 'shop      api      2/2   2     2     40d\n' >"${dir}/live/deployments.v1.apps.namespaced"
run "${dir}" --context scoped@test
require_rc 1 'a confirmed orphan outranks an unread kind'
require_text 'Verdict: ORPHANED — 1 of 7 scan records describes an object that no longer exists; 1 more could not be checked' \
  'the verdict must not hide the unread record'

# 7. The records themselves cannot be read: nothing was checked.
case_name='records-unreadable'
dir="$(fixture records-unreadable)"
rm "${dir}/records/${scans}.json"
run "${dir}" --context scoped@test
require_rc 2 'an unreadable scan set must not pass'
require_text 'Verdict: UNKNOWN — the scan records could not be read, so nothing was checked' 'the verdict must say nothing was checked'
assert_no_object_detail

# 8. A reply that is not a record list, and an empty one, prove nothing either.
case_name='records-malformed'
dir="$(fixture records-malformed)"
printf '{"kind":"Status","status":"Failure"}\n' >"${dir}/records/${scans}.json"
run "${dir}" --context scoped@test
require_rc 2 'a reply without a record list must not pass'
require_text 'Verdict: UNKNOWN — the scan records could not be read, so nothing was checked' 'a malformed reply is an unreadable one'
case_name='records-empty'
dir="$(fixture records-empty)"
printf '{"items":[]}\n' >"${dir}/records/${scans}.json"
run "${dir}" --context scoped@test
require_rc 2 'an empty scan set must not pass as clean'
require_text 'Verdict: UNKNOWN — no scan record was returned, so nothing was checked' 'zero records examined is not a pass'

# 9. The served kinds cannot be read at all.
case_name='discovery-unreadable'
dir="$(fixture discovery-unreadable)"
rm "${dir}/api-resources.txt"
run "${dir}" --context scoped@test
require_rc 2 'without the served kinds nothing can be resolved'
require_text 'Verdict: UNKNOWN — the kinds this cluster serves could not be read, so nothing was checked' 'the verdict must say nothing was checked'

# 10. A partial discovery: a kind missing from it is unknown, and the kinds it did return are still checked.
case_name='discovery-partial'
dir="$(fixture discovery-partial)"
grep -v 'KubeletInfo' "${dir}/api-resources.txt" >"${dir}/api-resources.new"
mv "${dir}/api-resources.new" "${dir}/api-resources.txt"
touch "${dir}/api-resources.partial"
printf 'shop      api      2/2   2     2     40d\n' >"${dir}/live/deployments.v1.apps.namespaced"
run "${dir}" --context scoped@test
require_rc 1 'kinds a partial discovery did return are still checked'
require_text 'Total: records=7 live=5 orphaned=1 unknown=1' 'the missing kind is unknown and the deleted object orphaned'
require_text 'the cluster does not serve this kind, or not under one name' 'the report must say why the record is unknown'
require_text 'The served kinds were read only in part' 'a partial discovery must be stated'

# 11. A kind the cluster does not serve is unknown even when discovery is complete: no list of that
#     kind succeeded, so its absence was never observed.
case_name='kind-not-served'
dir="$(fixture kind-not-served)"
grep -v 'KubeletInfo' "${dir}/api-resources.txt" >"${dir}/api-resources.new"
mv "${dir}/api-resources.new" "${dir}/api-resources.txt"
run "${dir}" --context scoped@test
require_rc 2 'an unserved kind must not pass and must not be called orphaned'
require_text 'Total: records=7 live=6 orphaned=0 unknown=1' 'an unserved kind is unknown'

# 12. A record without an object reference is unknown.
case_name='no-reference'
dir="$(fixture no-reference)"
record "${dir}/items.jsonl" shop deployment-ghost Deployment apps -
record "${dir}/items.jsonl" shop deployment-odd Deployment apps 'not-a-wlid'
record "${dir}/items.jsonl" shop deployment-bare Deployment apps 'wlid://cluster-test/namespace-/deployment-bare'
jq -s '{items: .}' "${dir}/items.jsonl" >"${dir}/records/${scans}.json"
run "${dir}" --context scoped@test
require_rc 2 'a record that names no object must not pass'
require_text 'Total: records=10 live=7 orphaned=0 unknown=3' 'records without a usable reference are unknown'
require_text 'the record carries no usable object reference' 'the report must say why the record is unknown'
# A list row never has an empty field, so a shell `read` splitting on tabs cannot shift its columns.
run "${dir}" --context scoped@test --list unknown
[[ "$(grep -c . <<<"${stdout}")" -eq 3 ]] || fail 'every record without a reference must be listed'
if ! awk -F '\t' 'NF != 5 { exit 1 } { for (i = 1; i <= 5; i++) if ($i == "") exit 1 }' <<<"${stdout}"; then
  fail 'a list row must hold five non-empty fields'
fi

# 13. A list whose rows are not a table of names is a failed read, not an empty cluster.
case_name='malformed-table'
dir="$(fixture malformed-table)"
printf 'shop\n' >"${dir}/live/deployments.v1.apps.namespaced"
run "${dir}" --context scoped@test
require_rc 2 'a malformed list must not be read as "no objects"'
require_text 'Total: records=7 live=5 orphaned=0 unknown=2' 'records of a malformed list are unknown'

# 14. A kind served by two groups is resolved by the record's own group, and by neither when the
#     record does not say which.
case_name='ambiguous-kind'
dir="$(fixture ambiguous-kind)"
record "${dir}/items.jsonl" kubescape rolebinding-x-serviceaccount-y ServiceAccount '' \
  'wlid://cluster-test/namespace-storage/node-worker-1'
jq -s '{items: .}' "${dir}/items.jsonl" >"${dir}/records/${scans}.json"
run "${dir}" --context scoped@test
require_rc 2 'a reference that two served kinds could satisfy must not be guessed'
require_text 'Total: records=8 live=7 orphaned=0 unknown=1' 'the ambiguous record is unknown and the core Node record stays live'
require_text 'the cluster does not serve this kind, or not under one name' 'the report must say the kind was not resolved'
if grep -Fq 'nodes.v1beta2.longhorn.io' "${dir}/calls.log"; then
  fail 'no record may be checked against a group it does not name'
fi

# 15. Summaries are checked the same way when asked for.
case_name='summaries'
dir="$(fixture summaries)"
mv "${dir}/records/${scans}.json" "${dir}/records/${summaries}.json"
run "${dir}" --context scoped@test --resource summaries
require_rc 0 'the summary set is checked like the scan set'
require_text 'Verdict: CLEAN — all 7 scan records describe an object that exists' 'summaries must be counted'
grep -Fq -- "get ${summaries} -A -o json" "${dir}/calls.log" || fail 'the summary resource must be the one read'

# 16. Usage errors read nothing and never pass.
case_name='usage'
dir="$(fixture usage)"
run "${dir}"
require_rc 2 'a missing context must not pass'
run "${dir}" --context
require_rc 2 'a context flag without a value must not pass'
run "${dir}" --context scoped@test --resource pods
require_rc 2 'an unsupported resource must not pass'
run "${dir}" --context scoped@test --list everything
require_rc 2 'an unsupported list must not pass'
run "${dir}" --context scoped@test --delete
require_rc 2 'an unknown flag must not pass'
[[ ! -s "${dir}/calls.log" ]] || fail 'a usage error must not reach the cluster'
run "${dir}" --help
require_rc 0 'help must exit cleanly'
require_text 'Usage:' 'help must print the usage'

# 17. A run that stops part-way reached no verdict. Without a guard it would leave with the failed
#     tool's status — which reads as ORPHANED — or, on Bash 3.2, with zero.
case_name='aborted'
dir="$(fixture aborted)"
mkdir -p "${work_dir}/broken"
printf '#!/usr/bin/env bash\nexit 1\n' >"${work_dir}/broken/sort"
chmod +x "${work_dir}/broken/sort"
path_prefix="${work_dir}/broken:"
run "${dir}" --context scoped@test
path_prefix=''
require_rc 2 'a run that stopped before its verdict must be UNKNOWN'
require_text 'Verdict: UNKNOWN — the report stopped before reaching a verdict' 'an aborted run must say it reached no verdict'
refute_text 'Verdict: CLEAN' 'an aborted run must not print a clean verdict'

finished=true
printf 'PASS: the Kubescape scan-record orphan report holds its contract.\n'
