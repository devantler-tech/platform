#!/usr/bin/env bash
# Point admin@prod at the KSail-owned Hetzner floating IP before a deployment
# can roll the control-plane node named by a stale KUBE_CONFIG secret.

set -euo pipefail

readonly cluster_name="prod"
readonly kube_context="admin@prod"
readonly floating_ip_name="${cluster_name}-floating-ip"
readonly hcloud_api="https://api.hetzner.cloud/v1/floating_ips?name=${floating_ip_name}"

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
  <<<"${response}")" || {
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
  ' <<<"${response}")" || {
  echo "::error::Hetzner floating IP ${floating_ip_name} is not owned by KSail for cluster ${cluster_name}; refusing to adopt it." >&2
  exit 1
}
readonly stable_ip
if [[ ! "${stable_ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
  echo "::error::Hetzner floating IP ${floating_ip_name} returned an invalid IPv4 address." >&2
  exit 1
fi

kube_cluster="$(kubectl --kubeconfig "${kubeconfig_path}" config view --raw \
  -o jsonpath="{.contexts[?(@.name==\"${kube_context}\")].context.cluster}")"
readonly kube_cluster
if [[ -z "${kube_cluster}" ]]; then
  echo "::error::Restored kubeconfig has no ${kube_context} context." >&2
  exit 1
fi

old_server="$(kubectl --kubeconfig "${kubeconfig_path}" config view --raw \
  -o jsonpath="{.clusters[?(@.name==\"${kube_cluster}\")].cluster.server}")"
readonly old_server
if [[ -z "${old_server}" ]]; then
  echo "::error::Context ${kube_context} references missing cluster ${kube_cluster}." >&2
  exit 1
fi

readonly stable_server="https://${stable_ip}:6443"
kubectl --kubeconfig "${kubeconfig_path}" config set-cluster "${kube_cluster}" \
  --server="${stable_server}" >/dev/null

updated_server="$(kubectl --kubeconfig "${kubeconfig_path}" config view --raw \
  -o jsonpath="{.clusters[?(@.name==\"${kube_cluster}\")].cluster.server}")"
readonly updated_server
if [[ "${updated_server}" != "${stable_server}" ]]; then
  echo "::error::Failed to persist production API endpoint ${stable_server} in the restored kubeconfig." >&2
  exit 1
fi

echo "✅ Production kubeconfig now uses the stable API endpoint ${stable_ip} (was ${old_server})."
