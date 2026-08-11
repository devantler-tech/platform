#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/use-prod-stable-api-endpoint.sh.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly endpoint_script="${root_dir}/scripts/use-prod-stable-api-endpoint.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
mkdir -p "${work_dir}/bin"

cat >"${work_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

url=""
authorization=""
while (($# > 0)); do
  case "$1" in
    -H | --header)
      authorization="$2"
      shift 2
      ;;
    http*)
      url="$1"
      shift
      ;;
    *)
      shift
      ;;
  esac
done

[[ "${url}" == 'https://api.hetzner.cloud/v1/floating_ips?name=prod-floating-ip' ]] || {
  printf 'unexpected URL: %s\n' "${url}" >&2
  exit 90
}
[[ "${authorization}" == 'Authorization: Bearer fixture-hcloud-token' ]] || {
  printf 'missing bearer authorization\n' >&2
  exit 91
}

if [[ "${FAKE_FLOATING_IP_MODE:-owned}" == "foreign" ]]; then
  printf '%s\n' '{"floating_ips":[{"name":"prod-floating-ip","ip":"5.75.215.250","labels":{"ksail.owned":"false","ksail.cluster.name":"prod"}}]}'
else
  printf '%s\n' '{"floating_ips":[{"name":"prod-floating-ip","ip":"5.75.215.250","labels":{"ksail.owned":"true","ksail.cluster.name":"prod"}}]}'
fi
EOF
chmod +x "${work_dir}/bin/curl"

write_node_bound_kubeconfig() {
  local path="$1"
  cat >"${path}" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: prod
    cluster:
      certificate-authority-data: Zml4dHVyZS1jYQ==
      server: https://49.13.53.183:6443
contexts:
  - name: admin@prod
    context:
      cluster: prod
      user: admin@prod
current-context: admin@prod
users:
  - name: admin@prod
    user: {}
EOF
}

server_for_prod() {
  KUBECONFIG="$1" kubectl config view --raw \
    -o jsonpath='{.clusters[?(@.name=="prod")].cluster.server}'
}

kubeconfig="${work_dir}/kubeconfig"
write_node_bound_kubeconfig "${kubeconfig}"

PATH="${work_dir}/bin:${PATH}" \
  KUBECONFIG="${kubeconfig}" \
  HCLOUD_TOKEN="fixture-hcloud-token" \
  "${endpoint_script}" >"${work_dir}/stdout" 2>"${work_dir}/stderr"

[[ "$(server_for_prod "${kubeconfig}")" == "https://5.75.215.250:6443" ]] ||
  fail 'the admin@prod cluster was not switched from the node IP to the floating IP'
grep -Fq '5.75.215.250' "${work_dir}/stdout" ||
  fail 'successful normalization did not report the selected stable endpoint'

write_node_bound_kubeconfig "${kubeconfig}"
if PATH="${work_dir}/bin:${PATH}" \
  KUBECONFIG="${kubeconfig}" \
  HCLOUD_TOKEN="fixture-hcloud-token" \
  FAKE_FLOATING_IP_MODE="foreign" \
  "${endpoint_script}" >"${work_dir}/stdout" 2>"${work_dir}/stderr"; then
  fail 'an ownership-mismatched floating IP was accepted'
fi
[[ "$(server_for_prod "${kubeconfig}")" == "https://49.13.53.183:6443" ]] ||
  fail 'the kubeconfig changed after floating-IP ownership validation failed'
grep -Fqi 'not owned by KSail' "${work_dir}/stderr" ||
  fail 'ownership rejection was not explained'

grep -Fq 'run: ./scripts/use-prod-stable-api-endpoint.sh' "${deploy_action}" ||
  fail 'deploy-prod does not invoke the stable-endpoint normalization'

printf 'ok — prod deploy selects only its KSail-owned stable API endpoint\n'
