#!/usr/bin/env bash
# Behaviour and wiring tests for scripts/use-prod-stable-api-endpoint.sh.
#
# The script runs in public workflow logs, so beyond selecting the endpoint it
# must name no address, and nothing else it read, on any exit. Each case below
# therefore pins BOTH streams line for line: what a case does not list may not
# be printed. Every address is a reserved documentation address (RFC 5737) or an
# `.invalid` name, so this file names no routable host either.
#
# The one line allowed to carry the address is the runner command that registers
# it as a masked value. The cases pin that too: issued on a runner as soon as the
# address is selected, before any later tool runs, on stderr only, and never
# into a file the runner would keep.
#
# The same holds for the addresses of the servers behind the endpoint, which the
# tools that run later in the job print as well: every address of every listed
# server is registered right after the endpoint, all of them or none.

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
readonly mask_line="::add-mask::${floating_ip}"
readonly arc_workflow="${root_dir}/.github/workflows/verify-arc-app-identity.yaml"

readonly switched_line='✅ Production kubeconfig now uses the stable API endpoint (the restored kubeconfig named a different server).'
readonly unchanged_line='✅ Production kubeconfig already uses the stable API endpoint.'
readonly no_context_line='::error::Restored kubeconfig has no usable admin@prod context.'
readonly no_cluster_line='::error::Context admin@prod references a cluster the restored kubeconfig does not define.'
readonly not_owned_line='::error::Hetzner floating IP prod-floating-ip is not owned by KSail for cluster prod; refusing to adopt it.'
readonly not_persisted_line='::error::Failed to persist the stable production API endpoint in the restored kubeconfig.'
readonly servers_unlisted_line='::error::Could not list the production servers from the Hetzner API, so the node addresses cannot be masked.'
readonly servers_invalid_line='::error::Hetzner returned an invalid server list, so the node addresses cannot be masked.'
readonly servers_endless_line='::error::Hetzner listed more server pages than expected, so the node addresses cannot all be masked.'
readonly servers_empty_line='::error::Hetzner listed no server address, so the node addresses cannot be masked.'
readonly servers_odd_address_line='::error::Hetzner returned a server address in an unexpected form, so the node addresses cannot be masked.'

# Every address the fixture's two server pages carry, once each, in the order the script issues
# them: public IPv4, the prefix of a public IPv6 network, private and alias addresses.
readonly node_addresses=(
  '192.0.2.11' '192.0.2.111' '192.0.2.12'
  '198.51.100.31' '198.51.100.32'
  '2001:db8:0:1::' '2001:db8:0:2::'
)

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

printf '%s\n' "${url}" >>"${FAKE_CURL_CALLS}"
[[ "${authorization}" == 'Authorization: Bearer fixture-hcloud-token' ]] || {
  printf 'missing bearer authorization\n' >&2
  exit 91
}

