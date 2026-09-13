#!/usr/bin/env bash
# Contract for the prune-protected-orphan-alert CronJob (#3503).
#
# The detector exists for one failure shape: a retirement whose manual delete
# step was missed, so a prune-protected HelmRelease kept running — unmanaged and
# unpatchable — for 74 days while nothing noticed (#3480). The scenarios below
# replay that shape against a stubbed kube API, state record and delivery, so
# what is asserted is what the container would really do:
#
#   WARNS on a freshly left-behind resource and records when it was first seen
#   (the RED proof: before this CronJob nothing reported it at all).
#
#   ESCALATES once a left-behind resource is older than the bound, and when the
#   Kustomization that applied it no longer exists — naming kind, object, owner
#   and age — without resetting the recorded first sighting.
#
#   STAYS QUIET on every managed shape: everything still in its inventory, an
#   unprotected object Flux will prune itself, and an object already being
#   deleted. A resolved finding is forgotten.
#
#   FAILS LOUDLY instead of reporting a silent zero on a non-200 or malformed
#   list, a Kustomization with no inventory to compare against, a missing or
#   malformed state record, a failed state write and a failed delivery.
#
#   WRITES NOTHING BUT ITS OWN STATE: the ClusterRole is get/list on the four
#   read resources, the Role is get/patch on the one state ConfigMap by name, and
#   the script's only non-GET call is that patch.
#
# The script is extracted from the manifest rather than copied here, so this
# cannot drift into testing a stale transcription of it.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly coroot="${root_dir}/k8s/bases/infrastructure/controllers/coroot"
readonly manifest="${coroot}/cron-job-prune-protected-orphan-alert.yaml"
readonly cluster_role="${coroot}/cluster-role-prune-protected-orphan-alert.yaml"
readonly role="${coroot}/role-prune-protected-orphan-alert.yaml"
readonly config_map="${coroot}/config-map-prune-protected-orphan-alert.yaml"
readonly kustomization="${coroot}/kustomization.yaml"
readonly STATE_NAME='prune-protected-orphan-alert-state'
# Not a hooks.slack.com-shaped literal: GitHub push protection matches that
# shape. `.invalid` is RFC 2606 reserved, so this can never resolve.
readonly REAL_WEBHOOK='https://hooks.test.invalid/delivery-target'
readonly PLACEHOLDER_WEBHOOK='https://example.invalid/no-slack-configured'
readonly HR_ID='tofu-system_tofu-controller_helm.toolkit.fluxcd.io_HelmRelease'
readonly NS_ID='_tofu-system__Namespace'
readonly PVC_ID='actual-budget_data__PersistentVolumeClaim'
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
for file in "${manifest}" "${cluster_role}" "${role}" "${config_map}" "${kustomization}"; do
  [ -f "${file}" ] || fail "not found: ${file}"
done

# ---------------------------------------------------------------------------
# Structural assertions.
# ---------------------------------------------------------------------------
container_path='.spec.jobTemplate.spec.template.spec.containers[0]'
pod_path='.spec.jobTemplate.spec.template.spec'
script_body="$(yq eval "${container_path}.command[2]" "${manifest}")"
[ -n "${script_body}" ] && [ "${script_body}" != "null" ] ||
  fail "could not extract the container script from ${manifest}"

cluster_rules="$(yq eval -o=json '.rules' "${cluster_role}" |
  jq -cS 'map({g: .apiGroups, r: (.resources | sort), v: (.verbs | sort), n: .resourceNames}) | sort_by(.g)')"
expected_cluster_rules='[{"g":[""],"n":null,"r":["namespaces","persistentvolumeclaims"],"v":["get","list"]},{"g":["helm.toolkit.fluxcd.io"],"n":null,"r":["helmreleases"],"v":["get","list"]},{"g":["kustomize.toolkit.fluxcd.io"],"n":null,"r":["kustomizations"],"v":["get","list"]}]'
[ "${cluster_rules}" = "${expected_cluster_rules}" ] ||
  fail "ClusterRole must be exactly get/list on namespaces, persistentvolumeclaims, helmreleases and kustomizations; found: ${cluster_rules}"
pass "ClusterRole is get/list on the four read resources only"

role_rules="$(yq eval -o=json '.rules' "${role}" | jq -cS .)"
expected_role_rules="[{\"apiGroups\":[\"\"],\"resourceNames\":[\"${STATE_NAME}\"],\"resources\":[\"configmaps\"],\"verbs\":[\"get\",\"patch\"]}]"
[ "${role_rules}" = "${expected_role_rules}" ] ||
  fail "Role must be exactly get/patch on the ${STATE_NAME} ConfigMap by name; found: ${role_rules}"
