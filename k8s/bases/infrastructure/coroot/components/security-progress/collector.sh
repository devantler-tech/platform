#!/usr/bin/env bash
# A failed collection replaces the previous snapshot with explicit unknowns.
set -uo pipefail
config_dir="${CONFIG_DIR:-/config}"
coverage_check="${COVERAGE_CHECK:-/config/check-kubescape-result-coverage.sh}"
reviewed_list="${REVIEWED_LIST:-/config/kubescape-unscanned-namespaces.tsv}"
metrics_dir="${METRICS_DIR:-/metrics}"
interval="${COLLECTION_INTERVAL_SECONDS:-300}"
case "${interval}" in '' | *[!0-9]* | 0) exit 2 ;; esac
case "${1:-}" in '' | --once) ;; *) exit 2 ;; esac
mkdir -p "${metrics_dir}" || exit 2
# Keep the projected credential in its file. Neither credentials nor kubeconfig
# contents are printed, and only this container receives the token volume.
if [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
  export KUBECONFIG=/tmp/security-progress-kubeconfig
  umask 077
  cat >"${KUBECONFIG}" <<'YAML'
apiVersion: v1
kind: Config
clusters:
  - name: cluster
    cluster:
      server: https://kubernetes.default.svc
      certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
users:
  - name: collector
    user:
      tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
contexts:
  - name: cluster
    context:
      cluster: cluster
      user: collector
current-context: cluster
YAML
fi

collect() (
  set -euo pipefail
  snapshot="$(mktemp -d)"
  trap 'rm -rf "${snapshot}"' EXIT
  now="$(date +%s)"
  # Isolate the two sources. One unavailable API must not hide the other source.
  if kubectl --request-timeout=30s get policyreports.wgpolicyk8s.io -A -o json >"${snapshot}/policy.json" &&
    kubectl --request-timeout=30s get clusterpolicyreports.wgpolicyk8s.io -o json >"${snapshot}/cluster-policy.json" &&
    jq -n --slurpfile policy "${snapshot}/policy.json" --slurpfile cluster "${snapshot}/cluster-policy.json" \
      '{mode:"policy",policy:$policy[0],cluster_policy:$cluster[0]}' |
    jq -er -f "${config_dir}/metrics.jq" >"${snapshot}/policy.prom"; then
    cat "${snapshot}/policy.prom"
    printf 'platform_security_collection_success{surface="policy"} 1\n'
  else
    printf 'platform_security_collection_success{surface="policy"} 0\n'
    printf 'policy collection is unknown\n' >&2
  fi
  printf 'platform_security_snapshot_timestamp_seconds{surface="policy"} %s\n' "${now}"

  mkdir "${snapshot}/kubescape"
  rc=0
  timeout 240 bash "${coverage_check}" --snapshot-dir "${snapshot}/kubescape" --reviewed-list "${reviewed_list}" \
    >"${snapshot}/coverage.log" 2>&1 || rc=$?
  if [ "${rc}" -le 1 ] && (
    cd "${snapshot}/kubescape"
    jq -n --argjson now "$(date +%s)" \
      --slurpfile workloads workloads.json --slurpfile pods pods.json --slurpfile posture posture.json \
      --slurpfile vulnerability vulnerability.json --slurpfile stored vulnerability-stored.json \
      --rawfile report report.txt --rawfile excluded "${reviewed_list}" \
      '{mode:"kubescape",now:$now,workloads:$workloads[0],pods:$pods[0],posture:$posture[0],
        vulnerability:$vulnerability[0],vulnerability_stored:$stored[0],coverage_report:$report,
        unscanned:($excluded | split("\n") | map(select(length > 0 and (startswith("#")|not)) | split("\t")[0]))}' |
      jq -er -f "${config_dir}/metrics.jq" >"${snapshot}/kubescape.prom"
  ); then
    cat "${snapshot}/kubescape.prom"
    printf 'platform_security_collection_success{surface="kubescape"} 1\n'
  else
    printf 'platform_security_collection_success{surface="kubescape"} 0\n'
    printf 'kubescape collection is unknown\n' >&2
  fi
  printf 'platform_security_snapshot_timestamp_seconds{surface="kubescape"} %s\n' "$(date +%s)"
)

while :; do
  # Atomic replacement; a reader can never scrape half a collection. Count
  # series from the old snapshot disappear immediately when their source fails.
  if collect >"${metrics_dir}/.next"; then
    :
  else
    printf 'platform_security_collection_success{surface="policy"} 0\nplatform_security_collection_success{surface="kubescape"} 0\n' \
      >"${metrics_dir}/.next"
  fi
  mv "${metrics_dir}/.next" "${metrics_dir}/metrics" || exit 2
  [ "${1:-}" != --once ] || break
  sleep "${interval}"
done
