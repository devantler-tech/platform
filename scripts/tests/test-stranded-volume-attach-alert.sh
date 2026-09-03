#!/usr/bin/env bash
# Contract for the stranded-volume-attach-alert CronJob (#2754).
#
# The detector exists for one incident shape: a pod whose volume stayed attached
# to a departed node in the cloud provider, logging `FailedAttachVolume` for
# nine hours while nothing alerted (#2363). The scenarios below replay that shape
# against a stubbed kube API and a stubbed curl, so what is asserted is what the
# container would really do:
#
#   FIRES on the 2026-07-01 shape — an old, still-repeating attach failure on a
#   pod that is still not Ready — and names the pod, node, PVC, PV and the
#   verbatim CSI message (AC1, AC3: this is the RED proof the original spec
#   would have failed).
#
#   STAYS QUIET on every ordinary shape (AC2): a fresh failure that is merely
#   slow; an old failure that stopped repeating; an old, still-repeating failure
#   on a pod that has since gone Ready (the slow-but-successful autoscaler
#   scale-up); and a pod that no longer exists.
#
#   FAILS LOUDLY instead of reporting a silent zero when the events read returns
#   a non-200 code or a malformed body, and when the delivery itself fails.
#
#   NEVER WRITES: the ClusterRole grants only get/list on the four read
#   resources (AC4, AC5), and the script issues no write verb.
#
# The script is extracted from the rendered manifest rather than copied here, so
# this cannot drift into testing a stale transcription of it.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly manifest="${root_dir}/k8s/bases/infrastructure/controllers/coroot/cron-job-stranded-volume-attach-alert.yaml"
readonly cluster_role="${root_dir}/k8s/bases/infrastructure/controllers/coroot/cluster-role-stranded-volume-attach-alert.yaml"
# Not a hooks.slack.com-shaped literal: GitHub push protection matches that
# shape. `.invalid` is RFC 2606 reserved, so this can never resolve.
readonly REAL_WEBHOOK='https://hooks.test.invalid/delivery-target'
readonly PLACEHOLDER_WEBHOOK='https://example.invalid/no-slack-configured'
fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}
pass() {
  printf 'ok — %s\n' "$1"
}
for tool in yq jq; do
  command -v "${tool}" >/dev/null 2>&1 || {
    printf '::error::%s is required to run this contract test.\n' "${tool}" >&2
    exit 64
  }
done
[ -f "${manifest}" ] || fail "manifest not found: ${manifest} (run from the repository root)"
[ -f "${cluster_role}" ] || fail "cluster role not found: ${cluster_role}"

# ---------------------------------------------------------------------------
# Structural assertions.
# ---------------------------------------------------------------------------
container_path='.spec.jobTemplate.spec.template.spec.containers[0]'
pod_path='.spec.jobTemplate.spec.template.spec'
script_body="$(yq eval "${container_path}.command[2]" "${manifest}")"
[ -n "${script_body}" ] && [ "${script_body}" != "null" ] ||
  fail "could not extract the container script from ${manifest}"

# AC5: read-only RBAC on exactly the four resources, no write verb anywhere.
verbs="$(yq eval '[.rules[].verbs[]] | unique | join(",")' "${cluster_role}")"
[ "${verbs}" = "get,list" ] ||
  fail "ClusterRole verbs must be exactly get,list; found: ${verbs}"
resources="$(yq eval '[.rules[].resources[]] | sort | join(",")' "${cluster_role}")"
[ "${resources}" = "events,persistentvolumeclaims,persistentvolumes,pods" ] ||
  fail "ClusterRole resources must be exactly the four read resources; found: ${resources}"
pass "RBAC is get/list on events, pods, persistentvolumes, persistentvolumeclaims"

# AC4: the script issues no write. Every API call goes through api_get, and the
# only curl that is not a GET is the delivery. Assert no HTTP method override.
if grep -Eq -- '-X[[:space:]]*(POST|PUT|PATCH|DELETE)|--request' <<<"${script_body}"; then
  fail "the script carries an HTTP method override; it must only read the kube API"
fi
pass "the script issues no write verb against the kube API"

# CKV_K8S_35 and the argv relocation trap, as for cnpg-degraded-alert.
if grep -q 'secretKeyRef' <<<"$(yq eval "${container_path}.env" "${manifest}")"; then
  fail "CKV_K8S_35: container env still carries a secretKeyRef; the webhook must arrive as a mounted file"
fi
grep -q 'stranded-volume-attach-alert-webhook' <<<"$(yq eval "${pod_path}.volumes" "${manifest}")" ||
  fail "no volume sourced from the stranded-volume-attach-alert-webhook Secret"
