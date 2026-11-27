package longhorninventory

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// V2ReplicaQuorum requires three running replicas of the exact volume on
// distinct nodes and disks.
func V2ReplicaQuorum(volume map[string]any, replicas []map[string]any) bool {
	backend := unstructured.Unstructured{Object: volume}
	name, uid := backend.GetName(), backend.GetUID()
	if name == "" || uid == "" || replicas == nil {
		return false
	}
	nodes, disks, identities := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, replica := range replicas {
		rawSpec, _, _ := unstructured.NestedFieldNoCopy(replica, "spec")
		spec, _ := rawSpec.(map[string]any)
		if spec["volumeName"] != name {
			continue
		}
		resource := unstructured.Unstructured{Object: replica}
		owners := resource.GetOwnerReferences()
		node, _ := spec["nodeID"].(string)
		disk, _ := spec["diskID"].(string)
		id := string(resource.GetUID())
		state, _, _ := unstructured.NestedString(replica, "status", "currentState")
		deleted, _, _ := unstructured.NestedFieldNoCopy(replica, "metadata", "deletionTimestamp")
		healthyAt, _ := spec["healthyAt"].(string)
		failedAt, _ := spec["failedAt"].(string)
		if len(owners) != 1 || owners[0].APIVersion != "longhorn.io/v1beta2" || owners[0].Kind != "Volume" ||
			owners[0].Name != name || owners[0].UID != uid || resource.GetNamespace() != "longhorn-system" ||
			spec["dataEngine"] != "v2" || healthyAt == "" || failedAt != "" || state != "running" || deleted != nil ||
			node == "" || disk == "" || id == "" || nodes[node] || disks[disk] || identities[id] {
			return false
		}
		nodes[node], disks[disk], identities[id] = true, true, true
	}
	return len(nodes) == 3
}
