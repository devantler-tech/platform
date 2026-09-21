#!/usr/bin/env bash
# Check the admission write against API fixtures, with kubectl as the external boundary.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch="$(mktemp -d)"
trap 'rm -rf "${scratch}"' EXIT
mkdir "${scratch}/bin"

# Six operator templates without the two fields (the live shape).
jq -n '
  def pod: {securityContext: {runAsNonRoot: true},
    containers: [{name: "main", securityContext: {capabilities: {drop: ["ALL"]}}}]};
  def t($kind; $name): {kind: $kind, metadata: {name: $name, namespace: "observability"},
    spec: {template: {spec: pod}}};
  {apiVersion: "v1", kind: "List", items: [
    t("Deployment"; "coroot-cluster-agent"), t("Deployment"; "coroot-prometheus"),
    t("StatefulSet"; "coroot-clickhouse-keeper"), t("StatefulSet"; "coroot-clickhouse-shard-0"),
    t("StatefulSet"; "coroot-coroot"), t("DaemonSet"; "coroot-node-agent")]}' >"${scratch}/unhardened.json"
jq '.items |= map(.spec.template.spec.securityContext.fsGroupChangePolicy = "OnRootMismatch" |
  .spec.template.spec.containers |= map(.securityContext.seLinuxOptions = {}))' \
  "${scratch}/unhardened.json" >"${scratch}/hardened.json"
# Only coroot-coroot still lacks the fields.
jq '.items[4].spec.template.spec.securityContext = {runAsNonRoot: true} |
  .items[4].spec.template.spec.containers[0].securityContext = {}' \
  "${scratch}/hardened.json" >"${scratch}/one-lacking.json"

# Each read returns the next queued fixture (read.N.json), falling back to read.json.
# Writes and rollouts are logged so the test can assert exactly what was touched.
cat >"${scratch}/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 $2" == '--context fixture' ]] || exit 99
shift 2
case "$1" in
  get)
    [[ "$*" == 'get deployments,statefulsets,daemonsets --namespace observability --selector app.kubernetes.io/managed-by=coroot-operator -o json --request-timeout=20s' ]] || exit 98
    [[ "${SCENARIO}" == api-error ]] && exit 1
    [[ "${SCENARIO}" == empty-list ]] && { printf '{"items":[]}'; exit 0; }
    count=$(( $(cat "${FIXTURE_DIR}/reads" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "${count}" >"${FIXTURE_DIR}/reads"
    if [[ -f "${FIXTURE_DIR}/read.${count}.json" ]]; then cat "${FIXTURE_DIR}/read.${count}.json"; else cat "${FIXTURE_DIR}/read.json"; fi
    exit 0 ;;
  annotate)
    [[ "$2 $3 $4" == '--namespace observability --overwrite' ]] || exit 97
    [[ "$6" == pod-security.devantler.tech/baseline-context-coroot-admitted=* ]] || exit 96
    printf '%s\n' "$5" >>"${FIXTURE_DIR}/writes"
    [[ "${SCENARIO}" == write-error ]] && exit 1
    exit 0 ;;
  rollout)
    [[ "$2 $3 $4" == 'status --namespace observability' ]] || exit 95
    printf '%s %s\n' "$5" "$6" >>"${FIXTURE_DIR}/rollouts"
    [[ "${SCENARIO}" == rollout-timeout ]] && exit 1
    exit 0 ;;
esac
exit 94
SH
printf '#!/usr/bin/env bash\nexit 0\n' >"${scratch}/bin/sleep"
chmod +x "${scratch}/bin/kubectl" "${scratch}/bin/sleep"
export PATH="${scratch}/bin:${PATH}" FIXTURE_DIR="${scratch}"
script="${root_dir}/scripts/admit-coroot-baseline-context.sh"

check() {
  local name="$1" expected="$2" scenario="$3" reason="${4:-}" rc=0
  rm -f "${scratch}/reads" "${scratch}/writes" "${scratch}/rollouts"
  SCENARIO="${scenario}" bash "${script}" --context fixture >"${scratch}/output" 2>&1 || rc=$?
  if [[ "${expected}" == pass && "${rc}" -ne 0 ]] || [[ "${expected}" == fail && "${rc}" -ne 1 ]]; then
    printf 'FAIL: %s (exit %s)\n' "${name}" "${rc}" >&2
    cat "${scratch}/output" >&2
    exit 1
  fi
  if [[ -n "${reason}" ]] && ! grep -qF "${reason}" "${scratch}/output"; then
    printf 'FAIL: %s failed for the wrong reason\n' "${name}" >&2
    cat "${scratch}/output" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${name}"
}
queue() { rm -f "${scratch}"/read.*.json; cp "${scratch}/$1.json" "${scratch}/read.json"; shift
  local n=1; for f in "$@"; do cp "${scratch}/${f}.json" "${scratch}/read.${n}.json"; n=$((n + 1)); done; }

queue hardened
check 'templates that already carry both fields are not written' pass normal 'No admission write needed'
[[ ! -f "${scratch}/writes" && ! -f "${scratch}/rollouts" ]] || { echo 'FAIL: a hardened template was written' >&2; exit 1; }

queue hardened unhardened hardened
check 'every template lacking a field is written and rolled out' pass normal
[[ "$(wc -l <"${scratch}/writes")" -eq 6 ]] || { echo 'FAIL: expected six writes' >&2; exit 1; }
grep -qx 'daemonset/coroot-node-agent' "${scratch}/writes"
grep -qx 'statefulset/coroot-clickhouse-shard-0 --timeout=15m' "${scratch}/rollouts"
[[ "$(wc -l <"${scratch}/rollouts")" -eq 6 ]] || { echo 'FAIL: expected six rollouts' >&2; exit 1; }

queue hardened one-lacking hardened
check 'only the template lacking a field is written' pass normal
[[ "$(cat "${scratch}/writes")" == 'statefulset/coroot-coroot' ]] || { echo 'FAIL: wrong write set' >&2; exit 1; }
[[ "$(cat "${scratch}/rollouts")" == 'statefulset/coroot-coroot --timeout=15m' ]] || { echo 'FAIL: wrong rollout set' >&2; exit 1; }

queue hardened unhardened unhardened hardened
check 'a write admitted before the label is visible is retried' pass normal
[[ "$(wc -l <"${scratch}/writes")" -eq 12 ]] || { echo 'FAIL: expected two write rounds' >&2; exit 1; }
[[ "$(wc -l <"${scratch}/rollouts")" -eq 6 ]] || { echo 'FAIL: expected six rollouts' >&2; exit 1; }

queue unhardened
check 'fields still missing after three writes fail before any rollout' fail normal 'admission did not add both fields'
[[ "$(wc -l <"${scratch}/writes")" -eq 18 ]] || { echo 'FAIL: expected three bounded write rounds' >&2; exit 1; }
[[ ! -f "${scratch}/rollouts" ]] || { echo 'FAIL: rolled out without admitted fields' >&2; exit 1; }

queue hardened unhardened hardened
check 'an unfinished rollout fails' fail rollout-timeout 'did not finish within 15m'
check 'a failed write fails' fail write-error 'admission write failed'
check 'API error is not absence' fail api-error 'API read failed'
check 'an empty population is not absence' fail empty-list 'no Coroot templates'
