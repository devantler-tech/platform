#!/usr/bin/env bash

# Safety-critical helpers for refresh-flux-ghcr-auth.sh. Keep these functions
# side-effect free except where their names explicitly describe an operation so
# the production ordering and quorum gates can be exercised with command fakes.

readonly GHCR_PULL_VERIFIED_REVISION_ANNOTATION="platform.devantler.tech/ghcr-pull-verified-revision-v2"
readonly GHCR_PULL_VERIFIED_IMAGE_ANNOTATION="platform.devantler.tech/ghcr-pull-verified-image-v2"

# Select nodes that have not completed the reboot-backed v2 proof for both the
# incoming credential revision and the exact image used for the registry pull.
# Legacy unversioned annotations deliberately do not satisfy this selector: the
# old bridge wrote them without rebooting containerd, so every such node must
# roll once after this contract lands.
select_talos_node_targets() {
  local nodes_file="$1"
  local desired_revision="$2"
  local operator_image="$3"
  local targets_file="$4"
  local unsorted_targets="${targets_file}.unsorted"

  if ! jq -r \
    --arg revision "${desired_revision}" \
    --arg image "${operator_image}" \
    --arg revision_annotation "${GHCR_PULL_VERIFIED_REVISION_ANNOTATION}" \
    --arg image_annotation "${GHCR_PULL_VERIFIED_IMAGE_ANNOTATION}" '
    .items[]
    | select(
        (.metadata.annotations[$revision_annotation] // "") != $revision
        or (.metadata.annotations[$image_annotation] // "") != $image
      )
    | (.metadata.labels // {}) as $labels
    | [
        (if (($labels | has("node-role.kubernetes.io/control-plane"))
          or ($labels | has("node-role.kubernetes.io/master")))
          then "1" else "0" end),
        .metadata.name,
        ([.status.addresses[]
          | select(.type == "InternalIP") | .address][0]),
        (.metadata.annotations[
          "platform.devantler.tech/ghcr-pull-desired-revision"
        ] // "")
      ]
    | @tsv
  ' "${nodes_file}" > "${unsorted_targets}"; then
    rm -f "${unsorted_targets}"
    return 1
  fi

  if ! LC_ALL=C sort -k1,1 -k2,2 \
    "${unsorted_targets}" > "${targets_file}"; then
    rm -f "${unsorted_targets}"
    return 1
  fi
  rm -f "${unsorted_targets}"
}

# Reapply and verify the complete live Kubernetes pull-consumer fanout. Flux and
# External Secrets reconcile independently, so a long Talos roll can overlap an
# hourly controller pass that restores the previous Git value.
sync_and_verify_kubernetes_fanout() {
  local namespace
  local rc

  patch_variables_base || {
    rc=$?
    return "${rc}"
  }
  force_sync_resource pushsecret flux-system seed-ghcr || {
    rc=$?
    return "${rc}"
  }
  for namespace in "$@"; do
    force_sync_resource externalsecret "${namespace}" ghcr-auth || {
      rc=$?
      return "${rc}"
    }
    verify_consumer_secret "${namespace}" || {
      rc=$?
      return "${rc}"
    }
  done
}

# Existing clusters must make every Kubernetes pull consumer safe before the
# first Talos reboot drains application pods, then establish the same proof
# again after the roll. The root Flux Secret remains last so a failed node roll
# or a live-controller race cannot advance reconciliation onto a partial state.
stage_fanout_before_talos() {
  local desired_revision="$1"
  local operator_image="$2"
  local rc
  shift 2

  sync_and_verify_kubernetes_fanout "$@" || {
    rc=$?
    return "${rc}"
  }
  sync_talos_registry_auth "${desired_revision}" "${operator_image}" || {
    rc=$?
    return "${rc}"
  }
  sync_and_verify_kubernetes_fanout "$@" || {
    rc=$?
    return "${rc}"
  }
  patch_root_secret || {
    rc=$?
    return "${rc}"
  }
}

# Prove every OTHER production control-plane member is Kubernetes-Ready,
# reachable through the Talos etcd status RPC, and alarm-free immediately before
# taking one member down. Any inventory, command, or output-shape ambiguity is a
# hard failure: blocking a reboot is safer than guessing about quorum.
other_control_planes_safe_to_reboot() {
  local rebooting="$1"
  local kube_context="$2"
  local state_dir="$3"
  local live_nodes_file="${state_dir}/control-plane-health-${rebooting}.json"
  local peer_file="${state_dir}/control-plane-etcd-peers-${rebooting}.tsv"
  local peer_name
  local peer_ip
  local peer_number=0
  local status_file
  local alarms_file

  if ! kubectl \
    --context "${kube_context}" \
    get nodes \
    --output json \
    > "${live_nodes_file}" 2>/dev/null; then
    echo "Could not re-read control-plane health before rebooting ${rebooting}."
    return 1
  fi

  if ! jq -er --arg rebooting "${rebooting}" '
    [
      .items[]
      | select(.metadata.name != $rebooting)
      | (.metadata.labels // {}) as $labels
      | select(($labels | has("node-role.kubernetes.io/control-plane"))
        or ($labels | has("node-role.kubernetes.io/master")))
      | {
          name: .metadata.name,
          internal_ips: [
            .status.addresses[]?
            | select(.type == "InternalIP")
            | .address
          ],
          ready: any(.status.conditions[]?;
            .type == "Ready" and .status == "True")
        }
    ] as $peers
    | if ($peers | length) < 2 then
        error("fewer than two other control-plane peers")
      elif all($peers[];
        .ready == true
        and (.name | type == "string" and test("^[^\\t\\r\\n]+$"))
        and (.internal_ips | length) == 1
        and (.internal_ips[0]
          | type == "string"
          and test("^[^\\t\\r\\n]+$"))) | not then
        error("a control-plane peer is unready or has an invalid InternalIP")
      elif ([$peers[].internal_ips[0]] | unique | length)
        != ($peers | length) then
        error("control-plane peer InternalIPs are not unique")
      else
        $peers[] | [.name, .internal_ips[0]] | @tsv
      end
  ' "${live_nodes_file}" > "${peer_file}" 2>/dev/null; then
    echo "Could not prove that every other control plane is Ready with a unique InternalIP before rebooting ${rebooting}."
    return 1
  fi

  while IFS=$'\t' read -r peer_name peer_ip; do
    peer_number=$((peer_number + 1))
    status_file="${state_dir}/etcd-status-${rebooting}-${peer_number}.txt"
    alarms_file="${state_dir}/etcd-alarms-${rebooting}-${peer_number}.txt"

    if ! talosctl \
      --nodes "${peer_ip}" \
      etcd status \
      > "${status_file}" 2>&1; then
      echo "Could not read etcd status from control-plane peer ${peer_name}."
      return 1
    fi
    if ! awk -v expected_node="${peer_ip}" '
      NR == 1 { header = ($1 == "NODE" && $2 == "MEMBER") }
      NR > 1 && $1 == expected_node { rows++ }
      END { exit !(header && rows == 1) }
    ' "${status_file}"; then
      echo "Control-plane peer ${peer_name} returned an incomplete etcd status response."
      return 1
    fi

    if ! talosctl \
      --nodes "${peer_ip}" \
      etcd alarm list \
      > "${alarms_file}" 2>&1; then
      echo "Could not read etcd alarms from control-plane peer ${peer_name}."
      return 1
    fi
    if ! awk '
      NF == 0 { next }
      {
        nonempty++
        if (nonempty == 1) {
          header = ($1 == "NODE" && $2 == "MEMBER" && $3 == "ALARM")
          next
        }
        rows++
      }
      END { exit !(header && rows == 0) }
    ' "${alarms_file}"; then
      echo "Control-plane peer ${peer_name} has an etcd alarm or returned an unrecognized alarm response."
      return 1
    fi
  done < "${peer_file}"
}
