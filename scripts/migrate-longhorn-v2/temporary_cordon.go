package main

import "maps"

// The ordinary pool resolver continues rejecting cordoned nodes. A preparation
// may inspect its own cordoned target only after binding the live hardware,
// original boot, node identity and fence to its separately validated journal.
// The analysis view never changes the observed object or sends a mutation.
func temporaryInventoryNodes(nodes []object, record *journal) ([]object, error) {
	if record == nil {
		return nodes, nil
	}
	if record.Schema != 2 || validateJournal(*record, record.Target) != nil || (record.Phase != "evacuating" && record.Phase != "draining") {
		return nil, errInventory
	}
	result := append([]object(nil), nodes...)
	found := false
	for i, node := range nodes {
		if text(node, "metadata", "name") != record.Target {
			continue
		}
		if found || value(node, "spec", "unschedulable") != true || text(node, "metadata", "annotations", fenceAnnotation) != "longhorn-v2-prepare-"+record.TemporarySelection ||
			text(node, "status", "nodeInfo", "bootID") != record.OriginalBootID {
			return nil, errInventory
		}
		if _, err := verifyNode(node, *record, true); err != nil {
			return nil, errInventory
		}
		found = true
		cloned := maps.Clone(node)
		spec := maps.Clone(valueObject(node, "spec"))
		spec["unschedulable"] = false
		cloned["spec"] = spec
		result[i] = cloned
	}
	if !found {
		return nil, errInventory
	}
	return result, nil
}