[ "$(yq eval '.metadata.name' "${config_map}")" = "${STATE_NAME}" ] ||
  fail "the state ConfigMap is not named ${STATE_NAME}, so the Role would grant nothing"
[ "$(yq eval '.metadata.annotations["kustomize.toolkit.fluxcd.io/ssa"]' "${config_map}")" = "IfNotPresent" ] ||
  fail "the state ConfigMap must be created once by Flux (ssa: IfNotPresent), or every reconcile would reset the grace window"
pass "the only write grant is get/patch on the Flux-created-once state ConfigMap"

# The script's only method override is the state patch.
overrides="$(grep -Ec -- '(^|[[:space:]])(-X|--request)([[:space:]]|=|$)' <<<"${script_body}" || true)"
if [ "${overrides}" != "1" ] || ! grep -Eq -- '-X PATCH' <<<"${script_body}"; then
  fail "the script must carry exactly one method override, the state PATCH; found ${overrides}"
fi
grep -Fq "STATE_PATH=/api/v1/namespaces/observability/configmaps/${STATE_NAME}" <<<"${script_body}" ||
  fail "the patched path is not the state ConfigMap"
pass "the script's only write is a PATCH of its own state"

for file in service-account role-binding cluster-role-binding secret cluster-role role config-map cron-job; do
  grep -qxF -- "  - ${file}-prune-protected-orphan-alert.yaml" "${kustomization}" ||
    fail "kustomization.yaml does not list ${file}-prune-protected-orphan-alert.yaml"
done
pass "kustomization.yaml lists all eight resources"

if grep -q 'secretKeyRef' <<<"$(yq eval "${container_path}.env" "${manifest}")"; then
  fail "CKV_K8S_35: container env carries a secretKeyRef; the webhook must arrive as a mounted file"
fi
default_mode="$(yq eval "${pod_path}.volumes[] | select(.name == \"webhook\") | .secret.defaultMode" "${manifest}")"
[ "${default_mode}" = "288" ] || fail "expected the webhook volume defaultMode 0440 (288 decimal), got: ${default_mode}"
[ "$(yq eval "${pod_path}.securityContext.fsGroup" "${manifest}")" = "$(yq eval "${pod_path}.securityContext.runAsUser" "${manifest}")" ] ||
  fail "fsGroup must match runAsUser, or the 0440 webhook file is unreadable"
if grep -Eq 'curl.*"\$\{?WEBHOOK_URL\}?"' <<<"${script_body}"; then
  fail "the webhook URL is passed to curl as an argument; it would appear in /proc/<pid>/cmdline"
fi
grep -Fq -- '--config -' <<<"${script_body}" || fail "the delivery does not read its URL from a stdin config"
[ "$(yq eval '.metadata.annotations["kustomize.toolkit.fluxcd.io/substitute"]' "${manifest}")" = "disabled" ] ||
  fail "the CronJob must opt out of Flux substitution, or \${BOUND_SECONDS} expands to empty"
pass "webhook is a 0440 mounted file never in argv, and Flux substitution is disabled"

# ---------------------------------------------------------------------------
# Behavioural scenarios.
# ---------------------------------------------------------------------------
work_root="$(mktemp -d /tmp/tmp.XXXXXXXXXX)"
trap 'rm -rf "${work_root}"' EXIT
now_epoch="$(date -u +%s)"
readonly now_epoch
readonly eight_days_one_hour=$((8 * 86400 + 3600))

