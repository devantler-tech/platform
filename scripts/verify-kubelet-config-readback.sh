#!/usr/bin/env bash
set -euo pipefail

# A green machine-config sync does not prove that every running kubelet took
# the settings the repository declares. #3137: two autoscaler nodes kept
# running without the disk eviction thresholds for weeks after the fix was
# deployed, and nothing reported it. This reads each node's live kubelet
# configuration and requires every declared setting to be present in it.
#
# Declared settings are the cluster-wide machine.kubelet.extraConfig patches.
# The comparison is literal and one-directional: a declared key must exist
# live with exactly the declared value, and anything else the kubelet reports
# is ignored. Every setting declared today is a number, a boolean or a string
# the kubelet reports back unchanged. One it rewrites (a duration such as 2m,
# reported as 2m0s) will fail here on its first deploy, visibly, and has to be
# declared in the kubelet's own spelling.
#
# A node that cannot be read is a failure, never a pass: the reproduction in
# #3137 printed nothing when the read was refused, which looked clean.
#
# Exit codes: 0 every node matches; 1 drift or an unreadable node; 2 nothing
# to compare against, or a declaration this check does not model.
root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
talos_dir=${TALOS_CONFIG_DIR:-$root_dir/talos}
kubectl_bin=${KUBECTL_BIN:-kubectl}
# Deployments restore a kubeconfig that contains admin@prod, but its current
# context is not a trusted cluster selector.
kube_context=${KUBE_CONTEXT:-admin@prod}
max_attempts=${KUBELET_READBACK_ATTEMPTS:-30}
interval_seconds=${KUBELET_READBACK_INTERVAL_SECONDS:-10}
max_seconds=${KUBELET_READBACK_MAX_SECONDS:-300}
# false reports a failure as a warning and exits 0. The production deploy
# starts that way until one clean production reading is on record (#3137).
enforce=${KUBELET_READBACK_ENFORCE:-true}

[[ "$enforce" =~ ^(true|false)$ ]] || {
  printf 'invalid KUBELET_READBACK_ENFORCE: expected true or false\n' >&2
  exit 2
}

# Every way this check can end without a pass goes through here, so that
# observe-only really cannot fail a deploy: a red step would skip the steps
# after it, and those reassert production credentials.
give_up() { # <exit code> <message>
  printf '%s\n' "$2" >&2
  if [[ "$enforce" == false ]]; then
    # One annotation, so the finding is visible on a deploy that stays green.
    printf '::warning title=Kubelet settings readback (observe-only)::%s\n' \
      "$(printf '%s' "$2" | tr '\n' ';')"
    exit 0
  fi
  exit "$1"
}

[[ "$max_attempts" =~ ^[1-9][0-9]*$ &&
   "$interval_seconds" =~ ^(0|[1-9][0-9]*)$ &&
   "$max_seconds" =~ ^[1-9][0-9]*$ ]] ||
  give_up 2 'invalid kubelet readback attempt, interval, or deadline setting'

