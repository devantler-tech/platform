#!/usr/bin/env bash

# The root manifests package is public, so the root Flux source must pull it
# anonymously (platform#6272). Pin that the deploy removes spec.sync.pullSecret
# from the FluxInstance only after proving an anonymous pull of the declared
# artifact; that it writes nothing on a detached cluster; that it never touches
# spec.verify; that it fails closed on anything it cannot read; and that both
# production entrypoints run it before the mutable tag moves.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/detach-flux-root-pull-secret.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly dr_workflow="${root_dir}/.github/workflows/dr-rebuild.yaml"
readonly ksail_prod="${root_dir}/ksail.prod.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${script}" ]] || fail 'detach-flux-root-pull-secret.sh must be an executable script'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

# The fake models flux-operator: annotating the FluxInstance re-renders the
# source's spec.secretRef from the pullSecret the FluxInstance carries AT THAT
# MOMENT, moves the source's generation when it changes, and marks it Ready.
readonly fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CALL_LOG}"
args="$*"
case "${args}" in
*"get fluxinstance flux -o json")
  [[ "${FI_FAIL:-0}" == "1" ]] && { printf 'Error from server (Forbidden)\n' >&2; exit 1; }
  cat "${STATE_DIR}/fi.json"
  ;;
*"get ocirepository flux-system -o json")
  [[ "${SRC_FAIL:-0}" == "1" ]] && { printf 'Error from server (NotFound)\n' >&2; exit 1; }
  cat "${STATE_DIR}/src.json"
  ;;
*"patch fluxinstance flux --type=json --field-manager=ksail -p "*)
  ops="${args##* -p }"
  jq -e --argjson ops "${ops}" '
    reduce $ops[] as $op (.;
      ($op.path | ltrimstr("/") | split("/")) as $p
      | if $op.op == "test" then (if getpath($p) == $op.value then . else error("test failed: \($op.path)") end)
        elif $op.op == "remove" then (if getpath($p) == null then error("remove of a missing path") else delpaths([$p]) end)
        else error("unexpected op \($op.op)") end)' \
    "${STATE_DIR}/fi.json" >"${STATE_DIR}/fi.next" || { printf 'patch rejected\n' >&2; exit 1; }
  jq '.metadata.generation = ((.metadata.generation // 1) + 1)' "${STATE_DIR}/fi.next" >"${STATE_DIR}/fi.json"
  ;;
*"annotate fluxinstance flux --overwrite reconcile.fluxcd.io/requestedAt="*)
  if [[ "${OPERATOR_RENDERS:-1}" == "1" ]]; then
    secret="$(jq -r '.spec.sync.pullSecret // ""' "${STATE_DIR}/fi.json")"
    jq --arg s "${secret}" '
      (.spec.secretRef.name // "") as $old
      | (if $s == "" then del(.spec.secretRef) else .spec.secretRef = {name: $s} end)
      | (if $old != $s then .metadata.generation += 1 else . end)
      | .status.conditions = [{type: "Ready", status: "True", observedGeneration: .metadata.generation}]' \
      "${STATE_DIR}/src.json" >"${STATE_DIR}/src.next"
    mv "${STATE_DIR}/src.next" "${STATE_DIR}/src.json"
    jq '.status.conditions = [{type: "Ready", status: "True", observedGeneration: .metadata.generation}]' \
      "${STATE_DIR}/fi.json" >"${STATE_DIR}/fi.next"
    mv "${STATE_DIR}/fi.next" "${STATE_DIR}/fi.json"
  fi
  ;;
*"annotate ocirepository flux-system --overwrite reconcile.fluxcd.io/requestedAt="*) ;;
*)
  printf 'unexpected kubectl call: %s\n' "${args}" >&2
  exit 99
  ;;
esac
FAKE_KUBECTL
chmod +x "${fake_kubectl}"

# The fake models GHCR's anonymous token exchange and manifest HEAD. It fails
# the run if any request carries basic auth or a credential file.
readonly fake_curl="${tmp_dir}/curl"
cat >"${fake_curl}" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CURL_LOG}"
case " $* " in
*" --user "* | *" -u "* | *" --netrc"* | *"Authorization: Basic"*)
  printf 'credential sent\n' >&2
  exit 98
  ;;