default_mode="$(yq eval "${pod_path}.volumes[] | select(.name == \"webhook\") | .secret.defaultMode" "${manifest}")"
[ "${default_mode}" = "288" ] || fail "expected the webhook volume defaultMode 0440 (288 decimal), got: ${default_mode}"
fs_group="$(yq eval "${pod_path}.securityContext.fsGroup" "${manifest}")"
run_as_user="$(yq eval "${pod_path}.securityContext.runAsUser" "${manifest}")"
[ "${fs_group}" = "${run_as_user}" ] || fail "fsGroup (${fs_group}) must match runAsUser (${run_as_user})"
if grep -Eq 'curl.*"\$\{?WEBHOOK_URL\}?"' <<<"${script_body}"; then
  fail "the webhook URL is passed to curl as an argument; it would appear in /proc/<pid>/cmdline"
fi
grep -Fq -- '--config -' <<<"${script_body}" ||
  fail "the delivery does not read its URL from a stdin config"
pass "webhook is a 0440 mounted file readable by fsGroup ${fs_group}, never in argv"

# The Flux substitution opt-out that keeps `${STALL_SECONDS}` a container env var.
[ "$(yq eval '.metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"]' "${manifest}")" = "disabled" ] ||
  fail "the CronJob must opt out of Flux substitution, or its env-var references expand to empty"
pass "Flux substitution is disabled on the CronJob"

# ---------------------------------------------------------------------------
# Behavioural scenarios.
# ---------------------------------------------------------------------------
work_root="$(mktemp -d /tmp/tmp.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT

now_epoch="$(date -u +%s)"
ts() { # $1 seconds ago → RFC 3339
  local at=$((now_epoch - $1))
  if date -u -r "${at}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null; then :; else date -u -d "@${at}" +%Y-%m-%dT%H:%M:%SZ; fi
}

# Fixture builders. The 2026-07-01 shape, parameterised.
event_json() { # $1 reason, $2 first-ago, $3 last-ago, $4 count, $5 ns, $6 pod
  jq -cn --arg reason "$1" --arg first "$(ts "$2")" --arg last "$(ts "$3")" --argjson count "$4" \
        --arg ns "$5" --arg pod "$6" '
    { reason: $reason, count: $count, firstTimestamp: $first, lastTimestamp: $last,
      involvedObject: { kind: "Pod", namespace: $ns, name: $pod },
      source: { component: "attachdetach-controller", host: "prod-worker-2" },
      message: "AttachVolume.Attach failed for volume \"pvc-6d6ce286-7e96-45d6-b101-8a5a805e0993\" : rpc error: code = FailedPrecondition desc = failed to publish volume: volume is attached" }'
}
events_body() { # stdin: event objects, one per line → list body
  jq -sc '{kind: "EventList", items: .}'
}
pod_json() { # $1 ready (true|false)
  jq -cn --arg ready "$1" '
    { metadata: { name: "openbao-0", namespace: "openbao" },
      spec: { nodeName: "prod-worker-2", volumes: [ { name: "data", persistentVolumeClaim: { claimName: "data-openbao-0" } } ] },
      status: { phase: (if $ready == "true" then "Running" else "Pending" end),
                conditions: [ { type: "Ready", status: (if $ready == "true" then "True" else "False" end) } ] } }'
}
pv_json() {
  jq -cn '{ metadata: { name: "pvc-6d6ce286-7e96-45d6-b101-8a5a805e0993" }, spec: { claimRef: { namespace: "openbao", name: "data-openbao-0" } } }'
}