shopt -s nullglob
cluster_patches=("$talos_dir"/cluster/*.yaml)
role_patches=("$talos_dir"/control-planes/*.yaml "$talos_dir"/workers/*.yaml)
shopt -u nullglob

((${#cluster_patches[@]} > 0)) ||
  give_up 2 "no cluster-wide Talos patches found under $talos_dir/cluster"

# A per-role kubelet setting would make "every node" the wrong expectation.
# Refuse it instead of comparing those nodes against half their declaration.
# A file may hold several documents, and a read that fails is not a zero.
for patch in "${role_patches[@]}"; do
  role_settings=$(yq eval -o=json -I=0 '.machine.kubelet.extraConfig // {}' "$patch" 2>/dev/null |
    jq -s 'map(length) | add // 0' 2>/dev/null) ||
    give_up 2 "could not read $patch to rule out per-role kubelet settings"
  [[ "$role_settings" == 0 ]] ||
    give_up 2 "per-role kubelet settings in $patch are not modelled by this check"
done

# shellcheck disable=SC2016  # yq program, not shell
declared=$(yq eval-all -o=json -I=0 \
  '. as $doc ireduce ({}; . * ($doc.machine.kubelet.extraConfig // {}))' \
  "${cluster_patches[@]}" 2>/dev/null) ||
  give_up 2 "could not read the cluster-wide Talos patches under $talos_dir/cluster"
jq -e 'type == "object" and length > 0' <<<"$declared" >/dev/null 2>&1 ||
  give_up 2 "no declared kubelet settings found under $talos_dir/cluster; nothing to compare"

# shellcheck disable=SC2016  # jq program, not shell
compare='
  def covers($want):
    . as $have
    | if ($want | type) == "object" then
        ($have | type) == "object" and
        all($want | to_entries[]; . as $e | ($have | has($e.key)) and ($have[$e.key] | covers($e.value)))
      else $have == $want end;
  (.kubeletconfig // null) as $live
  | if ($live | type) != "object" then ["no kubeletconfig in the reply"]
    else
      [ $declared | to_entries[] | . as $e
        | select(($live | has($e.key) and (.[$e.key] | covers($e.value))) | not)
        | "\($e.key): declared \($e.value | tojson), live \($live[$e.key] | tojson)" ]
      # Without these three the kubelet never evicts on disk pressure, which
      # is the condition #3137 found. They are kubelet defaults, so they are
      # only expected when the declaration asks for the defaults to be kept.
      + ( if $declared.mergeDefaultEvictionSettings == true then
            [ ("nodefs.available", "nodefs.inodesFree", "imagefs.available") as $signal
              | select((($live.evictionHard // {}) | has($signal)) | not)
              | "evictionHard: live has no \($signal) threshold" ]
          else [] end )
    end
  | .[]'

report=''
unread=false
node_count=0
autoscaler_count=0
deadline_epoch=$(( $(date +%s) + max_seconds ))
for ((attempt = 1; attempt <= max_attempts; attempt++)); do
  report=''
  unread=false
  node_count=0
  autoscaler_count=0
  # A node the autoscaler has just registered may not answer yet. Never accept
  # an unread node, but allow a normal replacement to converge within a
  # finite window.
  if names=$("$kubectl_bin" --context "$kube_context" --request-timeout=15s \
    get nodes -o json 2>/dev/null | jq -r '.items | if type == "array" then .[].metadata.name else error("no items") end'); then
    while IFS= read -r node; do
      [[ -n "$node" ]] || continue
      node_count=$((node_count + 1))
      if [[ ! "$node" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]]; then
        report+="unexpected node name; refusing to query it"$'\n'
        continue
      fi
      [[ "$node" == prod-control-plane-* || "$node" == prod-worker-* ]] ||
        autoscaler_count=$((autoscaler_count + 1))
      if ! configz=$("$kubectl_bin" --context "$kube_context" --request-timeout=15s \
        get --raw "/api/v1/nodes/$node/proxy/configz" 2>/dev/null); then
        report+="$node: kubelet configuration could not be read"$'\n'
        unread=true
        continue
      fi
      # jq prints nothing for an empty reply, which would read as no drift.
      if [[ -z "${configz//[[:space:]]/}" ]]; then
        report+="$node: kubelet configuration reply is empty"$'\n'
        unread=true
        continue
      fi
      if ! drift=$(jq -r --argjson declared "$declared" "$compare" <<<"$configz" 2>/dev/null); then
        report+="$node: kubelet configuration reply is not valid JSON"$'\n'
        continue
      fi
      while IFS= read -r line; do
        [[ -z "$line" ]] || report+="$node: $line"$'\n'
      done <<<"$drift"
    done <<<"$names"
    ((node_count > 0)) || report='the cluster reported no nodes'$'\n'
  else
    report="Kubernetes nodes could not be read from context $kube_context"$'\n'
    unread=true
  fi

  if [[ -z "$report" ]]; then
    printf 'PASS: %d node(s), %d of them autoscaler-provisioned, run every declared kubelet setting (%s)\n' \
      "$node_count" "$autoscaler_count" "$(jq -r 'keys | join(", ")' <<<"$declared")"
    exit 0
  fi

  # Only a read that did not complete can improve by waiting. Drift is a
  # finding, and waiting out the window would only delay reporting it.
  if [[ "$unread" == false ]] || ((attempt == max_attempts || $(date +%s) >= deadline_epoch)); then
    break
  fi
  sleep "$interval_seconds"
done

give_up 1 "Kubelet settings readback did not pass after $attempt attempt(s):"$'\n'"${report%$'\n'}"
