#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir
readonly node_agent_config="${root_dir}/k8s/bases/infrastructure/controllers/velero/config-map.yaml"
readonly helm_release="${root_dir}/k8s/bases/infrastructure/controllers/velero/helm-release.yaml"
readonly volume_policy="${root_dir}/k8s/providers/hetzner/infrastructure/controllers/velero/config-map.yaml"
readonly ci_workflow="${root_dir}/.github/workflows/ci.yaml"
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../flux-render.lib.sh
. "${root_dir}/scripts/flux-render.lib.sh"

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v yq >/dev/null 2>&1 || fail 'yq v4 is required to inspect the Velero node-agent configuration'
command -v jq >/dev/null 2>&1 || fail 'jq is required to verify the OpenBao backup schedule'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required to render the production storage contract'

if ! grep -Fq "'scripts/tests/test-velero-data-mover-resources.sh'" "${ci_workflow}" ||
  ! grep -Fq 'bash scripts/tests/test-velero-data-mover-resources.sh' "${ci_workflow}"; then
  fail 'CI must detect and execute the Velero data-mover resource contract'
fi

memory_limit="$(yq -er '.data["node-agent-config.json"] | from_json | .podResources.memoryLimit' "${node_agent_config}")" ||
  fail 'the Velero data-mover memory limit is missing'
readonly memory_limit

[[ "${memory_limit}" == '1Gi' ]] ||
  fail "Velero data-mover pods need a 1Gi memory limit; found ${memory_limit}"

reload_target="$(yq -er '.spec.values.nodeAgent.annotations."configmap.reloader.stakater.com/reload"' "${helm_release}")" ||
  fail 'the Velero node-agent must reload when its external ConfigMap changes'
readonly reload_target

[[ "${reload_target}" == 'node-agent-config' ]] ||
  fail "the Velero node-agent must reload only node-agent-config; found ${reload_target}"

config_payload="$(yq -er '.data["node-agent-config.json"]' "${node_agent_config}")" ||
  fail 'the Velero node-agent configuration payload is missing'
readonly config_payload

if command -v sha256sum >/dev/null 2>&1; then
  config_hash="$(printf '%s' "${config_payload}" | sha256sum | cut -c1-16)"
elif command -v shasum >/dev/null 2>&1; then
  config_hash="$(printf '%s' "${config_payload}" | shasum -a 256 | cut -c1-16)"
else
  fail 'sha256sum or shasum is required to validate the Velero node-agent rollout generation'
fi
readonly config_hash
expected_generation="config-${config_hash}"
readonly expected_generation

actual_generation="$(yq -er '.spec.values.nodeAgent.podLabels."platform.devantler.tech/node-agent-config-generation"' "${helm_release}")" ||
  fail 'the Velero node-agent pod template must carry the current ConfigMap generation'
readonly actual_generation

[[ "${actual_generation}" == "${expected_generation}" ]] ||
  fail "the Velero node-agent rollout generation is stale: expected ${expected_generation}, found ${actual_generation}"

hcloud_skip_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.storageClass[0] == "hcloud" and
      (.conditions.storageClass | length) == 1 and
      .conditions.pvcLabels."backup.platform.devantler.tech/volume-data" == "independently-mirrored" and
      (.conditions.pvcLabels | length) == 1 and
      .action.type == "skip"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly hcloud_skip_rules

[[ "${hcloud_skip_rules}" == '1' ]] ||
  fail "the production Velero policy must contain exactly one labelled hcloud skip rule; found ${hcloud_skip_rules}"

