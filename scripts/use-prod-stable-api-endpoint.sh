#!/usr/bin/env bash
# Point admin@prod at the KSail-owned Hetzner floating IP before a deployment
# can roll the control-plane node named by a stale KUBE_CONFIG secret.
#
# PUBLIC LOG. This repository's workflow logs are public and this script runs in
# them, so nothing it prints — on stdout or stderr, on success or failure — may
# name the endpoint address or anything the restored kubeconfig carries. Three
# rules keep that true:
#   * on a GitHub runner it registers the address as a masked value the moment
#     it is selected, so the runner redacts it from every later line of the job
#     whichever tool prints it. That one command carries the address to the
#     runner, which redacts it in the log line too. The addresses of the
#     servers behind the endpoint are registered the same way, right after it;
#   * its own lines report outcomes only, never a value it read;
#   * jq's and kubectl's own error text never reaches the log, because both can
#     quote their input — a failed kubectl jsonpath prints the object it was
#     given, which is the whole kubeconfig. Each call discards that text and the
#     script says what failed in its own words.
# scripts/tests/test-use-prod-stable-api-endpoint.sh pins both streams, line for
# line, on every exit.

set -euo pipefail

readonly cluster_name="prod"
readonly kube_context="admin@prod"
readonly floating_ip_name="${cluster_name}-floating-ip"
readonly hcloud_api="https://api.hetzner.cloud/v1/floating_ips?name=${floating_ip_name}"

# Discarding a tool's error text would also hide that the tool is missing.
for tool in curl jq kubectl; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "::error::${tool} is required to select the stable production API endpoint." >&2
    exit 1
  fi
done

if [[ -z "${HCLOUD_TOKEN:-}" ]]; then
  echo "::error::HCLOUD_TOKEN is required to resolve the production API floating IP." >&2
  exit 1
fi

kubeconfig_path="${KUBECONFIG:-${HOME}/.kube/config}"
readonly kubeconfig_path
if [[ ! -f "${kubeconfig_path}" ]]; then
  echo "::error::Kubeconfig ${kubeconfig_path} does not exist." >&2
  exit 1
fi

curl_error_file="$(mktemp)"
readonly curl_error_file
trap 'rm -f "${curl_error_file}"' EXIT
if ! response="$(curl \
  --fail \
  --silent \
  --show-error \
  --retry 3 \
  --retry-all-errors \
  --header "Authorization: Bearer ${HCLOUD_TOKEN}" \
  "${hcloud_api}" 2>"${curl_error_file}")"; then
  curl_error="$(head -c 1000 "${curl_error_file}" | tr '\r\n' '  ')"
  readonly curl_error
  echo "::error::Could not resolve ${floating_ip_name} from the Hetzner API: ${curl_error}" >&2
  exit 1
fi
readonly response

matching_count="$(jq -er \
  --arg name "${floating_ip_name}" \
  '[.floating_ips[]? | select(.name == $name)] | length' \
  <<<"${response}" 2>/dev/null)" || {
  echo "::error::Hetzner returned an invalid response while resolving ${floating_ip_name}." >&2
  exit 1
}
readonly matching_count
if [[ "${matching_count}" != "1" ]]; then
  echo "::error::Expected exactly one Hetzner floating IP named ${floating_ip_name}; found ${matching_count}." >&2
  exit 1
fi

