#!/usr/bin/env bash

# A bad verify on the root OCIRepository is delivered through that same source, so
# without an out-of-band repair it can never be replaced (platform#3014). Pin that the
# deploy repairs it from this commit's declared value: through the FluxInstance that
# flux-operator renders the source from, never by writing the source; with no write
# at all on a healthy cluster; and failing closed on anything it cannot read.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly script="${root_dir}/scripts/reassert-flux-root-verify.sh"
readonly declared_file="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/flux-instance/flux-instance.yaml"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

[[ -x "${script}" ]] || fail 'reassert-flux-root-verify.sh must be an executable script'

tmp_dir="$(mktemp -d)"
readonly tmp_dir
cleanup() { rm -rf "${tmp_dir}"; }
trap cleanup EXIT

# The fake models flux-operator: annotating the FluxInstance re-renders the source's
# spec.verify from whatever OCIRepository patch the FluxInstance carries AT THAT
# MOMENT. A repair that writes the source instead, or skips the FluxInstance, is
# therefore reverted here exactly as it would be in production.
readonly fake_kubectl="${tmp_dir}/kubectl"
cat >"${fake_kubectl}" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${CALL_LOG}"
args="$*"
readonly filter='.target.kind == "OCIRepository" and .target.name == "flux-system"'
case "${args}" in
*"get fluxinstance flux -o json")
  [[ "${FI_FAIL:-0}" == "1" ]] && { printf 'Error from server (Forbidden)\n' >&2; exit 1; }
  cat "${STATE_DIR}/fi.json"
  ;;
*"get ocirepository flux-system -o json")
  [[ "${SRC_FAIL:-0}" == "1" ]] && { printf 'Error from server (NotFound)\n' >&2; exit 1; }
  cat "${STATE_DIR}/src.json"
  ;;
*"patch fluxinstance flux --type=json --field-manager=kustomize-controller -p "*)
  ops="${args##* -p }"
  jq -e --argjson ops "${ops}" '
    reduce $ops[] as $op (.;
      ($op.path | ltrimstr("/") | split("/") | map(tonumber? // .)) as $p
      | if $op.op == "test" then (if getpath($p) == $op.value then . else error("test failed: \($op.path)") end)
        elif $op.op == "replace" then (if getpath($p) == null then error("replace of a missing path") else setpath($p; $op.value) end)
        else error("unexpected op \($op.op)") end)' \
    "${STATE_DIR}/fi.json" >"${STATE_DIR}/fi.next" || { printf 'patch rejected\n' >&2; exit 1; }
  mv "${STATE_DIR}/fi.next" "${STATE_DIR}/fi.json"
  ;;
*"annotate fluxinstance flux --overwrite reconcile.fluxcd.io/requestedAt="*)
  if [[ "${OPERATOR_RENDERS:-1}" == "1" ]]; then
    patch="$(jq -r "[.spec.kustomize.patches[] | select(${filter}) | .patch][0]" "${STATE_DIR}/fi.json")"
    verify="$(yq -o=json '[.[] | select(.path == "/spec/verify") | .value][0]' <<<"${patch}")"
    jq --argjson v "${verify}" '.spec.verify = $v' "${STATE_DIR}/src.json" >"${STATE_DIR}/src.next"
    mv "${STATE_DIR}/src.next" "${STATE_DIR}/src.json"
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

readonly filter='.target.kind == "OCIRepository" and .target.name == "flux-system"'
healthy_fi="$(yq -o=json '.' "${declared_file}")"
readonly healthy_fi
declared_verify="$(jq -r "[.spec.kustomize.patches[] | select(${filter}) | .patch][0]" <<<"${healthy_fi}" |
  yq -o=json '[.[] | select(.path == "/spec/verify") | .value][0]')"
readonly declared_verify
[[ "$(jq -r '.provider' <<<"${declared_verify}")" == 'cosign' ]] ||
  fail 'fixture: the declared patch must set a cosign verify'

# The 2026-08-07 incident shape: a multi-entry matcher, which cosign rejects outright.
readonly bad_patch='- op: add
  path: /spec/verify
  value:
    provider: cosign
    matchOIDCIdentity:
      - issuer: "^https://token\\.actions\\.githubusercontent\\.com$"
        subject: "^first$"
      - issuer: "^https://token\\.actions\\.githubusercontent\\.com$"
        subject: "^second$"