ks_body() { # $@ inventory ids of flux-system/infrastructure-controllers
  jq -cn --args '{items: [{metadata: {name: "infrastructure-controllers", namespace: "flux-system"},
                           status: {inventory: {entries: ($ARGS.positional | map({id: ., v: "v1"}))}}}]}' "$@"
}
obj() { # $1 namespace ("" = cluster-scoped), $2 name, $3 owner Kustomization, $4 prune annotation ("" = none), $5 deleting
  jq -cn --arg ns "$1" --arg name "$2" --arg owner "$3" --arg prune "$4" --arg deleting "${5:-false}" '
    {metadata: ({name: $name,
                 labels: {"kustomize.toolkit.fluxcd.io/name": $owner, "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
                 annotations: (if $prune == "" then {} else {"kustomize.toolkit.fluxcd.io/prune": $prune} end)}
                + (if $ns == "" then {} else {namespace: $ns} end)
                + (if $deleting == "true" then {deletionTimestamp: "2026-09-13T00:00:00Z"} else {} end))}'
}
list_of() { jq -sc '{items: .}'; }
state_body() { # $1 first-seen JSON ("" = the fresh, data-less object Flux creates)
  if [ -z "$1" ]; then
    jq -cn --arg n "${STATE_NAME}" '{metadata: {name: $n, namespace: "observability"}}'
  else
    jq -cn --arg n "${STATE_NAME}" --arg s "$1" '{metadata: {name: $n, namespace: "observability"}, data: {"first-seen.json": $s}}'
  fi
}

setup_scenario() {
  local name="$1" webhook="$2"
  local dir="${work_root}/${name}"
  mkdir -p "${dir}/bin" "${dir}/sa" "${dir}/webhook" "${dir}/tmp" "${dir}/api"
  printf 'fake-ca' >"${dir}/sa/ca.crt"
  printf 'fake-token' >"${dir}/sa/token"
  printf '%s' "${webhook}" >"${dir}/webhook/url"
  # Default: the three protected objects, all still in their inventory.
  ks_body "${HR_ID}" "${NS_ID}" "${PVC_ID}" >"${dir}/api/kustomizations.body"
  obj actual-budget data infrastructure-controllers disabled | list_of >"${dir}/api/pvcs.body"
  obj '' tofu-system infrastructure-controllers disabled | list_of >"${dir}/api/namespaces.body"
  obj tofu-system tofu-controller infrastructure-controllers disabled | list_of >"${dir}/api/helmreleases.body"
  state_body '' >"${dir}/api/state.body"
  printf '{}' >"${dir}/api/patch.body"
  for key in kustomizations pvcs namespaces helmreleases state patch; do
    printf '200' >"${dir}/api/${key}.code"
  done
  cat >"${dir}/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl. Kube API calls (those with -o) are served from canned bodies by
# method and exact path; the delivery is the call with no -o.
set -uo pipefail
dir="${SCENARIO_DIR}"
out_file=""
url=""
method="GET"
data=""
prev=""
for arg in "$@"; do
  [ "${prev}" = "-o" ] && out_file="${arg}"
  [ "${prev}" = "-X" ] && method="${arg}"
  [ "${prev}" = "--data" ] && data="${arg}"
  case "${arg}" in https://*) url="${arg}" ;; esac
  prev="${arg}"
done
if [ -n "${out_file}" ]; then
  key=""
  case "${method} ${url}" in
    "GET "*"/apis/kustomize.toolkit.fluxcd.io/v1/kustomizations") key="kustomizations" ;;
    "GET "*"/api/v1/persistentvolumeclaims?labelSelector=kustomize.toolkit.fluxcd.io%2Fname") key="pvcs" ;;
    "GET "*"/api/v1/namespaces?labelSelector=kustomize.toolkit.fluxcd.io%2Fname") key="namespaces" ;;
    "GET "*"/apis/helm.toolkit.fluxcd.io/v2/helmreleases?labelSelector=kustomize.toolkit.fluxcd.io%2Fname") key="helmreleases" ;;
    "GET "*"/api/v1/namespaces/observability/configmaps/prune-protected-orphan-alert-state") key="state" ;;
    "PATCH "*"/api/v1/namespaces/observability/configmaps/prune-protected-orphan-alert-state?fieldManager=prune-protected-orphan-alert")
      key="patch"
      cp "${data#@}" "${dir}/patched.json"
      ;;
  esac
  [ -n "${key}" ] || { printf '%s\n' "unstubbed: ${method} ${url}" >>"${dir}/stub-errors.log"; printf '599'; exit 0; }
  printf '%s\n' "${method} ${url}" >>"${dir}/api-calls.log"
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

