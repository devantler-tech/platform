#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/use-prod-stable-api-endpoint.sh.
#
# The script runs in public workflow logs, so beyond selecting the endpoint it
# must name no address on any path. Every address below is a reserved
# documentation address (RFC 5737) or an `.invalid` name, so this file names no
# routable host either.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly endpoint_script="${root_dir}/scripts/use-prod-stable-api-endpoint.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"

readonly floating_ip='203.0.113.10'
readonly stale_ip='198.51.100.20'
readonly stale_host='stale-control-plane.example.invalid'
readonly stable_server="https://${floating_ip}:6443"
readonly switched_line='✅ Production kubeconfig now uses the stable API endpoint (the restored kubeconfig named a different server).'
readonly unchanged_line='✅ Production kubeconfig already uses the stable API endpoint.'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

real_kubectl="$(command -v kubectl)" || fail 'kubectl is required on PATH'
readonly real_kubectl

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
mkdir -p "${work_dir}/bin" "${work_dir}/no-persist-bin"

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

# floating_ip <ip> <ksail.owned> — one Hetzner floating-IP object.
floating_ip() {
  printf '{"name":"prod-floating-ip","ip":"%s","labels":{"ksail.owned":"%s","ksail.cluster.name":"prod"}}' "$1" "$2"
}

# Every answer carries the address, so a path that echoes its input is caught.
case "${FAKE_FLOATING_IP_MODE:-owned}" in
  owned) printf '{"floating_ips":[%s]}\n' "$(floating_ip "${FAKE_FLOATING_IP}" true)" ;;
  foreign) printf '{"floating_ips":[%s]}\n' "$(floating_ip "${FAKE_FLOATING_IP}" false)" ;;
  duplicate)
    printf '{"floating_ips":[%s,%s]}\n' \
      "$(floating_ip "${FAKE_FLOATING_IP}" true)" "$(floating_ip "${FAKE_FLOATING_IP}" true)"
    ;;
  malformed) printf '{"floating_ips":[["%s"]]}\n' "${FAKE_FLOATING_IP}" ;;
  not-ipv4) printf '{"floating_ips":[%s]}\n' "$(floating_ip "${FAKE_FLOATING_IP}/32" true)" ;;
  unreachable)
    printf 'curl: (7) Failed to connect to the Hetzner API\n' >&2
    exit 7
    ;;
  *)
    printf 'unexpected mode: %s\n' "${FAKE_FLOATING_IP_MODE}" >&2
    exit 92
    ;;
esac
EOF
chmod +x "${work_dir}/bin/curl"

# Accepts `config set-cluster` and writes nothing, so the script's read-back
# still sees the server it tried to replace.
cat >"${work_dir}/no-persist-bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

for arg in "$@"; do
  if [[ "${arg}" == "set-cluster" ]]; then
    exit 0
  fi
done
exec "${REAL_KUBECTL}" "$@"
EOF
chmod +x "${work_dir}/no-persist-bin/kubectl"

# write_kubeconfig <path> <server> [context name] [cluster the context references]
write_kubeconfig() {
  cat >"$1" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: prod
    cluster:
      certificate-authority-data: Zml4dHVyZS1jYQ==
      server: $2
contexts:
  - name: ${3:-admin@prod}
    context:
      cluster: ${4:-prod}
      user: admin@prod
current-context: ${3:-admin@prod}
users:
  - name: admin@prod
    user: {}
EOF
}

server_for_prod() {
  KUBECONFIG="$1" kubectl config view --raw \
    -o jsonpath='{.clusters[?(@.name=="prod")].cluster.server}'
}

# run_endpoint <PATH prefix> [VAR=value ...] — the run's stdout and stderr land in ${work_dir}.
run_endpoint() {
  local path_prefix="$1"
  shift
  env PATH="${path_prefix}:${PATH}" \
    KUBECONFIG="${kubeconfig}" \
    HCLOUD_TOKEN="fixture-hcloud-token" \
    FAKE_FLOATING_IP="${floating_ip}" \
    REAL_KUBECTL="${real_kubectl}" \
    "$@" \
    "${endpoint_script}" >"${work_dir}/stdout" 2>"${work_dir}/stderr"
}

# True when the last run's output names a server: anything shaped like an IPv4
# address, any URL, or the stale control-plane name.
names_an_address() {
  grep -Eq -e '([0-9]{1,3}\.){3}[0-9]{1,3}' -e '://' \
    "${work_dir}/stdout" "${work_dir}/stderr" && return 0
  grep -Fq -e "${stale_host}" "${work_dir}/stdout" "${work_dir}/stderr"
}

expect_no_address() {
  if names_an_address; then
    fail "$1 named an address in the public log"
  fi
}