hcloud_skip_rule_count="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .action.type == "skip" and
      ((.conditions.storageClass // []) | contains(["hcloud"]))
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly hcloud_skip_rule_count

[[ "${hcloud_skip_rule_count}" == '1' ]] ||
  fail "the production Velero policy must contain exactly one hcloud skip rule; found ${hcloud_skip_rule_count}"

unsafe_hcloud_skip_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .action.type == "skip" and
      ((.conditions.storageClass // []) | contains(["hcloud"])) and
      .conditions.pvcLabels."backup.platform.devantler.tech/volume-data" != "independently-mirrored"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly unsafe_hcloud_skip_rules

[[ "${unsafe_hcloud_skip_rules}" == '0' ]] ||
  fail "every hcloud skip rule must require the independently mirrored PVC label; found ${unsafe_hcloud_skip_rules} unsafe rule(s)"

rendered_infrastructure="$(mktemp)"
readonly rendered_infrastructure
rendered_controllers="$(mktemp)"
readonly rendered_controllers
rendered_effective="$(mktemp)"
readonly rendered_effective
substitution_variables="$(mktemp)"
readonly substitution_variables
trap 'rm -f "${rendered_infrastructure}" "${rendered_controllers}" "${rendered_effective}" "${substitution_variables}"' EXIT
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure" >"${rendered_infrastructure}" ||
  fail 'the production infrastructure overlay did not render'
kubectl kustomize "${root_dir}/k8s/providers/hetzner/infrastructure/controllers" >"${rendered_controllers}" ||
  fail 'the production infrastructure controllers overlay did not render'

if ! yq -N -o=json -I0 '
  select(.kind == "HelmRelease" and .metadata.namespace == "velero" and .metadata.name == "velero") |
  .spec.values.schedules."openbao-recovery-keys"
' "${rendered_controllers}" | jq -e -s '
  . == [{
    disabled: false,
    schedule: "10 6 * * *",
    useOwnerReferencesInBackup: false,
    template: {
      ttl: "336h",
      storageLocation: "default",
      includedNamespaces: ["openbao"],
      includedResources: ["secrets"],
      includeClusterResources: false,
      snapshotVolumes: false,
      snapshotMoveData: false,
      defaultVolumesToFsBackup: false
    }
  }]
' >/dev/null; then
  fail 'the production OpenBao key backup must use the independent Secrets-only schedule'
fi

rebuild_selector="${root_dir}/scripts/select-velero-rebuild-backups.jq"
rebuild_cutoff='2026-11-22T07:30:00Z'
rebuild_backups="$(
  jq -n '
  def backup($name; $schedule; $created; $start; $stop; $spec):
    {metadata: {name: $name, namespace: "velero", creationTimestamp: $created,
      labels: {"velero.io/schedule-name": $schedule}},
      status: {phase: "Completed", startTimestamp: $start, completionTimestamp: $stop},
      spec: ({storageLocation: "default"} + $spec)};
  def full_spec:
    {includedNamespaces: ["*"], excludedNamespaces: ["kube-system", "velero"],
       excludedNamespaceScopedResources: ["volumesnapshots.snapshot.storage.k8s.io"],
       excludedClusterScopedResources: ["volumesnapshotcontents.snapshot.storage.k8s.io"],
       resourcePolicy: {kind: "configmap", name: "velero-volume-policies"}};
  {items: [
    backup("full-old"; "velero-daily-full"; "2026-11-22T10:09:00Z";
      "2026-11-21T02:17:00Z"; "2026-11-21T03:00:00Z"; full_spec),
    backup("full-1"; "velero-daily-full"; "2026-11-22T10:05:00Z";
      "2026-11-22T02:17:00Z"; "2026-11-22T03:00:00Z"; full_spec),
    backup("keys-1"; "velero-openbao-recovery-keys"; "2026-11-22T10:10:00Z";
      "2026-11-22T06:10:00Z"; "2026-11-22T06:11:00Z"; {includedNamespaces: ["openbao"], includedResources: ["secrets"],
       excludedNamespaces: ["longhorn-benchmark"],
       excludedResources: ["volumesnapshots.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io"],
       includeClusterResources: false, snapshotVolumes: false,
       snapshotMoveData: false, defaultVolumesToFsBackup: false}),
    backup("full-fresh-cluster"; "velero-daily-full"; "2026-11-22T10:20:00Z";
      "2026-11-22T10:20:00Z"; "2026-11-22T10:30:00Z"; full_spec),
    backup("full-missing-time"; "velero-daily-full"; "2026-11-22T10:30:00Z"; null; null; full_spec)
  ]}'
)"
if ! jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" <<<"${rebuild_backups}" |
  jq -e '. == {full: "full-1"}' >/dev/null; then
  fail 'rebuild must select the newest executed full pre-rebuild archive across re-imports'
fi
if ! jq 'del(.items[0]) | .items[0].spec.excludedNamespaces += ["longhorn-benchmark"]' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: "full-1"}' >/dev/null; then
  fail 'rebuild must admit a qualifying full archive with its reviewed exclusions'
fi
if ! jq 'del(.items[0]) | .items[0].spec.includedNamespaces = ["openbao"]' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: null}' >/dev/null; then
  fail 'rebuild must hold without an eligible full archive even when a key archive exists'
fi
if ! jq 'del(.items[0]) | .items[0].status.phase = "PartiallyFailed"' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: null}' >/dev/null; then
  fail 'rebuild must reject incomplete full backups'
