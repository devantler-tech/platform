#!/usr/bin/env bash
# Offline regressions for scripts/check-flux-prune-failures.sh (#4734): a fake
# kubectl serves Kustomization events, so no cluster or credential is used.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/check-flux-prune-failures.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

readonly digest='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
readonly older='sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
fake_kubectl="${tmp_dir}/kubectl"

cat >"${fake_kubectl}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
expected='--context admin@prod -n flux-system get events --field-selector involvedObject.kind=Kustomization -o json'
[[ "$*" == "${expected}" ]] || {
  printf 'unexpected kubectl arguments: %s\n' "$*" >&2
  exit 91
}
[[ "${FAKE_KUBECTL_FAIL:-false}" != "true" ]] || {
  printf 'the server has asked for the client to provide credentials\n' >&2
  exit 1
}
cat "${FAKE_EVENTS_FILE}"
EOF
chmod +x "${fake_kubectl}"

# event <layer> <reason> <revision-digest> <message>
event() {
  jq -n --arg layer "$1" --arg reason "$2" --arg revision "latest@$3" --arg message "$4" '{
    involvedObject: {kind: "Kustomization", name: $layer, namespace: "flux-system"},
    reason: $reason,
    message: $message,
    metadata: {annotations: {"kustomize.toolkit.fluxcd.io/revision": $revision}}
  }'
}

# events_file <name> <event-json>... writes a v1 List of the given events.
events_file() {
  local name="$1"
  shift
  printf '%s\n' "$@" | jq -s '{apiVersion: "v1", kind: "List", items: .}' >"${tmp_dir}/${name}.json"
  printf '%s\n' "${tmp_dir}/${name}.json"
}

run_check() {
  local file="$1"
  shift
  KUBECTL="${fake_kubectl}" FAKE_EVENTS_FILE="${file}" "$@" bash "${script}" "${digest}" 2>&1
}

expect_pass() {
  local name="$1" file="$2" want="$3" output
  if ! output="$(run_check "${file}" env)"; then
    printf 'FAIL %s: expected a pass, got:\n%s\n' "${name}" "${output}" >&2
    exit 1
  fi
  grep -Fq -- "${want}" <<<"${output}" || {
    printf 'FAIL %s: missing %q in:\n%s\n' "${name}" "${want}" "${output}" >&2
    exit 1
  }
}

expect_fail() {
  local name="$1" file="$2" want="$3" output
  shift 3
  if output="$(run_check "${file}" env "$@")"; then
    printf 'FAIL %s: expected a failure, got a pass:\n%s\n' "${name}" "${output}" >&2
    exit 1
  fi
  grep -Fq -- "${want}" <<<"${output}" || {
    printf 'FAIL %s: missing %q in:\n%s\n' "${name}" "${want}" "${output}" >&2
    exit 1
  }
  printf '%s\n' "${output}"
}

success_apps="$(event apps ReconciliationSucceeded "${digest}" 'Reconciliation finished in 2m5s, next run in 1h0m0s')"
success_infra="$(event infrastructure ReconciliationSucceeded "${digest}" 'HelmRelease/kube-system/cilium configured')"

# 1. A clean reconcile passes and says how many events it judged.
clean="$(events_file clean "${success_infra}" "${success_apps}")"
expect_pass 'clean reconcile' "${clean}" '(2 event(s) checked)'

# 2. A failed prune at this revision fails and names only the object.
pruned="$(events_file pruned "${success_infra}" \
  "$(event apps PruneFailed "${digest}" $'HTTPRoute/apps/whoami delete failed: admission webhook "validate.kyverno.svc" denied the request: secret-detail-xyz')" \
  "${success_apps}")"
output="$(expect_fail 'failed prune' "${pruned}" 'Flux layer apps could not delete HTTPRoute/apps/whoami')"
if grep -Fq 'secret-detail-xyz' <<<"${output}"; then
  printf 'FAIL failed prune: the rest of the Flux message was printed:\n%s\n' "${output}" >&2
  exit 1
fi

# 3. Several objects in one message, including a cluster-scoped one, are all named.
multi="$(events_file multi \
  "$(event infrastructure PruneFailed "${digest}" $'Namespace/retired delete failed: forbidden\nConfigMap/observability/old-dashboard delete failed: forbidden')")"
expect_fail 'several objects' "${multi}" 'could not delete Namespace/retired, ConfigMap/observability/old-dashboard' >/dev/null

# 4. A failed prune from an earlier deploy's revision does not fail this one.
older_failure="$(events_file older \
  "$(event apps PruneFailed "${older}" 'HTTPRoute/apps/whoami delete failed: denied')" "${success_apps}")"
expect_pass 'earlier revision' "${older_failure}" '(1 event(s) checked)'

# 5. A Kustomization outside the platform layers is not judged here.
tenant="$(events_file tenant \
  "$(event wedding-app PruneFailed "${digest}" 'Deployment/wedding-app/web delete failed: denied')" "${success_apps}")"
expect_pass 'tenant layer ignored' "${tenant}" '(1 event(s) checked)'

# 6. A failed prune whose message names no object still fails, without the message.
unnamed="$(events_file unnamed "$(event bootstrap PruneFailed "${digest}" 'prune failed: secret-detail-xyz')")"
output="$(expect_fail 'unnamed object' "${unnamed}" 'its event does not name the object')"
if grep -Fq 'secret-detail-xyz' <<<"${output}"; then
  printf 'FAIL unnamed object: the Flux message was printed:\n%s\n' "${output}" >&2
  exit 1
fi

# 7. No event at this revision means the check cannot see: fail, never pass.
empty="$(events_file empty)"
expect_fail 'empty event list' "${empty}" 'No Flux layer recorded an event' >/dev/null
stale_only="$(events_file stale "$(event apps ReconciliationSucceeded "${older}" 'Reconciliation finished')")"
expect_fail 'only older events' "${stale_only}" 'No Flux layer recorded an event' >/dev/null

# 8. An unreadable or unparsable event list fails.
expect_fail 'kubectl failure' "${clean}" 'Could not read the Flux Kustomization events' FAKE_KUBECTL_FAIL=true >/dev/null
printf 'not json\n' >"${tmp_dir}/garbage.json"
expect_fail 'unparsable events' "${tmp_dir}/garbage.json" 'could not be parsed' >/dev/null

# 9. A malformed digest is a usage error.
if KUBECTL="${fake_kubectl}" bash "${script}" 'latest' >/dev/null 2>&1; then
  printf 'FAIL usage: a malformed digest was accepted\n' >&2
  exit 1
fi

printf 'ok — a deploy fails when Flux could not prune an object at its revision\n'