# Run the extracted script in a sandbox, byte-identical to the container's except
# for the in-cluster absolute paths, which are redirected.
run_scenario() {
  local dir="$1"
  local patched="${dir}/script.sh"
  {
    printf '%s\n' 'set -eu'
    printf 'BOUND_SECONDS=%s\n' 604800
    printf 'export SCENARIO_DIR=%q\n' "${dir}"
    printf 'SA_OVERRIDE=%q\n' "${dir}/sa"
    # ORDER IS LOAD-BEARING: the `/tmp/` rewrite runs FIRST, so the later
    # rewrites' sandbox paths are not rewritten a second time.
    # shellcheck disable=SC2016  # `$SA_OVERRIDE` is emitted INTO the patched script.
    printf '%s\n' "${script_body}" |
      sed -e 's#/tmp/#'"${dir}"'/tmp/#g' \
        -e 's#^\( *\)SA=/var/run/secrets/kubernetes.io/serviceaccount$#\1SA="$SA_OVERRIDE"#' \
        -e 's#/etc/prune-protected-orphan-alert/url#'"${dir}"'/webhook/url#g'
  } >"${patched}"
  # shellcheck disable=SC2016  # matching the LITERAL `$SA_OVERRIDE` the sed wrote.
  grep -q 'SA="\$SA_OVERRIDE"' "${patched}" ||
    fail "serviceaccount path redirection did not apply; the script's SA line changed shape"
  grep -q "${dir}/webhook/url" "${patched}" || fail "webhook path redirection did not apply"
  ! grep -q -- "${dir}/tmp/${dir#/}" "${patched}" ||
    fail "a redirection rewrote another rule's output; the sandbox path is doubled"
  (
    cd "${dir}"
    PATH="${dir}/bin:${PATH}" SCENARIO_DIR="${dir}" bash "${patched}" >"${dir}/stdout.log" 2>"${dir}/stderr.log"
  )
}
delivered() { [ -f "$1/deliveries.log" ]; }
patched_state() { jq -r '.data["first-seen.json"]' "$1/patched.json"; }

# 1. Clean: every protected object is still in its inventory. Quiet, no write.
dir="$(setup_scenario clean "${REAL_WEBHOOK}")"
run_scenario "${dir}" || fail "clean: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "clean: a fully managed cluster alerted"
[ ! -f "${dir}/patched.json" ] || fail "clean: an unchanged state was rewritten"
grep -q 'none left behind' "${dir}/stdout.log" || fail "clean: the quiet verdict was not logged"
grep -q "GET .*configmaps/${STATE_NAME}\$" "${dir}/api-calls.log" || fail "clean: the state record was never read"
pass "a fully managed cluster stays quiet and writes nothing"

# 2. The #3480 shape, fresh: the HelmRelease was removed from Git, so it left
#    its inventory, but it is still running. Warn, record the sighting, no page.
dir="$(setup_scenario fresh "${REAL_WEBHOOK}")"
ks_body "${NS_ID}" "${PVC_ID}" >"${dir}/api/kustomizations.body"
run_scenario "${dir}" || fail "fresh: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "fresh: a resource inside the grace window paged"
if ! grep -q 'WARNING' "${dir}/stdout.log" || ! grep -qF 'HelmRelease tofu-system/tofu-controller' "${dir}/stdout.log"; then
  fail "fresh: the left-behind HelmRelease was not reported — this is the RED nothing caught for 74 days"
fi
grep -qF 'flux-system/infrastructure-controllers' "${dir}/stdout.log" || fail "fresh: the owning Kustomization is not named"
[ -f "${dir}/patched.json" ] || fail "fresh: the first sighting was not recorded"
[ "$(patched_state "${dir}" | jq -r 'keys | join(",")')" = "${HR_ID}" ] ||
  fail "fresh: the state must record exactly the left-behind HelmRelease; got $(patched_state "${dir}")"
seen="$(patched_state "${dir}" | jq -r --arg id "${HR_ID}" '.[$id]')"
[ "${seen}" -ge "${now_epoch}" ] && [ "${seen}" -le $((now_epoch + 120)) ] ||
  fail "fresh: the first sighting ${seen} is not the time of this run (${now_epoch})"
pass "a freshly left-behind HelmRelease is logged and its first sighting recorded"

# 3. The #3480 shape, stale: first seen 8 days ago. Page, naming it, and keep the
#    original sighting rather than restarting the window.
dir="$(setup_scenario stale "${REAL_WEBHOOK}")"
ks_body "${NS_ID}" "${PVC_ID}" >"${dir}/api/kustomizations.body"
state_body "{\"${HR_ID}\":$((now_epoch - eight_days_one_hour))}" >"${dir}/api/state.body"
run_scenario "${dir}" || fail "stale: the script exited non-zero: $(cat "${dir}/stderr.log")"
delivered "${dir}" || fail "stale: a resource left behind for 8 days did not page"
payload="$(jq -r .text "${dir}/delivered-payload.json")"
for needle in 'HelmRelease tofu-system/tofu-controller' 'flux-system/infrastructure-controllers' 'first seen 8d 1h ago' 'prune: disabled'; do
  grep -qF -- "${needle}" <<<"${payload}" || fail "stale: the alert does not say '${needle}'; payload: ${payload}"
