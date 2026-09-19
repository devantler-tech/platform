#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/wait-for-prod-api-stability.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

fake_kubectl="${tmp_dir}/kubectl"
state_file="${tmp_dir}/attempts"

cat >"${fake_kubectl}" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

[[ "$*" == "--context admin@prod get --raw=/readyz --request-timeout=5s" ]] || {
  printf 'unexpected kubectl arguments: %s\n' "$*" >&2
  exit 91
}

attempt=0
if [[ -f "${FAKE_STATE_FILE}" ]]; then
  attempt="$(<"${FAKE_STATE_FILE}")"
fi
attempt=$((attempt + 1))
printf '%s\n' "${attempt}" >"${FAKE_STATE_FILE}"

if [[ "${FAKE_ALWAYS_FAIL:-false}" == "true" || "${attempt}" == 3 ]]; then
  printf 'The connection to the server was refused\n' >&2
  exit 1
fi

printf 'ok\n'
EOF
chmod +x "${fake_kubectl}"

output="$({
  PROD_API_KUBECTL_BIN="${fake_kubectl}" \
    PROD_API_STABILITY_ATTEMPTS=6 \
    PROD_API_STABILITY_CONSECUTIVE_SUCCESSES=3 \
    PROD_API_STABILITY_INTERVAL=0 \
    FAKE_STATE_FILE="${state_file}" \
    bash "${script}"
} 2>&1)"

[[ "$(<"${state_file}")" == 6 ]] || {
  printf 'expected six readiness probes after the stability reset, got %s\n' "$(<"${state_file}")" >&2
  exit 1
}
grep -Fq 'Production Kubernetes API remained ready for 3 consecutive probes.' <<<"${output}" || {
  printf 'missing stable-readiness success: %s\n' "${output}" >&2
  exit 1
}

rm -f "${state_file}"
if failure_output="$({
  PROD_API_KUBECTL_BIN="${fake_kubectl}" \
    PROD_API_STABILITY_ATTEMPTS=3 \
    PROD_API_STABILITY_CONSECUTIVE_SUCCESSES=2 \
    PROD_API_STABILITY_INTERVAL=0 \
    FAKE_ALWAYS_FAIL=true \
    FAKE_STATE_FILE="${state_file}" \
    bash "${script}"
} 2>&1)"; then
  printf 'persistent API unavailability unexpectedly passed\n' >&2
  exit 1
fi
grep -Fq 'Production Kubernetes API did not reach a stable ready window' <<<"${failure_output}" || {
  printf 'missing bounded-failure diagnostic: %s\n' "${failure_output}" >&2
  exit 1
}

printf 'ok — post-update production API readiness requires a stable window\n'