# expect_refusal <what> <explanation> <PATH prefix> [VAR=value ...] — the run must fail, explain
# itself on stderr, print nothing on stdout, leave the kubeconfig as it was and name no address.
expect_refusal() {
  local what="$1" explanation="$2"
  shift 2
  cp "${kubeconfig}" "${work_dir}/kubeconfig.before"
  if run_endpoint "$@"; then
    fail "${what} was accepted"
  fi
  grep -Fq -- "${explanation}" "${work_dir}/stderr" ||
    fail "${what} was not explained"
  [[ ! -s "${work_dir}/stdout" ]] ||
    fail "${what} still printed a success line"
  cmp -s "${kubeconfig}" "${work_dir}/kubeconfig.before" ||
    fail "the kubeconfig changed although ${what} was refused"
  expect_no_address "refusing ${what}"
}

# A detector that cannot fire proves nothing, so show it firing on each shape,
# on each stream, before relying on its silence.
for sample in "endpoint ${floating_ip}" "was https://${stale_host}:6443" "${stale_host}"; do
  printf '%s\n' "${sample}" >"${work_dir}/stdout"
  : >"${work_dir}/stderr"
  names_an_address || fail "the address detector missed on stdout: ${sample}"
  : >"${work_dir}/stdout"
  printf '%s\n' "${sample}" >"${work_dir}/stderr"
  names_an_address || fail "the address detector missed on stderr: ${sample}"
done
printf '%s\n' "${switched_line}" "${unchanged_line}" >"${work_dir}/stdout"
: >"${work_dir}/stderr"
expect_no_address 'the detector, given only address-free lines,'

kubeconfig="${work_dir}/kubeconfig"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
run_endpoint "${work_dir}/bin" ||
  fail 'a KSail-owned floating IP was rejected'
[[ "$(server_for_prod "${kubeconfig}")" == "${stable_server}" ]] ||
  fail 'the admin@prod cluster was not switched from the node IP to the floating IP'
[[ "$(<"${work_dir}/stdout")" == "${switched_line}" ]] ||
  fail 'switching a stale kubeconfig was not reported as a switch, and as nothing else'
expect_no_address 'switching from a stale node address'

write_kubeconfig "${kubeconfig}" "https://${stale_host}:6443"
run_endpoint "${work_dir}/bin" ||
  fail 'a kubeconfig naming a stale control-plane host was rejected'
[[ "$(server_for_prod "${kubeconfig}")" == "${stable_server}" ]] ||
  fail 'the admin@prod cluster was not switched from the node name to the floating IP'
[[ "$(<"${work_dir}/stdout")" == "${switched_line}" ]] ||
  fail 'switching from a stale node name was not reported as a switch, and as nothing else'
expect_no_address 'switching from a stale node name'

write_kubeconfig "${kubeconfig}" "${stable_server}"
run_endpoint "${work_dir}/bin" ||
  fail 'a kubeconfig already on the stable endpoint was rejected'
[[ "$(server_for_prod "${kubeconfig}")" == "${stable_server}" ]] ||
  fail 'a kubeconfig already on the stable endpoint was moved off it'
[[ "$(<"${work_dir}/stdout")" == "${unchanged_line}" ]] ||
  fail 'an already-stable kubeconfig was not reported as unchanged, and as nothing else'
expect_no_address 'confirming an already-stable kubeconfig'

# Every way the script can refuse, in the order it checks them.
write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
expect_refusal 'a missing HCLOUD_TOKEN' 'HCLOUD_TOKEN is required' \
  "${work_dir}/bin" HCLOUD_TOKEN=
expect_refusal 'a missing kubeconfig' 'does not exist' \
  "${work_dir}/bin" KUBECONFIG="${work_dir}/absent"
expect_refusal 'an unreachable Hetzner API' 'Could not resolve prod-floating-ip from the Hetzner API' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=unreachable
expect_refusal 'a malformed Hetzner answer' 'Hetzner returned an invalid response' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=malformed
expect_refusal 'a duplicated floating IP' 'Expected exactly one Hetzner floating IP' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=duplicate
expect_refusal 'an ownership-mismatched floating IP' 'not owned by KSail' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=foreign
expect_refusal 'a floating IP that is not an IPv4 address' 'returned an invalid IPv4 address' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=not-ipv4
expect_refusal 'an endpoint that was never persisted' 'Failed to persist the stable production API endpoint' \
  "${work_dir}/no-persist-bin:${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'someone@prod'
expect_refusal 'a kubeconfig without the admin@prod context' 'has no admin@prod context' \
  "${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'admin@prod' 'retired'
expect_refusal 'a context that references a missing cluster' 'references missing cluster retired' \
  "${work_dir}/bin"

grep -Fq 'run: ./scripts/use-prod-stable-api-endpoint.sh' "${deploy_action}" ||
  fail 'deploy-prod does not invoke the stable-endpoint normalization'

printf 'ok — prod deploy selects only its KSail-owned stable API endpoint, and names no address doing it\n'
