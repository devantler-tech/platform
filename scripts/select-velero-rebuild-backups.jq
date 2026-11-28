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

def reviewed_volume_policy:
  .spec.resourcePolicy == null or
  .spec.resourcePolicy == {kind: "configmap", name: "velero-volume-policies"} or
  (.spec.resourcePolicy == {kind: "configmap", name: "velero-daily-volume-policies"} and
   .spec.defaultVolumesToFsBackup == true);

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
  reviewed_volume_policy;

def execution_time:
  if type == "string" then
    try (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) catch null
  else null end;

def before_rebuild($limit):
  (.status.startTimestamp | execution_time) as $started |
  (.status.completionTimestamp | execution_time) as $finished |
  $started != null and $finished != null and $started <= $finished and $finished < $limit;

($cutoff | execution_time) as $limit
| if $limit == null then error("invalid rebuild cutoff") else
    [ .items[] | select(completed and full and before_rebuild($limit)) ]
    | sort_by([(.status.startTimestamp | execution_time), .metadata.name])
    | {full: (last | .metadata.name)}
  end