esac
case "$*" in
*"https://ghcr.io/token?scope=repository:devantler-tech/platform/manifests:pull&service=ghcr.io"*)
  [[ "${TOKEN_FAIL:-0}" == "1" ]] && exit 22
  printf '{"token":"anonymous-fixture-token"}\n'
  ;;
*"https://ghcr.io/v2/devantler-tech/platform/manifests/manifests/"*)
  [[ "$*" == *"Authorization: Bearer anonymous-fixture-token"* ]] || { printf '401'; exit 0; }
  printf '%s' "${ANON_STATUS:-200}"
  ;;
*)
  printf 'unexpected curl call: %s\n' "$*" >&2
  exit 99
  ;;
esac
FAKE_CURL
chmod +x "${fake_curl}"

readonly verify='{"provider":"cosign","matchOIDCIdentity":[{"issuer":"^https://token\\.actions\\.githubusercontent\\.com$","subject":"^fixture$"}]}'
fi_with() { # <pull-secret or empty> [url]
  jq -n --arg s "$1" --arg url "${2:-oci://ghcr.io/devantler-tech/platform/manifests}" '{
    metadata: {generation: 4},
    spec: {sync: ({kind: "OCIRepository", url: $url, ref: "latest", path: "./clusters/prod", provider: "generic"}
      + (if $s == "" then {} else {pullSecret: $s} end))},
    status: {conditions: [{type: "Ready", status: "True", observedGeneration: 4}]}}'
}
src_with() { # <secret-ref or empty>
  jq -n --arg s "$1" --argjson v "${verify}" '{
    metadata: {generation: 7},
    spec: ({verify: $v} + (if $s == "" then {} else {secretRef: {name: $s}} end)),
    status: {conditions: [{type: "Ready", status: "True", observedGeneration: 7}]}}'
}

case_dir=''
status=0
output=''
run_case() { # <name> <fi-json> <src-json> [VAR=value ...]
  local name="$1" fi_json="$2" src_json="$3"
  shift 3
  case_dir="${tmp_dir}/${name}"
  mkdir -p "${case_dir}"
  printf '%s\n' "${fi_json}" >"${case_dir}/fi.json"
  printf '%s\n' "${src_json}" >"${case_dir}/src.json"
  : >"${case_dir}/calls"
  : >"${case_dir}/curl"
  status=0
  output="$(env STATE_DIR="${case_dir}" CALL_LOG="${case_dir}/calls" CURL_LOG="${case_dir}/curl" \
    KUBECTL="${fake_kubectl}" CURL="${fake_curl}" \
    FLUX_PULL_SECRET_RENDER_TIMEOUT_SECONDS=2 FLUX_PULL_SECRET_POLL_INTERVAL_SECONDS=1 \
    GITHUB_STEP_SUMMARY="${case_dir}/summary" "$@" "${script}" 2>&1)" || status=$?
}
require_status() { [[ "${status}" -eq "$1" ]] || fail "$2: exit ${status}, want $1; output: ${output}"; }
require_no_write() {
  if grep -Eq '^(.* )?(patch|annotate|apply|create|delete|replace|edit) ' "${case_dir}/calls"; then
    fail "$1: wrote to the cluster: $(cat "${case_dir}/calls")"
  fi
}
fi_field() { jq -r "$1" "${case_dir}/fi.json"; }
src_field() { jq -c "$1" "${case_dir}/src.json"; }

# 1. A detached cluster takes no write and makes no registry request.
run_case current "$(fi_with '')" "$(src_with '')"
require_status 0 'detached cluster'
require_no_write 'detached cluster'
[[ ! -s "${case_dir}/curl" ]] || fail 'detached cluster: queried the registry for nothing'
grep -q '^CURRENT' <<<"${output}" || fail "detached cluster: summary does not say CURRENT: ${output}"

# 2. An attached cluster is detached once the anonymous pull is proved, through
#    the FluxInstance under the ksail field manager; verify is untouched.
run_case detach "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)"
require_status 0 'attached cluster'
[[ "$(fi_field '.spec.sync.pullSecret // "absent"')" == absent ]] || fail 'attached cluster: pullSecret not removed'
[[ "$(src_field '.spec.secretRef // "absent"')" == '"absent"' ]] || fail 'attached cluster: source still names a secret'
[[ "$(src_field '.spec.verify')" == "$(jq -c . <<<"${verify}")" ]] || fail 'attached cluster: spec.verify changed'
[[ "$(fi_field '.spec.sync.url')/$(fi_field '.spec.sync.ref')" == 'oci://ghcr.io/devantler-tech/platform/manifests/latest' ]] ||
  fail 'attached cluster: other sync fields changed'
