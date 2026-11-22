# A completed backup provides evidence only for its declared resource scope.
def completed:
  .metadata.namespace == "velero" and
  .metadata.deletionTimestamp == null and
  (.metadata.name | type == "string" and test("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")) and
  .status.phase == "Completed" and
  (.status.errors // 0) == 0 and
  .spec.storageLocation == "default" and
  (.spec.labelSelector // {}) == {} and
  (.spec.orLabelSelectors // []) == [];

# Velero excludes CSI snapshot objects from the normal resource listing.
def volume_snapshot: "volumesnapshots.snapshot.storage.k8s.io";
def volume_snapshot_content: "volumesnapshotcontents.snapshot.storage.k8s.io";
def only_excludes($approved): ((. // []) - $approved) == [];
def includes_all: (. // []) | . == [] or . == ["*"];

def full:
  (.spec.excludedNamespaces // []) as $excluded |
  .metadata.labels["velero.io/schedule-name"] == "velero-daily-full" and
  .spec.includedNamespaces == ["*"] and
  (["kube-system", "velero"] - $excluded) == [] and
  ($excluded - ["kube-system", "velero", "longhorn-benchmark"]) == [] and
  .spec.includeClusterResources != false and
  (.spec.includedResources | includes_all) and
  (.spec.includedNamespaceScopedResources | includes_all) and
  (.spec.includedClusterScopedResources | includes_all) and
  (.spec.excludedResources | only_excludes([volume_snapshot, volume_snapshot_content])) and
  (.spec.excludedNamespaceScopedResources | only_excludes([volume_snapshot])) and
  (.spec.excludedClusterScopedResources | only_excludes([volume_snapshot_content])) and
  (.spec.resourcePolicy == null or .spec.resourcePolicy == {kind: "configmap", name: "velero-volume-policies"});

def openbao_keys:
  .metadata.labels["velero.io/schedule-name"] == "velero-openbao-recovery-keys" and
  .spec.includedNamespaces == ["openbao"] and
  .spec.includedResources == ["secrets"] and
  # The controller also adds all namespaces labeled exclude-from-backup.
  all((.spec.excludedNamespaces // [])[]; type == "string" and
    test("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$") and . != "openbao") and
  (.spec.excludedResources | only_excludes([volume_snapshot, volume_snapshot_content])) and
  (.spec.includedNamespaceScopedResources // []) == [] and
  (.spec.excludedNamespaceScopedResources // []) == [] and
  (.spec.includedClusterScopedResources // []) == [] and
  (.spec.excludedClusterScopedResources // []) == [] and
  .spec.resourcePolicy == null and
  .spec.includeClusterResources == false and
  (.spec.snapshotVolumes // false) == false and
  (.spec.snapshotMoveData // false) == false and
  (.spec.defaultVolumesToFsBackup // false) == false;

def newest:
  sort_by([(.metadata.creationTimestamp | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601), .metadata.name])
  | last | .metadata.name;

[.items[] | select(completed)] as $completed
| {
    full: ($completed | map(select(full)) | newest),
    openbao: ($completed | map(select(full or openbao_keys)) | newest)
  }
