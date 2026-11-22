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

def full:
  (.spec.excludedNamespaces // []) as $excluded |
  .metadata.labels["velero.io/schedule-name"] == "velero-daily-full" and
  .spec.includedNamespaces == ["*"] and
  (["kube-system", "velero"] - $excluded) == [] and
  ($excluded - ["kube-system", "velero", "longhorn-benchmark"]) == [] and
  ((.spec.includedResources // []) | . == [] or . == ["*"]) and
  ((.spec.includedNamespaceScopedResources // []) | . == [] or . == ["*"]) and
  ((.spec.excludedResources // []) | length) == 0 and
  ((.spec.excludedNamespaceScopedResources // []) - ["volumesnapshots.snapshot.storage.k8s.io"] | length) == 0;

def openbao_keys:
  .metadata.labels["velero.io/schedule-name"] == "velero-openbao-recovery-keys" and
  .spec.includedNamespaces == ["openbao"] and
  .spec.includedResources == ["secrets"] and
  (.spec.excludedNamespaces // []) == [] and
  (.spec.excludedResources // []) == [] and
  (.spec.includedNamespaceScopedResources // []) == [] and
  (.spec.excludedNamespaceScopedResources // []) == [] and
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
