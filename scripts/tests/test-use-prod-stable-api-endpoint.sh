#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/use-prod-stable-api-endpoint.sh.
#
# The script runs in public workflow logs, so beyond selecting the endpoint it
# must name no address, and nothing else it read, on any exit. Each case below
# therefore pins BOTH streams line for line: what a case does not list may not
# be printed. Every address is a reserved documentation address (RFC 5737) or an
# `.invalid` name, so this file names no routable host either.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly endpoint_script="${root_dir}/scripts/use-prod-stable-api-endpoint.sh"
readonly deploy_action="${root_dir}/.github/actions/deploy-prod/action.yml"

readonly floating_ip='203.0.113.10'
readonly stale_ip='198.51.100.20'
readonly stale_host='stale-control-plane.example.invalid'
readonly kubeconfig_token='fixture-kubeconfig-credential'
readonly stable_server="https://${floating_ip}:6443"

readonly switched_line='✅ Production kubeconfig now uses the stable API endpoint (the restored kubeconfig named a different server).'
readonly unchanged_line='✅ Production kubeconfig already uses the stable API endpoint.'
readonly no_context_line='::error::Restored kubeconfig has no usable admin@prod context.'
readonly no_cluster_line='::error::Context admin@prod references a cluster the restored kubeconfig does not define.'
readonly not_owned_line='::error::Hetzner floating IP prod-floating-ip is not owned by KSail for cluster prod; refusing to adopt it.'
readonly not_persisted_line='::error::Failed to persist the stable production API endpoint in the restored kubeconfig.'

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

# The real tools, resolved before any case narrows PATH.
real_bash="$(command -v bash)" || fail 'bash is required on PATH'
real_jq="$(command -v jq)" || fail 'jq is required on PATH'
real_kubectl="$(command -v kubectl)" || fail 'kubectl is required on PATH'
readonly real_bash real_jq real_kubectl

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
mkdir -p "${work_dir}/bin" "${work_dir}/kubectl-bin"

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

# floating_ip_object <ip> <ksail.owned> <ksail.cluster.name> — one Hetzner floating-IP object.
floating_ip_object() {
  printf '{"name":"prod-floating-ip","ip":"%s","labels":{"ksail.owned":"%s","ksail.cluster.name":"%s"}}' \
    "$1" "$2" "$3"
}

# Every answer but the empty one carries the address, so a path that echoes its input is caught.
owned="$(floating_ip_object "${FAKE_FLOATING_IP}" true prod)"
case "${FAKE_FLOATING_IP_MODE:-owned}" in
  owned) printf '{"floating_ips":[%s]}\n' "${owned}" ;;
  duplicate) printf '{"floating_ips":[%s,%s]}\n' "${owned}" "${owned}" ;;
  absent) printf '{"floating_ips":[]}\n' ;;
  foreign) printf '{"floating_ips":[%s]}\n' "$(floating_ip_object "${FAKE_FLOATING_IP}" false prod)" ;;
  other-cluster) printf '{"floating_ips":[%s]}\n' "$(floating_ip_object "${FAKE_FLOATING_IP}" true staging)" ;;
  not-ipv4) printf '{"floating_ips":[%s]}\n' "$(floating_ip_object "${FAKE_FLOATING_IP}/32" true prod)" ;;
  malformed) printf '{"floating_ips":[["%s"]]}\n' "${FAKE_FLOATING_IP}" ;;
  unreachable)
    printf 'curl: (7) Failed to connect to the Hetzner API' >&2
    exit 7
    ;;
  *)
    printf 'unexpected mode: %s\n' "${FAKE_FLOATING_IP_MODE}" >&2
    exit 92
    ;;
esac
EOF
chmod +x "${work_dir}/bin/curl"

# A kubectl that records how it was called, misbehaves on one subcommand and is the real one
# otherwise:
#   silent <subcommand>  — reports success and does nothing;
#   loud <subcommand>    — fails the way kubectl does, quoting what it was given.
cat >"${work_dir}/kubectl-bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${FAKE_KUBECTL_CALLS}"
for arg in "$@"; do
  if [[ "${arg}" == "${FAKE_KUBECTL_SUBCOMMAND}" ]]; then
    if [[ "${FAKE_KUBECTL_MODE}" == "loud" ]]; then
      printf 'error: object given to the engine was: %s %s\n' "${FAKE_KUBECTL_QUOTES}" "$*" >&2
      exit 1
    fi
    exit 0
  fi
done
exec "${REAL_KUBECTL}" "$@"
EOF
chmod +x "${work_dir}/kubectl-bin/kubectl"