'
with_source_patch() { # <patch-text>
  jq --arg p "$1" "(.spec.kustomize.patches[] | select(${filter}) | .patch) = \$p" <<<"${healthy_fi}"
}
bad_fi="$(with_source_patch "${bad_patch}")"
readonly bad_fi
bad_verify="$(yq -o=json '.[0].value' <<<"${bad_patch}")"
readonly bad_verify
src_with() { jq -n --argjson v "$1" '{spec: {verify: $v}}'; }

case_dir=''
status=0
output=''

# run_case <name> <fi-json> <src-json> [VAR=value ...]
run_case() {
  local name="$1" fi="$2" src="$3"
  shift 3
  case_dir="${tmp_dir}/${name}"
  mkdir -p "${case_dir}"
  : >"${case_dir}/calls"
  printf '%s' "${fi}" >"${case_dir}/fi.json"
  printf '%s' "${src}" >"${case_dir}/src.json"
  set +e
  output="$(env -i PATH="${PATH}" HOME="${HOME}" \
    KUBECTL="${fake_kubectl}" CALL_LOG="${case_dir}/calls" STATE_DIR="${case_dir}" \
    GITHUB_STEP_SUMMARY="${case_dir}/summary" \
    FLUX_VERIFY_POLL_INTERVAL_SECONDS=0 FLUX_VERIFY_RENDER_TIMEOUT_SECONDS=2 "$@" \
    "${script}" 2>&1)"
  status=$?
  set -e
}

writes() { grep -cE ' (patch|annotate) ' "${case_dir}/calls" || true; }
source_verify() { jq -S -c '.spec.verify' "${case_dir}/src.json"; }
canonical() { jq -S -c . <<<"$1"; }

expect_status() { # <case> <expected>
  [[ "${status}" -eq "$2" ]] || fail "$1: expected exit $2, got ${status}: ${output}"
}
expect_no_write() { # <case>
  [[ "$(writes)" -eq 0 ]] || fail "$1: wrote to the cluster but must not have: $(cat "${case_dir}/calls")"
}

# 1. Healthy cluster (negative control): both objects already declare it ⇒ no write.
run_case healthy "${healthy_fi}" "$(src_with "${declared_verify}")"
expect_status healthy 0
expect_no_write healthy
[[ "${output}" == *CURRENT* ]] || fail "healthy: must report CURRENT: ${output}"
grep -qF 'CURRENT' "${case_dir}/summary" || fail 'healthy: must record the outcome in the step summary'

# 2. DECISIVE: the incident. The live FluxInstance and source both carry a multi-entry
#    matcher; the deploy repairs both with no manual intervention.
run_case incident "${bad_fi}" "$(src_with "${bad_verify}")"
expect_status incident 0
[[ "$(source_verify)" == "$(canonical "${declared_verify}")" ]] ||
  fail "incident: the source must end on the declared verify, got $(source_verify)"
grep -qF -- '--field-manager=kustomize-controller' "${case_dir}/calls" ||
  fail 'incident: the FluxInstance repair must write under kustomize-controller'
grep -qF 'annotate ocirepository flux-system' "${case_dir}/calls" ||
  fail 'incident: the source must be asked to fetch after it is re-rendered'
grep -qF 'REPAIRED' "${case_dir}/summary" || fail 'incident: must record REPAIRED in the step summary'
# The only repair is the FluxInstance: the source has a single writer, flux-operator.
if grep -E ' (patch|apply|replace|edit) ocirepository' "${case_dir}/calls" | grep -q .; then
  fail 'incident: the source must never be written directly'
fi
# Order: FluxInstance repaired, then re-rendered, then the source fetches.
awk '/ patch fluxinstance /{p=NR} / annotate fluxinstance /{a=NR} / annotate ocirepository /{s=NR}
  END { exit (p && a && s && p < a && a < s) ? 0 : 1 }' "${case_dir}/calls" ||
  fail "incident: expected patch → reconcile request → fetch request, got: $(cat "${case_dir}/calls")"

# 3. Every call is pinned to the prod context and the flux-system namespace.
if grep -v -- '^--context admin@prod -n flux-system ' "${case_dir}/calls" | grep -q .; then
  fail 'every kubectl call must pass --context admin@prod -n flux-system'
fi

# 4. The repair targets the right entry when the source patch is not the first one.
reordered="$(jq '.spec.kustomize.patches |= ([.[] | select(.target.kind != "OCIRepository")] + [.[] | select(.target.kind == "OCIRepository")])' <<<"${bad_fi}")"
run_case reordered "${reordered}" "$(src_with "${bad_verify}")"
expect_status reordered 0
[[ "$(source_verify)" == "$(canonical "${declared_verify}")" ]] ||
  fail 'reordered: must repair the OCIRepository patch wherever it sits'
