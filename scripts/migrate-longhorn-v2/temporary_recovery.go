package main

// resolveTemporaryResetRecovery selects only the original journaled pool member
// after durable reset intent, allowing observation while that member recovers.
func resolveTemporaryResetRecovery(selection string, nodes []object, pools map[string]migrationPool, record journal) (string, error) {
	if (record.Schema != 2 && record.Schema != 3) || record.Phase != "reset-requested" || record.TemporarySelection != selection || validateJournal(record, record.Target) != nil {
		return "", errInventory
	}
	target, _, err := resolveTemporarySelection(selection, nodes, pools, &record)
	if err != nil || target != record.Target {
		return "", errInventory
	}
	return target, nil
}

// This exception selects only the already journaled physical node for observing
// recovery. It proves no readiness and grants no new reset. The state machine's
// reset-requested phase can only inspect post-state, never replay destruction.
func verifyTemporaryResetMember(n object, pool migrationPool, record journal) error {
	role := "v1"
	if record.Schema == 3 {
		role = "v2"
	}
	if pool.role != role || pool.name != "autoscale-longhorn-"+role || text(n, "metadata", "labels", migrationPoolLabel) != pool.role ||
		text(n, "metadata", "labels", "ksail.io/autoscaled") != "true" || text(n, "metadata", "labels", "node.kubernetes.io/instance-type") != pool.serverType || text(n, "metadata", "resourceVersion") == "" {
		return errInventory
	}
	if _, err := verifyNode(n, record, true); err != nil {
		return errInventory
	}
	fence := text(n, "metadata", "annotations", fenceAnnotation)
	if value(n, "spec", "unschedulable") == true {
		if fence != "longhorn-v2-prepare-"+record.TemporarySelection {
			return errInventory
		}
	} else if fence != "" {
		return errInventory
	}
	return nil
}
