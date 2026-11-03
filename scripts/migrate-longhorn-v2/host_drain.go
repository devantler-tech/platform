package main

import "context"

const hostApplicationDrainSelector = "--pod-selector=longhorn.io/component!=instance-manager"

// drainRuntime proves that the selector excludes only the target's ready V1
// instance-manager Pod. Longhorn 1.13 does not recreate this Pod while cordoned;
// retaining it until the journaled reboot preserves the three-copy precondition.
// Every application Pod remains subject to the normal eviction and PDB checks.
func (h hostMemoryReboot) drainRuntime(ctx context.Context, s inventory) error {
	if err := h.drainPolicy(ctx); err != nil {
		return err
	}
	return h.excludedRuntime(ctx, s)
}

// drainPolicy binds every maintenance stage to the reviewed production policy.
func (h hostMemoryReboot) drainPolicy(ctx context.Context) error {
	policy, err := h.c.get(ctx, "settings.longhorn.io", "longhorn-system", "node-drain-policy", false)
	if err != nil || text(policy, "metadata", "name") != "node-drain-policy" || text(policy, "metadata", "namespace") != "longhorn-system" || text(policy, "value") != "block-if-contains-last-replica" {
		return errHostMemory
	}
	return nil
}

// excludedRuntime refuses an unrelated, replaced or unready Pod carrying the
// selector label, so application workloads cannot escape eviction by that label.
func (h hostMemoryReboot) excludedRuntime(ctx context.Context, s inventory) error {
	managers, err := h.c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil {
		return errHostMemory
	}
	byName, err := indexed(managers)
	if err != nil {
		return errHostMemory
	}
	var target object
	for _, manager := range byName {
		if text(manager, "spec", "nodeID") != h.target {
			continue
		}
		if target != nil || text(manager, "spec", "dataEngine") != "v1" || text(manager, "status", "currentState") != "running" || text(manager, "metadata", "namespace") != "longhorn-system" || text(manager, "metadata", "uid") == "" || text(manager, "metadata", "resourceVersion") == "" || value(manager, "metadata", "deletionTimestamp") != nil {
			return errHostMemory
		}
		target = manager
	}
	if target == nil {
		return errHostMemory
	}
	excluded := 0
	for _, pod := range s.pods {
		if text(pod, "spec", "nodeName") != h.target || text(pod, "metadata", "labels", "longhorn.io/component") != "instance-manager" {
			continue
		}
		excluded++
		owners, ok := value(pod, "metadata", "ownerReferences").([]any)
		if !ok || len(owners) != 1 || text(pod, "metadata", "namespace") != "longhorn-system" || text(pod, "metadata", "name") != text(target, "metadata", "name") || text(pod, "metadata", "uid") == "" || text(pod, "metadata", "resourceVersion") == "" || value(pod, "metadata", "deletionTimestamp") != nil || text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) {
			return errHostMemory
		}
		owner, ok := owners[0].(object)
		if !ok || text(owner, "apiVersion") != "longhorn.io/v1beta2" || text(owner, "kind") != "InstanceManager" || text(owner, "name") != text(target, "metadata", "name") || text(owner, "uid") != text(target, "metadata", "uid") || value(owner, "controller") != true {
			return errHostMemory
		}
	}
	if excluded != 1 {
		return errHostMemory
	}
	return nil
}