fi
if ! jq 'del(.items[0]) | .items[0].spec.excludedClusterScopedResources += ["persistentvolumes"]' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: null}' >/dev/null; then
  fail 'rebuild must reject additional resource exclusions that remove recovery coverage'
fi
if ! jq 'del(.items[0]) | .items[0].spec.resourcePolicy.name = "different-policy"' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: null}' >/dev/null; then
  fail 'rebuild must reject a different full volume policy'
fi
if ! jq 'del(.items[0]) | .items[0].status.completionTimestamp = "2026-11-22T07:31:00Z"' <<<"${rebuild_backups}" |
  jq -e --arg cutoff "${rebuild_cutoff}" -f "${rebuild_selector}" |
  jq -e '. == {full: null}' >/dev/null; then
  fail 'rebuild must reject backups that completed after the cutoff'
fi

vault_config="${root_dir}/k8s/bases/infrastructure/vault-config/job.yaml"
yq -o=json -I=0 '.spec.template.spec.initContainers' "${vault_config}" |
  jq -e 'map(select(.name == "fetch-snapshot" or .name == "vault-init")) |
    length == 2 and all(.[];
    ([.env[]? | select(.name == "DR_SNAPSHOT_NAME" or .name == "DR_SNAPSHOT_SHA256") |
      [.name, .valueFrom.configMapKeyRef.name, .valueFrom.configMapKeyRef.key, .valueFrom.configMapKeyRef.optional]] | sort) ==
    [["DR_SNAPSHOT_NAME", "dr-openbao-snapshot-selection", "name", true],
     ["DR_SNAPSHOT_SHA256", "dr-openbao-snapshot-selection", "sha256", true]])' >/dev/null ||
  fail 'both restore containers must consume the same exact snapshot selection'

# The production validation selects the disposable copy for the immediate
# Longhorn class. Unset and the WFFC production path keep their source class.
production_backup_class="$(yq -er '.data.velero_backup_pvc_storage_class // "longhorn"' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")" ||
  fail 'the production backup storage setting could not be read'
readonly production_backup_class
production_wffc_backup_class="$(yq -er '.data.velero_backup_wffc_pvc_storage_class // "longhorn-wffc"' "${root_dir}/k8s/clusters/prod/bootstrap/config-map.yaml")" ||
  fail 'the production WFFC backup storage setting could not be read'
readonly production_wffc_backup_class

# Both source classes occur in production CSI uploads. Exercise each switch on
# its own as well as together; opting in one must not change the other class.
for mode in unset production longhorn wffc both; do
  : >"${substitution_variables}"
  expected_longhorn='longhorn'
  expected_wffc='longhorn-wffc'
  case "${mode}" in
    production)
      printf 'velero_backup_pvc_storage_class\t%s\nvelero_backup_wffc_pvc_storage_class\t%s\n' \
        "${production_backup_class}" "${production_wffc_backup_class}" >"${substitution_variables}"
      expected_longhorn='longhorn-backup'
      ;;
    longhorn | both)
      printf 'velero_backup_pvc_storage_class\tlonghorn-backup\n' >>"${substitution_variables}"
      expected_longhorn='longhorn-backup'
      ;;
  esac
  if [[ "${mode}" == 'wffc' || "${mode}" == 'both' ]]; then
    printf 'velero_backup_wffc_pvc_storage_class\tlonghorn-backup\n' >>"${substitution_variables}"
    expected_wffc='longhorn-backup'
  fi
  flux_render_substitute "${substitution_variables}" "${rendered_controllers}" >"${rendered_effective}" ||
    fail "the ${mode} backup storage configuration did not substitute"
  actual_longhorn="$(yq -N -er '
    select(.kind == "ConfigMap" and .metadata.namespace == "velero" and .metadata.name == "node-agent-config") |
    .data["node-agent-config.json"] | from_json | .backupPVC.longhorn.storageClass // "longhorn"
  ' "${rendered_effective}")" || fail 'the Longhorn snapshot copy mapping is invalid'
  actual_wffc="$(yq -N -er '
    select(.kind == "ConfigMap" and .metadata.namespace == "velero" and .metadata.name == "node-agent-config") |
    .data["node-agent-config.json"] | from_json | .backupPVC["longhorn-wffc"].storageClass // "longhorn-wffc"
  ' "${rendered_effective}")" || fail 'the WFFC snapshot copy mapping is invalid'
  [[ "${actual_longhorn}" == "${expected_longhorn}" ]] ||
    fail "${mode}: Longhorn copies need ${expected_longhorn}; found ${actual_longhorn}"
  [[ "${actual_wffc}" == "${expected_wffc}" ]] ||
    fail "${mode}: WFFC copies need ${expected_wffc}; found ${actual_wffc}"