grep -q 'manifests/manifests/latest' "${case_dir}/curl" || fail 'attached cluster: anonymous pull of the synced ref not proved'
first_patch="$(grep -n 'patch fluxinstance' "${case_dir}/calls" | head -1 | cut -d: -f1)"
last_curl_line="$(wc -l <"${case_dir}/curl" | tr -d ' ')"
[[ -n "${first_patch}" && "${last_curl_line}" -ge 2 ]] || fail 'attached cluster: patch issued without both registry probes'
grep -q 'patch ocirepository' "${case_dir}/calls" && fail 'attached cluster: wrote the source directly'
grep -q '^DETACHED' <<<"${output}" || fail "attached cluster: summary does not say DETACHED: ${output}"

# 3. A package that is still private leaves the cluster untouched.
run_case private "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)" ANON_STATUS=401
require_status 2 'private package'
require_no_write 'private package'
[[ "$(fi_field '.spec.sync.pullSecret')" == ksail-registry-credentials ]] || fail 'private package: pullSecret changed'

# 4. A failed anonymous token exchange is not proof of access.
run_case token "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)" TOKEN_FAIL=1
require_status 2 'token failure'
require_no_write 'token failure'

# 5. A FluxInstance syncing from anywhere else is not this script's to change.
run_case foreign "$(fi_with ksail-registry-credentials oci://example.com/other/manifests)" "$(src_with ksail-registry-credentials)"
require_status 2 'foreign source'
require_no_write 'foreign source'

# 6. Unreadable live state fails closed.
run_case fi-unreadable "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)" FI_FAIL=1
require_status 2 'unreadable FluxInstance'
require_no_write 'unreadable FluxInstance'
run_case src-unreadable "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)" SRC_FAIL=1
require_status 2 'unreadable source'
require_no_write 'unreadable source'

# 7. An operator that never re-renders the source is reported, not passed.
run_case stuck "$(fi_with ksail-registry-credentials)" "$(src_with ksail-registry-credentials)" OPERATOR_RENDERS=0
require_status 1 'operator never renders'
grep -q 'DETACH INCOMPLETE' "${case_dir}/summary" || fail 'operator never renders: summary missing'

# 8. A source the operator has not re-rendered yet is converged without a patch.
run_case lagging "$(fi_with '')" "$(src_with ksail-registry-credentials)"
require_status 0 'operator lag'
grep -q 'patch fluxinstance' "${case_dir}/calls" && fail 'operator lag: patched a FluxInstance that names no secret'
[[ "$(src_field '.spec.secretRef // "absent"')" == '"absent"' ]] || fail 'operator lag: source still names a secret'

# 9. Wiring: both production entrypoints detach before the mutable tag moves,
#    and KSail is no longer given a root pull credential.
step_index() { # <file> <needle>
  grep -nF -- "$2" "$1" | head -1 | cut -d: -f1
}
for entrypoint in "${deploy_action}" "${dr_workflow}"; do
  detach="$(step_index "${entrypoint}" './scripts/detach-flux-root-pull-secret.sh')"
  publish="$(step_index "${entrypoint}" 'id: publish_platform_manifest')"
  [[ -n "${detach}" ]] || fail "${entrypoint##*/} does not run the detach step"
  ((detach < publish)) || fail "${entrypoint##*/} detaches after the mutable tag can move"
done
create="$(step_index "${dr_workflow}" 'run: ./scripts/run-ksail-prod.sh cluster create')"
detach="$(step_index "${dr_workflow}" './scripts/detach-flux-root-pull-secret.sh')"
((create < detach)) || fail 'DR detaches before the cluster exists'
registry="$(yq -er '.spec.cluster.localRegistry.registry' "${ksail_prod}")"
[[ "${registry}" == 'ghcr.io/devantler-tech/platform/manifests' ]] ||
  fail "ksail.prod.yaml localRegistry must carry no credential, got ${registry}"

printf 'PASS: the root source detaches from its pull secret only after an anonymous pull is proved\n'