# The server list, one page per call. Every answer carries an address, so a path that echoes its
# input is caught.
readonly servers_url='https://api.hetzner.cloud/v1/servers?per_page=50&page='
if [[ "${url}" == "${servers_url}"* ]]; then
  page="${url#"${servers_url}"}"
  # one_server_page <public IPv4 as JSON> <next page as JSON> — a page holding one server.
  one_server_page() {
    printf '{"servers":[{"public_net":{"ipv4":{"ip":%s},"ipv6":null},"private_net":[]}],"meta":{"pagination":{"next_page":%s}}}\n' \
      "$1" "$2"
  }
  case "${FAKE_SERVERS_MODE:-two-pages}:${page}" in
    # Page 1: a server with every kind of address, and one with a public IPv4 only.
    two-pages:1)
      printf '%s%s%s\n' \
        '{"servers":[{"public_net":{"ipv4":{"ip":"198.51.100.31"},"ipv6":{"ip":"2001:db8:0:1::/64"}},' \
        '"private_net":[{"ip":"192.0.2.11","alias_ips":["192.0.2.111"]}]},' \
        '{"public_net":{"ipv4":{"ip":"198.51.100.32"},"ipv6":null},"private_net":[]}],"meta":{"pagination":{"next_page":2}}}'
      ;;
    # Page 2: a server with no public IPv4, and one repeating an address and lacking private_net.
    two-pages:2)
      printf '%s%s%s\n' \
        '{"servers":[{"public_net":{"ipv4":null,"ipv6":{"ip":"2001:db8:0:2::/64"}},' \
        '"private_net":[{"ip":"192.0.2.12","alias_ips":[]}]},' \
        '{"public_net":{"ipv4":{"ip":"198.51.100.31"}}}],"meta":{"pagination":{"next_page":null}}}'
      ;;
    unreachable:*)
      printf 'curl: (7) Failed to connect to 198.51.100.31' >&2
      exit 7
      ;;
    malformed:*) printf '{"servers":{"ip":"198.51.100.31"}}\n' ;;
    not-json:*) printf 'gateway error naming 198.51.100.31\n' ;;
    no-servers:*) printf '{"servers":[],"meta":{"pagination":{"next_page":null}}}\n' ;;
    no-addresses:*)
      printf '{"servers":[{"name":"198.51.100.31","public_net":{"ipv4":null,"ipv6":null},"private_net":[]}],"meta":{"pagination":{"next_page":null}}}\n'
      ;;
    repeating:*) one_server_page '"198.51.100.31"' "${page}" ;;
    odd-next-page:*) one_server_page '"198.51.100.31"' '"198.51.100.31"' ;;
    endless:*) one_server_page '"198.51.100.31"' "$((page + 1))" ;;
    second-page-unreachable:1) one_server_page '"198.51.100.31"' 2 ;;
    second-page-unreachable:2) exit 7 ;;
    # An address that would start a second runner command of its own.
    command:*) one_server_page '"198.51.100.31\n::add-mask::198.51.100.32"' null ;;
    host-name:*) one_server_page '"node-1.example.invalid"' null ;;
    too-short:*) one_server_page '"1.2"' null ;;
    *)
      printf 'unexpected server list request: %s page %s\n' "${FAKE_SERVERS_MODE:-}" "${page}" >&2
      exit 93
      ;;
  esac
  exit 0
fi

