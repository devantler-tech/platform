package main

import (
	"errors"
	"maps"
	"sort"
)

type retirementChild struct {
	OwnerReference                                                    object
	Resource                                                          string
	Identity                                                          benchmarkResource
	ResourceVersion, OwnerID, NodeID, EngineName, InstanceManagerName string
}

func falseOrAbsent(v any) bool {
	if v == nil {
		return true
	}
	b, ok := v.(bool)
	return ok && !b
}

// This qualifies declared ownership and stopped CR state only. The caller must
// obtain complete native inventories, pin controller/physical process epochs,
// install and prove the no-start fences, perform non-no-op UID/RV checkpoints,
// and observe the actual instance-manager RPC before retirement is possible.
func qualifyRetirementInstances(record benchmarkRecord, groups map[string][]object) ([]retirementChild, error) {
	if err := verifyRetirementReceipts(record); err != nil {
		return nil, err
	}
	if len(groups) != 4 {
		return nil, errors.New("complete native child inventories are required")
	}
	backend := record.Resources["LonghornVolume"]
	seen := map[string]bool{}
	for _, original := range record.Resources {
		seen[original.UID] = true
	}
	names := map[string]bool{}
	children := []retirementChild{}
	engines := map[string]bool{}
	for _, resource := range []string{"engines", "replicas", "enginefrontends", "volumeattachments"} {
		items, present := groups[resource]
		if !present || items == nil {
			return nil, errors.New("a native child inventory is unread")
		}
		for _, item := range items {
			identity := benchmarkIdentity(item)
			version := text(item, "metadata", "resourceVersion")
			key := resource + "/" + identity.Name
			if identity.Name == "" || identity.UID == "" || identity.Namespace != "longhorn-system" || version == "" || seen[identity.UID] || names[key] {
				return nil, errors.New("native child identities are incomplete or duplicated")
			}
			seen[identity.UID], names[key] = true, true
			association := "volumeName"
			if resource == "volumeattachments" {
				association = "volume"
			}
			volume, valid := value(item, "spec", association).(string)
			if !valid || volume == "" {
				return nil, errors.New("a native child volume association is unknown")
			}
			refs, valid := value(item, "metadata", "ownerReferences").([]any)
			if !valid && value(item, "metadata", "ownerReferences") != nil {
				return nil, errors.New("native child owner references are malformed")
			}
			ownerMatches := false
			for _, raw := range refs {
				ref, valid := raw.(object)
				if !valid || text(ref, "uid") == "" {
					return nil, errors.New("native child owner references are incomplete")
				}
				ownerMatches = ownerMatches || text(ref, "uid") == backend.UID
			}
			if volume != backend.Name && !ownerMatches {
				continue
			}
			if volume != backend.Name || len(refs) != 1 || !ownerMatches || value(item, "metadata", "deletionTimestamp") != nil {
				return nil, errors.New("a backend child has a foreign or changing owner")
			}
			owner := refs[0].(object)
			if text(owner, "apiVersion") != "longhorn.io/v1beta2" || text(owner, "kind") != "Volume" || text(owner, "name") != backend.Name || !falseOrAbsent(owner["controller"]) || !falseOrAbsent(owner["blockOwnerDeletion"]) {
				return nil, errors.New("a backend child differs from the native Volume owner chain")
			}
			child := retirementChild{Resource: resource, Identity: identity, ResourceVersion: version, OwnerReference: maps.Clone(owner)}
			if resource == "volumeattachments" {
				if !absentOrEmptyMap(value(item, "spec", "attachmentTickets")) || !absentOrEmptyMap(value(item, "status", "attachmentTicketStatuses")) {
					return nil, errors.New("backend attachment requests remain")
				}
			} else {
				child.OwnerID = text(item, "status", "ownerID")
				child.NodeID = text(item, "spec", "nodeID")
				child.EngineName = text(item, "spec", "engineName")
				child.InstanceManagerName = text(item, "status", "instanceManagerName")
				if text(item, "spec", "dataEngine") != record.Phase || text(item, "spec", "desireState") != "stopped" || text(item, "status", "currentState") != "stopped" || !falseOrAbsent(value(item, "status", "starting")) || !falseOrAbsent(value(item, "status", "started")) || child.OwnerID == "" || !kubeName.MatchString(child.OwnerID) {
					return nil, errors.New("backend instance ownership or stopped state is unknown")
				}
				if resource == "engines" {
					engines[identity.Name] = true
				}
				if resource == "replicas" && !absentOrEmptyString(value(item, "spec", "linkedCloneSrcReplicaName")) {
					return nil, errors.New("a backend replica has an external linked-clone dependency")
				}
			}
			children = append(children, child)
		}
	}
	for _, child := range children {
		if (child.Resource == "replicas" || child.Resource == "enginefrontends") && !engines[child.EngineName] {
			return nil, errors.New("a backend child does not join an independently qualified engine")
		}
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].Resource != children[j].Resource {
			return children[i].Resource < children[j].Resource
		}
		return children[i].Identity.Name < children[j].Identity.Name
	})
	return children, nil
}