# path_without <tool> — a directory holding only what the script needs, minus that tool.
path_without() {
  local missing="$1" dir="${work_dir}/without-$1"
  mkdir -p "${dir}"
  ln -s "${real_bash}" "${dir}/bash"
  if [[ "${missing}" != "curl" ]]; then ln -s "${work_dir}/bin/curl" "${dir}/curl"; fi
  if [[ "${missing}" != "jq" ]]; then ln -s "${real_jq}" "${dir}/jq"; fi
  if [[ "${missing}" != "kubectl" ]]; then ln -s "${real_kubectl}" "${dir}/kubectl"; fi
  printf '%s\n' "${dir}"
}

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
    user:
      token: ${kubeconfig_token}
EOF
}

# write_kubeconfig_without <contexts|clusters> <path> <server> — real kubectl fails a query for
# the missing list by printing the object it was given.
write_kubeconfig_without() {
  write_kubeconfig "$2" "$3"
  case "$1" in
    contexts) sed -e '/^contexts:$/,/^current-context:/d' "$2" >"$2.tmp" ;;
    clusters) sed -e '/^clusters:$/,/^      server:/d' "$2" >"$2.tmp" ;;
    *) fail "write_kubeconfig_without: unknown list $1" ;;
  esac
  mv "$2.tmp" "$2"
  if grep -q "^$1:" "$2"; then
    fail "the kubeconfig fixture still has its $1 list"
  fi
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
    FAKE_KUBECTL_QUOTES="https://${stale_ip}:6443 ${kubeconfig_token}" \
    FAKE_KUBECTL_CALLS="${work_dir}/kubectl-calls" \
    REAL_KUBECTL="${real_kubectl}" \
    "$@" \
    "${endpoint_script}" >"${work_dir}/stdout" 2>"${work_dir}/stderr"
}

# expect_stream <stdout|stderr> <line> — true when the last run's stream is exactly that one
# line, or exactly nothing when the line is empty.
expect_stream() {
  if [[ -n "$2" ]]; then
    printf '%s\n' "$2" >"${work_dir}/expected"
  else
    : >"${work_dir}/expected"
  fi
  cmp -s "${work_dir}/$1" "${work_dir}/expected"
}

# True when the last run's output names something the script read: anything shaped like an IPv4
# address, any URL, the stale control-plane name or the kubeconfig's credential. The exact-line
# checks already exclude these; this one also catches an allowed line edited to carry one.
names_what_it_read() {
  grep -Eq -e '([0-9]{1,3}\.){3}[0-9]{1,3}' -e '://' \
    "${work_dir}/stdout" "${work_dir}/stderr" && return 0
  grep -Fq -e "${stale_host}" -e "${kubeconfig_token}" "${work_dir}/stdout" "${work_dir}/stderr"
}

# expect_selection <what> <stdout line> <PATH prefix> [VAR=value ...] — the run must succeed,
# leave admin@prod on the stable endpoint, print exactly that line and nothing on stderr.
expect_selection() {
  local what="$1" line="$2"
  shift 2
  run_endpoint "$@" || fail "${what} was refused"
  [[ "$(server_for_prod "${kubeconfig}")" == "${stable_server}" ]] ||
    fail "${what} did not leave admin@prod on the stable endpoint"
  expect_stream stdout "${line}" || fail "${what} did not print exactly its one outcome line"
  expect_stream stderr '' || fail "${what} printed to stderr"
  if names_what_it_read; then fail "${what} named something it read in the public log"; fi
}

# expect_refusal <what> <stderr line> <PATH prefix> [VAR=value ...] — the run must fail, print
# exactly that line on stderr and nothing on stdout, and leave the kubeconfig as it was.
expect_refusal() {
  local what="$1" line="$2"
  shift 2
  cp "${kubeconfig}" "${work_dir}/kubeconfig.before"
  if run_endpoint "$@"; then
    fail "${what} was accepted"
  fi
  expect_stream stderr "${line}" || fail "${what} did not print exactly its one explanation"
  expect_stream stdout '' || fail "${what} still printed to stdout"
  cmp -s "${kubeconfig}" "${work_dir}/kubeconfig.before" ||
    fail "the kubeconfig changed although ${what} was refused"
  if names_what_it_read; then fail "${what} named something it read in the public log"; fi
}

# A detector that cannot fire proves nothing, so show it firing on each shape, on each stream,
# and staying silent on the lines the script may print, before relying on it.
for sample in "endpoint ${floating_ip}" "was https://${stale_host}:6443" "${stale_host}" "${kubeconfig_token}"; do
  printf '%s\n' "${sample}" >"${work_dir}/stdout"
  : >"${work_dir}/stderr"
  names_what_it_read || fail "the detector missed on stdout: ${sample}"
  : >"${work_dir}/stdout"
  printf '%s\n' "${sample}" >"${work_dir}/stderr"
  names_what_it_read || fail "the detector missed on stderr: ${sample}"