stable_ip="$(jq -er \
  --arg name "${floating_ip_name}" \
  --arg cluster "${cluster_name}" '
    .floating_ips[]
    | select(.name == $name)
    | select(.labels["ksail.owned"] == "true")
    | select(.labels["ksail.cluster.name"] == $cluster)
    | .ip
    | select(type == "string" and length > 0)
  ' <<<"${response}" 2>/dev/null)" || {
  echo "::error::Hetzner floating IP ${floating_ip_name} is not owned by KSail for cluster ${cluster_name}; refusing to adopt it." >&2
  exit 1
}
readonly stable_ip
if [[ ! "${stable_ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  echo "::error::Hetzner floating IP ${floating_ip_name} returned an invalid IPv4 address." >&2
  exit 1
fi

# Tools that run later in the job print the endpoint on their own — the cluster
# update names it on every deploy, and a failed deploy's diagnostic excerpts can
# too — so ask the runner to redact it before anything here or after can use
# it. The command goes to stderr because several callers discard stdout, and it
# goes nowhere else: the runner redacts the value in this line as well, while a
# file, a step summary or an artifact would keep it. Off a runner there is
# nothing to redact and the line would only print the address, so it is skipped.
if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  echo "::add-mask::${stable_ip}" >&2
fi

# The endpoint is not the only address those tools print: the cluster update
# names the node it read its settings from, and each node it writes to (#4612).
# Which node that is changes from deploy to deploy and the autoscaler adds nodes
# no file here lists, so every address of every server the token can see now is
# registered the same way — a server that is not a node costs one unused mask.
# A server created later in the job is not covered: its address is not known
# here. A read that fails, or an answer this cannot take addresses from, stops
# the job here: continuing would publish what this step exists to hide. Off a
# runner nothing would read the commands, so the servers are not asked for.
readonly servers_api="https://api.hetzner.cloud/v1/servers?per_page=50"
readonly servers_page_limit=20
if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  node_addresses=""
  servers_page=1
  while [[ -n "${servers_page}" ]]; do
    if ((servers_page > servers_page_limit)); then
      echo "::error::Hetzner listed more server pages than expected, so the node addresses cannot all be masked." >&2
      exit 1
    fi
    if ! servers_response="$(curl \
      --fail \
      --silent \
      --max-time 30 \
      --retry 3 \
      --retry-all-errors \
      --header "Authorization: Bearer ${HCLOUD_TOKEN}" \
      "${servers_api}&page=${servers_page}" 2>/dev/null)"; then
      echo "::error::Could not list the production servers from the Hetzner API, so the node addresses cannot be masked." >&2
      exit 1
    fi
    # A server may lack a kind of address, but an address or a list that is
    # present in another shape than expected is an answer this cannot take
    # addresses from, as is a page that does not say whether another follows.
    # A public IPv6 address is reported as the server's network; its prefix is
    # registered, which covers an address printed in that same compressed form.
    if ! page_addresses="$(jq -r '
        def address: if type == "string" then . else error("not an address") end;
        def list: if . == null then [] elif type == "array" then . else error("not a list") end;
        if (.servers | type) != "array" then error("no server list") else . end
        | if (.meta.pagination | type) != "object" or (.meta.pagination | has("next_page") | not)
          then error("no pagination") else . end
        | .servers[]
        | if type != "object" then error("not a server") else . end
        | (.public_net.ipv4 | if . == null then empty else .ip | address end),
          (.public_net.ipv6 | if . == null then empty else .ip | address | sub("/[0-9]+$"; "") end),
          (.private_net | list | .[] | (.ip | address), (.alias_ips | list | .[] | address))
      ' <<<"${servers_response}" 2>/dev/null)" ||
      ! next_page="$(jq -r '.meta.pagination.next_page // ""' <<<"${servers_response}" 2>/dev/null)"; then
      echo "::error::Hetzner returned an invalid server list, so the node addresses cannot be masked." >&2
      exit 1
    fi
    if [[ -n "${next_page}" && "${next_page}" != "$((servers_page + 1))" ]]; then
      echo "::error::Hetzner returned an invalid server list, so the node addresses cannot be masked." >&2
      exit 1
    fi
    node_addresses+="${page_addresses}"$'\n'
    servers_page="${next_page}"
  done

  node_addresses="$(LC_ALL=C sort -u <<<"${node_addresses}" | sed -e '/^$/d')"
  readonly node_addresses
  if [[ -z "${node_addresses}" ]]; then
    echo "::error::Hetzner listed no server address, so the node addresses cannot be masked." >&2
    exit 1
  fi
  # Each value becomes part of a runner command, so anything that is not plainly
  # an address is refused rather than passed on.
  while IFS= read -r node_address; do
    if [[ ! "${node_address}" =~ ^[0-9A-Fa-f:.]{7,45}$ ]]; then
      echo "::error::Hetzner returned a server address in an unexpected form, so the node addresses cannot be masked." >&2
      exit 1
    fi
  done <<<"${node_addresses}"
  while IFS= read -r node_address; do
    echo "::add-mask::${node_address}" >&2
  done <<<"${node_addresses}"
fi

# kubeconfig_field <jsonpath> — one field of the restored kubeconfig, or nothing
# when kubectl cannot produce it. Only names and the server are read, so
# credentials stay redacted (no --raw).
kubeconfig_field() {
  kubectl --kubeconfig "${kubeconfig_path}" config view -o jsonpath="$1" 2>/dev/null || true
}

kube_cluster="$(kubeconfig_field "{.contexts[?(@.name==\"${kube_context}\")].context.cluster}")"
readonly kube_cluster
if [[ -z "${kube_cluster}" ]]; then
  echo "::error::Restored kubeconfig has no usable ${kube_context} context." >&2
  exit 1
fi

old_server="$(kubeconfig_field "{.clusters[?(@.name==\"${kube_cluster}\")].cluster.server}")"
readonly old_server
if [[ -z "${old_server}" ]]; then
  echo "::error::Context ${kube_context} references a cluster the restored kubeconfig does not define." >&2
  exit 1
fi

readonly stable_server="https://${stable_ip}:6443"
if kubectl --kubeconfig "${kubeconfig_path}" config set-cluster "${kube_cluster}" \
  --server="${stable_server}" >/dev/null 2>&1; then
  updated_server="$(kubeconfig_field "{.clusters[?(@.name==\"${kube_cluster}\")].cluster.server}")"
else
  updated_server=""
fi
readonly updated_server
if [[ "${updated_server}" != "${stable_server}" ]]; then
  echo "::error::Failed to persist the stable production API endpoint in the restored kubeconfig." >&2
  exit 1
fi

# Whether the restored kubeconfig was stale is the one thing an operator needs
# from this line, and it can be said without naming either server.
if [[ "${old_server}" == "${stable_server}" ]]; then
  echo "✅ Production kubeconfig already uses the stable API endpoint."
else
  echo "✅ Production kubeconfig now uses the stable API endpoint (the restored kubeconfig named a different server)."
fi