done
[ ! -f "${dir}/patched.json" ] || fail "stale: the recorded first sighting was rewritten"
grep -q "^url = ${REAL_WEBHOOK}$" "${dir}/curl-stdin.log" || fail "stale: the webhook URL did not travel on curl's stdin config"
pass "a resource left behind past the bound pages with kind, object, owner and age"

# 4. Resolved: the state remembers an old finding that has since been deleted or
#    restored. Forget it; no page.
dir="$(setup_scenario resolved "${REAL_WEBHOOK}")"
state_body "{\"${HR_ID}\":$((now_epoch - eight_days_one_hour))}" >"${dir}/api/state.body"
run_scenario "${dir}" || fail "resolved: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "resolved: a finding that no longer exists paged"
[ "$(patched_state "${dir}" 2>/dev/null)" = "{}" ] || fail "resolved: the resolved finding was not forgotten"
pass "a resolved finding is forgotten"

# 5. The whole Kustomization was retired: its objects keep its labels but it no
#    longer exists, so there is no inventory that could ever list them again.
dir="$(setup_scenario owner-gone "${REAL_WEBHOOK}")"
obj '' tofu-system retired disabled | list_of >"${dir}/api/namespaces.body"
state_body "{\"${NS_ID}\":$((now_epoch - eight_days_one_hour))}" >"${dir}/api/state.body"
run_scenario "${dir}" || fail "owner-gone: the script exited non-zero: $(cat "${dir}/stderr.log")"
delivered "${dir}" || fail "owner-gone: an object whose Kustomization no longer exists did not page"
payload="$(jq -r .text "${dir}/delivered-payload.json")"
if ! grep -qF 'Namespace tofu-system' <<<"${payload}" || ! grep -qF 'flux-system/retired` no longer exists' <<<"${payload}"; then
  fail "owner-gone: the alert does not name the object and its vanished Kustomization; payload: ${payload}"
fi
pass "an object whose Kustomization no longer exists is reported"

# 6. Quiet shapes: an unprotected object Flux will prune itself, and a protected
#    one already being deleted, are neither reported nor recorded.
dir="$(setup_scenario quiet "${REAL_WEBHOOK}")"
ks_body "${NS_ID}" >"${dir}/api/kustomizations.body"
obj tofu-system tofu-controller infrastructure-controllers '' | list_of >"${dir}/api/helmreleases.body"
obj actual-budget data infrastructure-controllers disabled true | list_of >"${dir}/api/pvcs.body"
run_scenario "${dir}" || fail "quiet: the script exited non-zero: $(cat "${dir}/stderr.log")"
if delivered "${dir}" || [ -f "${dir}/patched.json" ] || grep -q 'WARNING' "${dir}/stdout.log"; then
  fail "quiet: an unprotected or already-deleting object was reported"
fi
pass "unprotected and already-deleting objects stay quiet"

# 7. A Kustomization with no inventory cannot be judged: fail, never guess.
dir="$(setup_scenario no-inventory "${REAL_WEBHOOK}")"
printf '%s' '{"items":[{"metadata":{"name":"infrastructure-controllers","namespace":"flux-system"},"status":{}}]}' >"${dir}/api/kustomizations.body"
if run_scenario "${dir}"; then fail "no-inventory: a Kustomization without an inventory exited 0 (silent zero)"; fi
grep -q 'records no inventory' "${dir}/stderr.log" || fail "no-inventory: the failure does not say why"
! delivered "${dir}" && [ ! -f "${dir}/patched.json" ] || fail "no-inventory: it paged or wrote state before failing"
pass "a Kustomization with no inventory fails the run instead of guessing"

# 8. Silent-zero guards on every read and the state write.
dir="$(setup_scenario list-error "${REAL_WEBHOOK}")"
printf '403' >"${dir}/api/helmreleases.code"
if run_scenario "${dir}"; then fail "list-error: an HTTP 403 HelmRelease list exited 0 (silent zero)"; fi
grep -q 'HTTP 403 listing Flux-applied HelmReleases' "${dir}/stderr.log" || fail "list-error: the failure does not name the code and list"
dir="$(setup_scenario list-shape "${REAL_WEBHOOK}")"
printf '%s' '{"items":{}}' >"${dir}/api/pvcs.body"
if run_scenario "${dir}"; then fail "list-shape: a malformed PVC list exited 0 (silent zero)"; fi
pass "a failed or malformed list fails the run loudly"