done
printf '%s\n' "${switched_line}" "${unchanged_line}" >"${work_dir}/stdout"
printf '%s\n' "${no_context_line}" "${no_cluster_line}" "${not_owned_line}" "${not_persisted_line}" \
  >"${work_dir}/stderr"
if names_what_it_read; then fail 'the detector fired on lines that name nothing'; fi

kubeconfig="${work_dir}/kubeconfig"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
expect_selection 'a kubeconfig naming a replaced node address' "${switched_line}" "${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "https://${stale_host}:6443"
expect_selection 'a kubeconfig naming a replaced node host' "${switched_line}" "${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "${stable_server}"
expect_selection 'a kubeconfig already on the stable endpoint' "${unchanged_line}" "${work_dir}/bin"

# Only names and the server are read, so no call may ask kubectl for unredacted credentials.
write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
: >"${work_dir}/kubectl-calls"
expect_selection 'a run through the recording kubectl' "${switched_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=silent FAKE_KUBECTL_SUBCOMMAND=none
grep -Fq -- 'config view' "${work_dir}/kubectl-calls" ||
  fail 'the recording kubectl saw no kubeconfig read'
if grep -Fq -- '--raw' "${work_dir}/kubectl-calls"; then
  fail 'the script asked kubectl for unredacted credentials'
fi

# Every way the script can refuse, in the order it checks them.
write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
for tool in curl jq kubectl; do
  expect_refusal "a runner without ${tool}" \
    "::error::${tool} is required to select the stable production API endpoint." \
    "${work_dir}/bin" PATH="$(path_without "${tool}")"
done
expect_refusal 'a missing HCLOUD_TOKEN' \
  '::error::HCLOUD_TOKEN is required to resolve the production API floating IP.' \
  "${work_dir}/bin" HCLOUD_TOKEN=
expect_refusal 'a missing kubeconfig' \
  "::error::Kubeconfig ${work_dir}/absent does not exist." \
  "${work_dir}/bin" KUBECONFIG="${work_dir}/absent"
expect_refusal 'an unreachable Hetzner API' \
  '::error::Could not resolve prod-floating-ip from the Hetzner API: curl: (7) Failed to connect to the Hetzner API' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=unreachable
expect_refusal 'a malformed Hetzner answer' \
  '::error::Hetzner returned an invalid response while resolving prod-floating-ip.' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=malformed
expect_refusal 'no floating IP by that name' \
  '::error::Expected exactly one Hetzner floating IP named prod-floating-ip; found 0.' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=absent
expect_refusal 'a duplicated floating IP' \
  '::error::Expected exactly one Hetzner floating IP named prod-floating-ip; found 2.' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=duplicate
expect_refusal 'a floating IP KSail does not own' "${not_owned_line}" \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=foreign
expect_refusal 'a floating IP owned for another cluster' "${not_owned_line}" \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=other-cluster
expect_refusal 'a floating IP that is not an IPv4 address' \
  '::error::Hetzner floating IP prod-floating-ip returned an invalid IPv4 address.' \
  "${work_dir}/bin" FAKE_FLOATING_IP_MODE=not-ipv4
expect_refusal 'a kubectl that fails every read by quoting the kubeconfig' "${no_context_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=loud FAKE_KUBECTL_SUBCOMMAND=view
expect_refusal 'a kubectl that refuses the write by quoting the server' "${not_persisted_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=loud FAKE_KUBECTL_SUBCOMMAND=set-cluster
expect_refusal 'an endpoint that was never persisted' "${not_persisted_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=silent FAKE_KUBECTL_SUBCOMMAND=set-cluster

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'someone@prod'
expect_refusal 'a kubeconfig without the admin@prod context' "${no_context_line}" "${work_dir}/bin"

write_kubeconfig_without contexts "${kubeconfig}" "https://${stale_ip}:6443"
expect_refusal 'a kubeconfig with no contexts at all' "${no_context_line}" "${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'admin@prod' 'retired'
expect_refusal 'a context that references an undefined cluster' "${no_cluster_line}" "${work_dir}/bin"

write_kubeconfig_without clusters "${kubeconfig}" "https://${stale_ip}:6443"
expect_refusal 'a kubeconfig with no clusters at all' "${no_cluster_line}" "${work_dir}/bin"

grep -Fq 'run: ./scripts/use-prod-stable-api-endpoint.sh' "${deploy_action}" ||
  fail 'deploy-prod does not invoke the stable-endpoint normalization'

printf 'ok — prod deploy selects only its KSail-owned stable API endpoint, and names nothing it read doing it\n'