# Build one scenario sandbox: a fake serviceaccount dir, a webhook file, and a
# stubbed curl that serves canned API responses BY URL PATH and records deliveries.
setup_scenario() {
  local name="$1" webhook="$2"
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin" "${dir}/sa" "${dir}/webhook" "${dir}/tmp" "${dir}/api"
  printf 'fake-ca' >"${dir}/sa/ca.crt"
  printf 'fake-token' >"${dir}/sa/token"
  printf '%s' "${webhook}" >"${dir}/webhook/url"
  # Defaults: no events of either reason, HTTP 200.
  printf '%s' '{"kind":"EventList","items":[]}' >"${dir}/api/events-FailedAttachVolume.body"
  printf '%s' '{"kind":"EventList","items":[]}' >"${dir}/api/events-FailedMount.body"
  printf '200' >"${dir}/api/events-FailedAttachVolume.code"
  printf '200' >"${dir}/api/events-FailedMount.code"
  printf '200' >"${dir}/api/pod.code"
  pod_json false >"${dir}/api/pod.body"
  printf '200' >"${dir}/api/pv.code"
  pv_json >"${dir}/api/pv.body"
  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl. The kube API reads are recognised by the URL argument and served
# from the scenario's canned bodies; the delivery is the call with no -o.
set -uo pipefail
dir="${SCENARIO_DIR}"
printf '%s\n' "$*" >>"${dir}/curl-argv.log"
out_file=""
url=""
prev=""
for arg in "$@"; do
  [ "${prev}" = "-o" ] && out_file="${arg}"
  case "${arg}" in https://*) url="${arg}" ;; esac
  prev="${arg}"
done
if [ -n "${out_file}" ]; then
  key=""
  case "${url}" in
    *"/events?fieldSelector=reason%3DFailedAttachVolume") key="events-FailedAttachVolume" ;;
    *"/events?fieldSelector=reason%3DFailedMount") key="events-FailedMount" ;;
    */namespaces/*/pods/*) key="pod" ;;
    */persistentvolumes/*) key="pv" ;;
  esac
  [ -n "${key}" ] || { printf '%s\n' "unstubbed URL: ${url}" >>"${dir}/stub-errors.log"; printf '599'; exit 0; }
  printf '%s\n' "${url}" >>"${dir}/api-reads.log"
  cat "${dir}/api/${key}.body" >"${out_file}"
  cat "${dir}/api/${key}.code"
  exit 0
fi
cat >>"${dir}/curl-stdin.log"
for arg in "$@"; do
  case "${arg}" in
    @*) cp "${arg#@}" "${dir}/delivered-payload.json" 2>/dev/null || true ;;
  esac
done
printf 'delivered\n' >>"${dir}/deliveries.log"
exit "${STUB_DELIVERY_EXIT:-0}"
STUB
  chmod +x "${dir}/bin/curl"
  printf '%s' "${dir}"
}

# Run the extracted script inside a scenario sandbox, byte-identical to the
# container's except for the two in-cluster absolute paths, which are redirected.
run_scenario() {
  local dir="$1"
  local patched="${dir}/script.sh"
  {
    printf '%s\n' 'set -eu'
    printf 'STALL_SECONDS=%s\nRECENT_SECONDS=%s\n' 600 900
    printf 'export SCENARIO_DIR=%q\n' "${dir}"
    printf 'SA_OVERRIDE=%q\n' "${dir}/sa"
    # ORDER IS LOAD-BEARING: the `/tmp/` rewrite must run FIRST — see the same
    # note in test-cnpg-degraded-alert.sh.
    # shellcheck disable=SC2016  # `$SA_OVERRIDE` is emitted INTO the patched script.
    printf '%s\n' "${script_body}" |
      sed -e 's#^\( *\)SA=/var/run/secrets/kubernetes.io/serviceaccount$#\1SA="$SA_OVERRIDE"#' \
        -e 's#/tmp/#'"${dir}"'/tmp/#g' \
        -e 's#/etc/stranded-volume-attach-alert/url#'"${dir}"'/webhook/url#g'
  } >"${patched}"
  # shellcheck disable=SC2016  # matching the LITERAL `$SA_OVERRIDE` the sed wrote.
  grep -q 'SA="\$SA_OVERRIDE"' "${patched}" ||
    fail "serviceaccount path redirection did not apply; the script's SA line changed shape"
  grep -q "${dir}/webhook/url" "${patched}" ||
    fail "webhook path redirection did not apply"
  # The doubling shape specifically — a sandbox path nested INSIDE another — not
  # merely two sandbox paths on one line: the script legitimately names several
  # scratch files in one jq invocation, and each is rewritten independently.
  ! grep -q -- "${dir}/tmp/${dir#/}" "${patched}" ||
    fail "a redirection rewrote another rule's output; the sandbox path is doubled"
  (
    cd "${dir}"
    PATH="${dir}/bin:${PATH}" SCENARIO_DIR="${dir}" bash "${patched}" >"${dir}/stdout.log" 2>"${dir}/stderr.log"
  )
}

delivered() { [ -f "$1/deliveries.log" ]; }

# 1. The 2026-07-01 shape: first failure 30 minutes ago, still repeating a
#    minute ago, 26632 repeats, pod still Pending. MUST fire and name everything.
dir="$(setup_scenario incident "${REAL_WEBHOOK}")"
event_json FailedAttachVolume 1800 60 26632 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
run_scenario "${dir}" || fail "incident: the script exited non-zero: $(cat "${dir}/stderr.log")"
delivered "${dir}" || fail "incident: the 2026-07-01 shape produced NO alert — this is the RED the original spec would have failed"
payload="$(jq -r .text "${dir}/delivered-payload.json")"
for needle in 'openbao/openbao-0' 'prod-worker-2' 'openbao/data-openbao-0' 'pvc-6d6ce286-7e96-45d6-b101-8a5a805e0993' 'volume is attached' 'FailedAttachVolume ×26632'; do
  grep -qF -- "${needle}" <<<"${payload}" || fail "incident: the alert does not name '${needle}'; payload: ${payload}"
done
grep -q "^url = ${REAL_WEBHOOK}$" "${dir}/curl-stdin.log" || fail "incident: the webhook URL did not travel on curl's stdin config"
pass "the 2026-07-01 shape fires and names pod, node, PVC, PV and the CSI message"

# 2. Fresh failure: first occurrence 2 minutes ago. A slow attach that is still
#    inside the stall window must stay quiet.
dir="$(setup_scenario fresh "${REAL_WEBHOOK}")"
event_json FailedAttachVolume 120 30 4 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
run_scenario "${dir}" || fail "fresh: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "fresh: a 2-minute-old failure alerted; the stall window is not being honoured"
pass "a failure younger than the stall window stays quiet"

# 3. Historical failure: old first occurrence, but it stopped repeating 20
#    minutes ago. The attach succeeded; its history must not alert.
dir="$(setup_scenario historical "${REAL_WEBHOOK}")"
event_json FailedAttachVolume 3600 1200 40 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
run_scenario "${dir}" || fail "historical: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "historical: a failure that stopped repeating alerted; the recency window is not being honoured"
pass "a failure that stopped repeating stays quiet"

# 4. Slow-but-successful: old AND still-recent events, but the pod is Ready —
#    the autoscaler scale-up that took a while and then worked. Quiet.
dir="$(setup_scenario ready "${REAL_WEBHOOK}")"
event_json FailedMount 1800 60 12 openbao openbao-0 | events_body >"${dir}/api/events-FailedMount.body"
pod_json true >"${dir}/api/pod.body"
run_scenario "${dir}" || fail "ready: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "ready: a pod that has gone Ready alerted; the pod read is not being honoured"
grep -q '/namespaces/openbao/pods/openbao-0' "${dir}/api-reads.log" || fail "ready: the pod was never read, so the quiet verdict was not earned"
pass "a slow-but-successful attach on a pod that is now Ready stays quiet"

# 5. The pod is gone (404): nothing left to strand. Quiet, and not an error.
dir="$(setup_scenario gone "${REAL_WEBHOOK}")"
event_json FailedAttachVolume 1800 60 100 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
printf '404' >"${dir}/api/pod.code"
run_scenario "${dir}" || fail "gone: the script exited non-zero on a deleted pod: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "gone: a deleted pod alerted"
pass "a deleted pod stays quiet without failing the run"

# 6. Silent-zero guards: a non-200 events read and a malformed body each FAIL
#    the run rather than reporting "none stranded".
dir="$(setup_scenario api-error "${REAL_WEBHOOK}")"
printf '500' >"${dir}/api/events-FailedAttachVolume.code"
if run_scenario "${dir}"; then fail "api-error: an HTTP 500 events read exited 0 (silent zero)"; fi
grep -q 'HTTP 500' "${dir}/stderr.log" || fail "api-error: the failure does not name the HTTP code"
pass "a non-200 events read fails the run loudly"
dir="$(setup_scenario api-shape "${REAL_WEBHOOK}")"
printf '%s' '{"items":{}}' >"${dir}/api/events-FailedMount.body"
if run_scenario "${dir}"; then fail "api-shape: a malformed events body exited 0 (silent zero)"; fi
pass "a malformed events body fails the run loudly"

# 7. Delivery failure fails the run: nothing else would notice a dropped alert.
dir="$(setup_scenario delivery-fails "${REAL_WEBHOOK}")"
event_json FailedAttachVolume 1800 60 26632 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
if STUB_DELIVERY_EXIT=22 run_scenario "${dir}"; then fail "delivery-fails: a failed POST exited 0"; fi
pass "a failed delivery fails the run"

# 8. Placeholder webhook (local/CI): the finding is logged, delivery skipped, exit 0.
dir="$(setup_scenario placeholder "${PLACEHOLDER_WEBHOOK}")"
event_json FailedAttachVolume 1800 60 26632 openbao openbao-0 | events_body >"${dir}/api/events-FailedAttachVolume.body"
run_scenario "${dir}" || fail "placeholder: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "placeholder: an alert was delivered to the example.invalid placeholder"
grep -q 'not delivered' "${dir}/stdout.log" || fail "placeholder: the skip was not logged"
pass "the local/CI placeholder webhook skips delivery and exits 0"

for d in "${work_root}"/*/; do
  [ -f "${d}/stub-errors.log" ] && fail "unstubbed API read in $(basename "${d}"): $(cat "${d}/stub-errors.log")"
done
printf 'PASS: stranded-volume-attach-alert fires on the 2026-07-01 shape, stays quiet on every ordinary shape, and never reports a silent zero\n'