[[ "${url}" == 'https://api.hetzner.cloud/v1/floating_ips?name=prod-floating-ip' ]] || {
  printf 'unexpected URL: %s\n' "${url}" >&2
  exit 90
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
# Whether the runner had already been asked to mask every address when this call was made.
if [[ "$(grep -c '^::add-mask::' "${FAKE_STDERR}")" == "${FAKE_MASK_COUNT}" ]]; then
  printf 'masked\n' >>"${FAKE_KUBECTL_CALLS}.mask"
else
  printf 'unmasked\n' >>"${FAKE_KUBECTL_CALLS}.mask"
fi
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

# The files a runner keeps after the step: nothing the script writes may land in one.
readonly runner_files=(output env state path step-summary)

# run_endpoint <PATH prefix> [VAR=value ...] — the script runs as a runner step. Its stdout and
# stderr land in ${work_dir}, with the mask command taken off stderr into ${masked} (see
# take_mask), and the status returned is the script's own.
run_endpoint() {
  local path_prefix="$1" status=0 name
  shift
  for name in "${runner_files[@]}"; do : >"${work_dir}/runner-${name}"; done
  env PATH="${path_prefix}:${PATH}" \
    GITHUB_ACTIONS=true \
    GITHUB_OUTPUT="${work_dir}/runner-output" \
    GITHUB_ENV="${work_dir}/runner-env" \
    GITHUB_STATE="${work_dir}/runner-state" \
    GITHUB_PATH="${work_dir}/runner-path" \
    GITHUB_STEP_SUMMARY="${work_dir}/runner-step-summary" \
    FAKE_STDERR="${work_dir}/stderr" \
    FAKE_MASK_COUNT="$((${#node_addresses[@]} + 1))" \
    FAKE_CURL_CALLS="${work_dir}/curl-calls" \
    KUBECONFIG="${kubeconfig}" \
    HCLOUD_TOKEN="fixture-hcloud-token" \
    FAKE_FLOATING_IP="${floating_ip}" \
    FAKE_KUBECTL_QUOTES="https://${stale_ip}:6443 ${kubeconfig_token}" \
    FAKE_KUBECTL_CALLS="${work_dir}/kubectl-calls" \
    REAL_KUBECTL="${real_kubectl}" \
    "$@" \
    "${endpoint_script}" >"${work_dir}/stdout" 2>"${work_dir}/stderr" || status=$?
  for name in "${runner_files[@]}"; do
    [[ ! -s "${work_dir}/runner-${name}" ]] ||
      fail "the script wrote to the runner's ${name} file, which outlives the step"
  done
  take_mask
  return "${status}"
}

# take_mask — the mask commands are the only lines allowed to carry an address, and only as the
# leading lines of stderr. Record which were there in ${masked} and remove them, so every check
# after this one reads streams that may name nothing:
#   no        none;
#   endpoint  the endpoint's alone;
#   all       the endpoint's, then one for each address in ${node_addresses}, in that order.
# Any other set fails here, as does a command anywhere else: on stdout a caller that discards
# stdout would drop it, and a partial set or a later position means the addresses were not all
# registered once, up front.
take_mask() {
  local address
  : >"${work_dir}/masks"
  while [[ "$(head -n 1 "${work_dir}/stderr")" == '::add-mask::'* ]]; do
    head -n 1 "${work_dir}/stderr" >>"${work_dir}/masks"
    sed -e '1d' "${work_dir}/stderr" >"${work_dir}/stderr.rest"
    mv "${work_dir}/stderr.rest" "${work_dir}/stderr"
  done
  if grep -q -e '::add-mask::' "${work_dir}/stdout" "${work_dir}/stderr"; then
    fail 'a mask command was printed somewhere other than the leading lines of stderr'
  fi
  printf '%s\n' "${mask_line}" >"${work_dir}/masks.endpoint"
  cp "${work_dir}/masks.endpoint" "${work_dir}/masks.all"
  for address in "${node_addresses[@]}"; do
    printf '::add-mask::%s\n' "${address}" >>"${work_dir}/masks.all"
  done
  if [[ ! -s "${work_dir}/masks" ]]; then
    masked=no
  elif cmp -s "${work_dir}/masks" "${work_dir}/masks.endpoint"; then
    masked=endpoint
  elif cmp -s "${work_dir}/masks" "${work_dir}/masks.all"; then
    masked=all
  else
    fail 'the mask commands issued are neither the endpoint alone nor the endpoint and every server address'
  fi
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
  [[ "${masked}" == all ]] ||
    fail "${what} did not ask the runner to mask the endpoint and every server address"
  [[ "$(server_for_prod "${kubeconfig}")" == "${stable_server}" ]] ||
    fail "${what} did not leave admin@prod on the stable endpoint"
  expect_stream stdout "${line}" || fail "${what} did not print exactly its one outcome line"
  expect_stream stderr '' || fail "${what} printed to stderr"
  if names_what_it_read; then fail "${what} named something it read in the public log"; fi
}

# refuse_with_mask <no|endpoint|all> <what> <stderr line> <PATH prefix> [VAR=value ...] — the run
# must fail, print exactly that line on stderr and nothing on stdout, and leave the kubeconfig as
# it was. The first argument says which mask commands must have been issued by then.
refuse_with_mask() {
  local want_mask="$1" what="$2" line="$3"
  shift 3
  cp "${kubeconfig}" "${work_dir}/kubeconfig.before"
  if run_endpoint "$@"; then
    fail "${what} was accepted"
  fi
  [[ "${masked}" == "${want_mask}" ]] ||
    fail "${what}: mask command issued=${masked}, expected ${want_mask}"
  expect_stream stderr "${line}" || fail "${what} did not print exactly its one explanation"
  expect_stream stdout '' || fail "${what} still printed to stdout"
  cmp -s "${kubeconfig}" "${work_dir}/kubeconfig.before" ||
    fail "the kubeconfig changed although ${what} was refused"
  if names_what_it_read; then fail "${what} named something it read in the public log"; fi
}

# A refusal before the address is selected: there is nothing to mask yet.
expect_refusal() { refuse_with_mask no "$@"; }

# A refusal over the server list: the endpoint is selected and masked, and no server address
# is, because they are registered together or not at all.
expect_refusal_over_servers() { refuse_with_mask endpoint "$@"; }

# A refusal after the addresses are selected: the runner must already have been asked to mask
# them, because the tools that fail from here on are the ones that quote what they were given.
expect_refusal_after_selection() { refuse_with_mask all "$@"; }

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
  "${servers_unlisted_line}" "${servers_invalid_line}" "${servers_endless_line}" \
  "${servers_empty_line}" "${servers_odd_address_line}" >"${work_dir}/stderr"
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

# A mask issued after a tool has run is too late for whatever that tool printed. Every kubectl
# call in that same run noted whether the command was already out.
grep -Fxq 'masked' "${work_dir}/kubectl-calls.mask" ||
  fail 'the recording kubectl never saw the mask command'
if grep -Fxq 'unmasked' "${work_dir}/kubectl-calls.mask"; then
  fail 'kubectl was called before the runner had been asked to mask every address'
fi

# Off a runner nothing reads the commands, so they would only print the addresses to a terminal,
# and the servers are not asked for.
write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
: >"${work_dir}/curl-calls"
run_endpoint "${work_dir}/bin" GITHUB_ACTIONS= || fail 'a run off a runner was refused'
[[ "${masked}" == no ]] || fail 'a run off a runner printed a mask command'
expect_stream stdout "${switched_line}" || fail 'a run off a runner did not print its one outcome line'
expect_stream stderr '' || fail 'a run off a runner printed to stderr'
grep -Fq 'floating_ips' "${work_dir}/curl-calls" || fail 'the recording curl saw no floating IP read'
if grep -Fq '/servers' "${work_dir}/curl-calls"; then
  fail 'a run off a runner asked Hetzner for the server list'
fi

# On a runner the server list is read to its last page, one request per page.
write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443"
: >"${work_dir}/curl-calls"
expect_selection 'a run that reads two server pages' "${switched_line}" "${work_dir}/bin"
[[ "$(grep -c '/servers' "${work_dir}/curl-calls")" == 2 ]] ||
  fail 'the two server pages were not read with one request each'

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

# Every way the server list can fail to give addresses. Each stops the job before a later tool
# can print one, and none leaves some addresses masked and others not.
expect_refusal_over_servers 'an unreachable server list' "${servers_unlisted_line}" \
  "${work_dir}/bin" FAKE_SERVERS_MODE=unreachable
expect_refusal_over_servers 'a server list whose second page is unreachable' "${servers_unlisted_line}" \
  "${work_dir}/bin" FAKE_SERVERS_MODE=second-page-unreachable
for mode in malformed not-json repeating odd-next-page; do
  expect_refusal_over_servers "a server list that is ${mode}" "${servers_invalid_line}" \
    "${work_dir}/bin" FAKE_SERVERS_MODE="${mode}"
done
expect_refusal_over_servers 'a server list that never ends' "${servers_endless_line}" \
  "${work_dir}/bin" FAKE_SERVERS_MODE=endless
for mode in no-servers no-addresses; do
  expect_refusal_over_servers "a server list with ${mode}" "${servers_empty_line}" \
    "${work_dir}/bin" FAKE_SERVERS_MODE="${mode}"
done
for mode in command host-name too-short; do
  expect_refusal_over_servers "a server address that is a ${mode}" "${servers_odd_address_line}" \
    "${work_dir}/bin" FAKE_SERVERS_MODE="${mode}"
done

expect_refusal_after_selection 'a kubectl that fails every read by quoting the kubeconfig' "${no_context_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=loud FAKE_KUBECTL_SUBCOMMAND=view
expect_refusal_after_selection 'a kubectl that refuses the write by quoting the server' "${not_persisted_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=loud FAKE_KUBECTL_SUBCOMMAND=set-cluster
expect_refusal_after_selection 'an endpoint that was never persisted' "${not_persisted_line}" \
  "${work_dir}/kubectl-bin:${work_dir}/bin" FAKE_KUBECTL_MODE=silent FAKE_KUBECTL_SUBCOMMAND=set-cluster

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'someone@prod'
expect_refusal_after_selection 'a kubeconfig without the admin@prod context' "${no_context_line}" "${work_dir}/bin"

write_kubeconfig_without contexts "${kubeconfig}" "https://${stale_ip}:6443"
expect_refusal_after_selection 'a kubeconfig with no contexts at all' "${no_context_line}" "${work_dir}/bin"

write_kubeconfig "${kubeconfig}" "https://${stale_ip}:6443" 'admin@prod' 'retired'
expect_refusal_after_selection 'a context that references an undefined cluster' "${no_cluster_line}" "${work_dir}/bin"

write_kubeconfig_without clusters "${kubeconfig}" "https://${stale_ip}:6443"
expect_refusal_after_selection 'a kubeconfig with no clusters at all' "${no_cluster_line}" "${work_dir}/bin"

grep -Fq 'run: ./scripts/use-prod-stable-api-endpoint.sh' "${deploy_action}" ||
  fail 'deploy-prod does not invoke the stable-endpoint normalization'

# The mask command reaches the runner only through the step's stderr, so a caller that hides
# stderr leaves its job unmasked. There are many ways to spell that, so this does not look for
# them: every line under .github that names the script must be one of the spellings below, and
# anything else fails until it is reviewed and added here.
#
# One caller does hide stderr and is listed rather than changed: an open pull request is
# rewriting its invocation line, and the step after it prints fixed verdict tokens only.
readonly arc_invocation='if ! ./scripts/use-prod-stable-api-endpoint.sh >/dev/null 2>&1; then'
readonly lint_list_entry="scripts/use-prod-stable-api-endpoint.sh \\"
invocations=0
arc_invocations=0
while IFS= read -r mention; do
  file="${mention%%:*}"
  line="${mention#*:}"
  line="${line#"${line%%[![:space:]]*}"}"
  case "${line}" in
    'run: ./scripts/use-prod-stable-api-endpoint.sh' | \
      'run: ./scripts/use-prod-stable-api-endpoint.sh >/dev/null')
      invocations=$((invocations + 1))
      ;;
    "${arc_invocation}")
      [[ "${file}" == "${arc_workflow}" ]] ||
        fail "${file#"${root_dir}/"} hides the endpoint helper's stderr, so the runner never sees the mask command"
      arc_invocations=$((arc_invocations + 1))
      ;;
    # Not invocations: the path filter, the lint list and the test's own name in ci.yaml.
    "- 'scripts/use-prod-stable-api-endpoint.sh'" | "${lint_list_entry}" | \
      *'scripts/tests/test-use-prod-stable-api-endpoint.sh'*) ;;
    *)
      fail "${file#"${root_dir}/"} names the endpoint helper in a form this test has not reviewed: ${line}"
      ;;
  esac
done < <(grep -rF 'use-prod-stable-api-endpoint.sh' "${root_dir}/.github")
((invocations >= 11)) ||
  fail "found only ${invocations} reviewed invocations of the endpoint helper; the wiring check is not reading them all"
((arc_invocations == 1)) ||
  fail "expected the one listed stderr-hiding invocation in the ARC identity workflow, found ${arc_invocations}: update or drop its exception"

printf 'ok — prod deploy selects only its KSail-owned stable API endpoint, masks it and every server address for the rest of the job, and names nothing it read doing it\n'