done

# Opt in through the same postBuild variable, then follow the effective mapping
# into the deployed StorageClass rather than testing an uncomposed fixture.
printf 'velero_backup_pvc_storage_class\tlonghorn-backup\n' >"${substitution_variables}"
flux_render_substitute "${substitution_variables}" "${rendered_controllers}" >"${rendered_effective}" ||
  fail 'the opted-in backup storage configuration did not substitute'
backup_storage_class="$(yq -N -er '
  select(.kind == "ConfigMap" and .metadata.namespace == "velero" and .metadata.name == "node-agent-config") |
  .data["node-agent-config.json"] | from_json | .backupPVC.longhorn.storageClass
' "${rendered_effective}")" || fail 'Longhorn snapshot uploads need an explicit staging storage class'
export backup_storage_class
readonly backup_storage_class

[[ "${backup_storage_class}" == 'longhorn-backup' ]] ||
  fail 'opted-in snapshot uploads must select the disposable staging class'

backup_storage_contract="$(yq -N -er '
  select(.kind == "StorageClass" and .metadata.name == strenv(backup_storage_class)) |
  .provisioner + "|" + .parameters.numberOfReplicas + "|" + .volumeBindingMode + "|" + .reclaimPolicy + "|" +
  (.metadata.annotations."storageclass.kubernetes.io/is-default-class" // "false")
' "${rendered_controllers}")" || fail 'the mapped snapshot staging class must be deployed with the controllers'
readonly backup_storage_contract

[[ "${backup_storage_contract}" == 'driver.longhorn.io|1|Immediate|Delete|false' ]] ||
  fail "snapshot staging needs one disposable Longhorn replica and immediate binding, never the default class; found ${backup_storage_contract}"

source_replica_count="$(yq -N -er '
  select(.kind == "StorageClass" and .metadata.name == "longhorn-wffc") | .parameters.numberOfReplicas
' "${rendered_controllers}")" || fail 'the durable source storage class must still render'
readonly source_replica_count
[[ "${source_replica_count}" == '3' ]] || fail 'source-volume redundancy must remain at three replicas'

hcloud_pvcs="$(
  yq -N -r '
    select(.kind == "PersistentVolumeClaim" and .spec.storageClassName == "hcloud") |
    .metadata.namespace + "/" + .metadata.name
  ' "${rendered_infrastructure}" | sort
)" || fail 'the production hcloud PVC inventory could not be read'
readonly hcloud_pvcs

# The retained original and active replacement snapshot caches use the same
# independently mirrored route. Other hcloud data must retain filesystem backup.
expected_hcloud_pvcs=$'openbao/vault-snapshots\nopenbao/vault-snapshots-recovery'
readonly expected_hcloud_pvcs

[[ "${hcloud_pvcs}" == "${expected_hcloud_pvcs}" ]] ||
  fail "the explicit production hcloud PVC inventory changed; found: ${hcloud_pvcs:-none}"

independently_mirrored_pvcs="$(
  yq -r '
    select(
      .kind == "PersistentVolumeClaim" and
      .metadata.labels."backup.platform.devantler.tech/volume-data" == "independently-mirrored"
    ) |
    .metadata.namespace + "/" + .metadata.name
  ' "${rendered_infrastructure}" | sed '/^---$/d' | sort
)" || fail 'the independently mirrored PVC inventory could not be read'
readonly independently_mirrored_pvcs