dir="$(setup_scenario state-missing "${REAL_WEBHOOK}")"
printf '404' >"${dir}/api/state.code"
if run_scenario "${dir}"; then fail "state-missing: a missing state ConfigMap exited 0"; fi
grep -q 'HTTP 404 reading the state ConfigMap' "${dir}/stderr.log" || fail "state-missing: the failure does not say why"
for bad in 'not json' '{"x":"yesterday"}' '[1]'; do
  dir="$(setup_scenario "state-bad-$((RANDOM))" "${REAL_WEBHOOK}")"
  state_body "${bad}" >"${dir}/api/state.body"
  if run_scenario "${dir}"; then fail "state-bad: first-seen.json '${bad}' exited 0"; fi
  grep -q 'not an object of epoch seconds' "${dir}/stderr.log" || fail "state-bad: '${bad}' failed for the wrong reason: $(cat "${dir}/stderr.log")"
done
pass "a missing or malformed state record fails the run loudly"

dir="$(setup_scenario patch-error "${REAL_WEBHOOK}")"
ks_body "${NS_ID}" "${PVC_ID}" >"${dir}/api/kustomizations.body"
printf '409' >"${dir}/api/patch.code"
if run_scenario "${dir}"; then fail "patch-error: a failed state write exited 0"; fi
grep -q 'HTTP 409 updating the state ConfigMap' "${dir}/stderr.log" || fail "patch-error: the failure does not say why"
! delivered "${dir}" || fail "patch-error: it delivered after failing to record state"
pass "a failed state write fails the run before delivering"

# 9. Delivery fails the run — and the state is already saved, so the retry does
#    not restart the new finding's grace window.
dir="$(setup_scenario delivery-fails "${REAL_WEBHOOK}")"
ks_body "${PVC_ID}" >"${dir}/api/kustomizations.body"
state_body "{\"${HR_ID}\":$((now_epoch - eight_days_one_hour))}" >"${dir}/api/state.body"
if STUB_DELIVERY_EXIT=22 run_scenario "${dir}"; then fail "delivery-fails: a failed POST exited 0"; fi
[ "$(patched_state "${dir}" | jq -r 'keys | sort | join(",")')" = "${NS_ID},${HR_ID}" ] ||
  fail "delivery-fails: state was not saved before the failed delivery; got $(patched_state "${dir}" 2>/dev/null || echo none)"
[ "$(patched_state "${dir}" | jq -r --arg id "${HR_ID}" '.[$id]')" = "$((now_epoch - eight_days_one_hour))" ] ||
  fail "delivery-fails: the old finding's first sighting was not kept"
pass "a failed delivery fails the run after the state is saved"

# 10. Placeholder webhook (local/CI): logged, delivery skipped, exit 0.
dir="$(setup_scenario placeholder "${PLACEHOLDER_WEBHOOK}")"
ks_body "${NS_ID}" "${PVC_ID}" >"${dir}/api/kustomizations.body"
state_body "{\"${HR_ID}\":$((now_epoch - eight_days_one_hour))}" >"${dir}/api/state.body"
run_scenario "${dir}" || fail "placeholder: the script exited non-zero: $(cat "${dir}/stderr.log")"
! delivered "${dir}" || fail "placeholder: an alert was delivered to the example.invalid placeholder"
grep -q 'not delivered' "${dir}/stdout.log" || fail "placeholder: the skip was not logged"
pass "the local/CI placeholder webhook skips delivery and exits 0"

# 11. A non-HTTPS webhook is refused before anything else happens.
dir="$(setup_scenario plain-http 'http://hooks.test.invalid:443/delivery-target')"
if run_scenario "${dir}"; then fail "plain-http: an http:// webhook was accepted"; fi
grep -q 'must use https://' "${dir}/stderr.log" || fail "plain-http: the refusal does not name the https requirement"
! delivered "${dir}" || fail "plain-http: an alert was delivered over http"
pass "a non-HTTPS webhook is refused"

for d in "${work_root}"/*/; do
  [ -f "${d}/stub-errors.log" ] && fail "unstubbed API call in $(basename "${d}"): $(cat "${d}/stub-errors.log")"
done
printf 'PASS: prune-protected-orphan-alert warns, escalates past the bound, stays quiet on managed shapes and never reports a silent zero\n'