jq -e '[.spec.kustomize.patches[] | select(.target.kind == "Deployment")] | length > 0' "${case_dir}/fi.json" >/dev/null ||
  fail 'reordered: the other patches must survive'

# 5. FluxInstance already corrected but the source still stale ⇒ re-render only.
run_case stale_source "${healthy_fi}" "$(src_with "${bad_verify}")"
expect_status stale_source 0
grep -qF ' patch fluxinstance ' "${case_dir}/calls" && fail 'stale_source: must not rewrite a current FluxInstance'
[[ "$(source_verify)" == "$(canonical "${declared_verify}")" ]] || fail 'stale_source: must re-render the source'

# 6. An unparseable live patch is the bad state, not a reason to stop.
run_case garbled "$(with_source_patch 'not: [a, json6902, list')" "$(src_with "${bad_verify}")"
expect_status garbled 0
[[ "$(source_verify)" == "$(canonical "${declared_verify}")" ]] || fail 'garbled: must repair an unparseable patch'

# 7. flux-operator never re-renders ⇒ exit 1 and says so, never a pass.
run_case no_render "${bad_fi}" "$(src_with "${bad_verify}")" OPERATOR_RENDERS=0
expect_status no_render 1
grep -qF 'REPAIR INCOMPLETE' "${case_dir}/summary" || fail 'no_render: must record REPAIR INCOMPLETE'
grep -qF 'annotate ocirepository' "${case_dir}/calls" && fail 'no_render: must not ask a stale source to fetch'

# 8–11. Unreadable or ambiguous live state fails closed, without writing.
run_case fi_unreadable "${bad_fi}" "$(src_with "${bad_verify}")" FI_FAIL=1
expect_status fi_unreadable 2
expect_no_write fi_unreadable

run_case src_unreadable "${bad_fi}" "$(src_with "${bad_verify}")" SRC_FAIL=1
expect_status src_unreadable 2
expect_no_write src_unreadable

run_case no_entry "$(jq '.spec.kustomize.patches |= map(select(.target.kind != "OCIRepository"))' <<<"${bad_fi}")" \
  "$(src_with "${bad_verify}")"
expect_status no_entry 2
expect_no_write no_entry

run_case two_entries "$(jq '.spec.kustomize.patches += [.spec.kustomize.patches[] | select(.target.kind == "OCIRepository")]' <<<"${bad_fi}")" \
  "$(src_with "${bad_verify}")"
expect_status two_entries 2
expect_no_write two_entries

# 12. A declared file that does not set exactly one verify fails closed, without writing.
two_ops_file="${tmp_dir}/two-ops.yaml"
yq '(.spec.kustomize.patches[] | select(.target.kind == "OCIRepository") | .patch) |= (. + "\n" + .)' \
  "${declared_file}" >"${two_ops_file}"
run_case declared_ambiguous "${bad_fi}" "$(src_with "${bad_verify}")" FLUX_INSTANCE_FILE="${two_ops_file}"
expect_status declared_ambiguous 2
expect_no_write declared_ambiguous

# 13. Wiring: the deploy runs it after publishing and before reconciling, on every deploy.
#     No `if:`, so a green deploy cannot have skipped it without the step itself failing.
awk '/- name: 🔎 Verify Flux GHCR pull credential after publish/ { published = 1; next }
  published && /- name: 🛡️ Reassert the root source verification/ { in_step = 1; seen = 1; next }
  /- name: 🔁 Trigger Flux reconciliation/ { if (!seen) bad = 1; exit }
  in_step && /- name:/ { in_step = 0 }
  in_step && /^[[:space:]]*if:/ { bad = 1 }
  in_step && /run: \.\/scripts\/reassert-flux-root-verify\.sh[[:space:]]*$/ { runs = 1 }
  END { exit (seen && runs && !bad) ? 0 : 1 }' "${deploy_action}" ||
  fail 'deploy-prod must run reassert-flux-root-verify.sh unconditionally, after publish and before the reconcile'
grep -qF -- "- 'scripts/reassert-flux-root-verify.sh'" "${ci_workflow}" ||
  fail 'ci.yaml must run this test when the script changes'
grep -qF -- "- 'scripts/tests/test-reassert-flux-root-verify.sh'" "${ci_workflow}" ||
  fail 'ci.yaml must run this test when the test changes'
grep -qF -- 'run: bash scripts/tests/test-reassert-flux-root-verify.sh' "${ci_workflow}" ||
  fail 'ci.yaml must run this test'

printf 'reassert-flux-root-verify: all cases passed\n'