[[ "${independently_mirrored_pvcs}" == "${expected_hcloud_pvcs}" ]] ||
  fail "only the retained original and active replacement snapshot caches must use independent mirrors; found: ${independently_mirrored_pvcs:-none}"

controller_hcloud_claims="$({
  yq -r '
    select(.kind == "HelmRelease" and .metadata.name == "openbao") |
    [
      {"consumer": "openbao/data", "storageClass": .spec.values.server.dataStorage.storageClass},
      {"consumer": "openbao/audit", "storageClass": .spec.values.server.auditStorage.storageClass}
    ] | .[] | select(.storageClass == "hcloud") | .consumer
  ' "${rendered_controllers}"
  yq -r '
    select(.kind == "Coroot" and .metadata.name == "coroot") |
    [
      {"consumer": "coroot/server", "storageClass": .spec.storage.className},
      {"consumer": "coroot/prometheus", "storageClass": .spec.prometheus.storage.className},
      {"consumer": "coroot/clickhouse", "storageClass": .spec.clickhouse.storage.className},
      {"consumer": "coroot/keeper", "storageClass": .spec.clickhouse.keeper.storage.className}
    ] | .[] | select(.storageClass == "hcloud") | .consumer
  ' "${rendered_infrastructure}"
} | sed '/^---$/d' | sort)" || fail 'the controller-created hcloud PVC inventory could not be read'
readonly controller_hcloud_claims

expected_controller_hcloud_claims=$'coroot/clickhouse\ncoroot/keeper\ncoroot/prometheus\ncoroot/server\nopenbao/audit\nopenbao/data'
readonly expected_controller_hcloud_claims

[[ "${controller_hcloud_claims}" == "${expected_controller_hcloud_claims}" ]] ||
  fail "the controller-created hcloud PVC inventory changed; found: ${controller_hcloud_claims:-none}"

# Controller-created claims have no independently-mirrored marker in their
# declarations. Because the skip policy now requires that marker, these six
# consumers fall through to defaultVolumesToFsBackup instead of losing data.
controller_opt_outs="$({
  yq -r '
    select(.kind == "HelmRelease" and .metadata.name == "openbao") |
    .. | select(
      tag == "!!map" and
      ."backup.platform.devantler.tech/volume-data" == "independently-mirrored"
    ) | ."backup.platform.devantler.tech/volume-data"
  ' "${rendered_controllers}"
  yq -r '
    select(.kind == "Coroot" and .metadata.name == "coroot") |
    .. | select(
      tag == "!!map" and
      ."backup.platform.devantler.tech/volume-data" == "independently-mirrored"
    ) | ."backup.platform.devantler.tech/volume-data"
  ' "${rendered_infrastructure}"
} | sed '/^---$/d')" || fail 'the controller-created PVC opt-out inventory could not be read'
readonly controller_opt_outs

[[ -z "${controller_opt_outs}" ]] ||
  fail 'a controller-created hcloud PVC declaration must not opt out of Velero volume-data backup'

longhorn_snapshot_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.storageClass[0] == "longhorn" and
      (.conditions.storageClass | length) == 1 and
      .action.type == "snapshot"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly longhorn_snapshot_rules

[[ "${longhorn_snapshot_rules}" == '1' ]] ||
  fail "the production Velero policy must retain exactly one Longhorn snapshot rule; found ${longhorn_snapshot_rules}"

empty_dir_skip_rules="$(yq -er '
  .data["policy.yaml"] | from_yaml |
  [.volumePolicies[] |
    select(
      .conditions.volumeTypes[0] == "emptyDir" and
      (.conditions.volumeTypes | length) == 1 and
      .action.type == "skip"
    )
  ] | length
' "${volume_policy}")" || fail 'the production Velero volume policy is invalid'
readonly empty_dir_skip_rules

[[ "${empty_dir_skip_rules}" == '1' ]] ||
  fail "the production Velero policy must retain exactly one emptyDir skip rule; found ${empty_dir_skip_rules}"

printf 'Velero data-mover resource contract is valid.\n'
