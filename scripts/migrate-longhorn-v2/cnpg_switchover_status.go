package main

import (
	"errors"
	"slices"
	"time"
)

// Match the CloudNativePG v1.30.2 promote status transaction and keep every
// unrelated condition. Bind the write to precisely the observed status
// revision, so a concurrent failover or fence stops the request.
func cnpgV2SwitchoverPatch(database cnpgSource, cluster object, previous, target string, now time.Time) ([]object, error) {
	refused := errors.New("the witnessed database primary or promotion target is unverified")
	_, admitted := cnpgStandbyProfileForSource(database)
	_, previousErr := cnpgInstanceID(previous, database.Name)
	_, targetErr := cnpgInstanceID(target, database.Name)
	if !admitted || !recoveredNativeObject(cluster, "postgresql.cnpg.io/v1", "Cluster", database.Namespace) ||
		text(cluster, "metadata", "name") != database.Name || previousErr != nil ||
		targetErr != nil || previous == target || now.IsZero() ||
		text(cluster, "status", "phase") != "Cluster in healthy state" ||
		text(cluster, "status", "currentPrimary") != previous || text(cluster, "status", "targetPrimary") != previous {
		return nil, refused
	}
	if fence := text(cluster, "metadata", "annotations", "cnpg.io/fencedInstances"); fence != "" && fence != "[]" {
		return nil, refused
	}
	conditions, ok := value(cluster, "status", "conditions").([]any)
	if !ok {
		return nil, refused
	}
	updated := slices.Clone(conditions)
	stamp := now.UTC().Format(time.RFC3339Nano)
	readyIndex := -1
	for i, raw := range conditions {
		condition, ok := raw.(object)
		if !ok || text(condition, "type") == "" {
			return nil, refused
		}
		if text(condition, "type") == "Ready" {
			if readyIndex != -1 || text(condition, "status") != "True" {
				return nil, refused
			}
			readyIndex = i
			updated[i] = object{"type": "Ready", "status": "False", "reason": "ClusterIsNotReady",
				"message": "Cluster Is Not Ready", "lastTransitionTime": stamp}
		}
	}
	if readyIndex == -1 {
		return nil, refused
	}
	return []object{
		{"op": "test", "path": "/metadata/uid", "value": text(cluster, "metadata", "uid")},
		{"op": "test", "path": "/metadata/resourceVersion", "value": text(cluster, "metadata", "resourceVersion")},
		{"op": "replace", "path": "/status/targetPrimary", "value": target},
		{"op": "add", "path": "/status/targetPrimaryTimestamp", "value": stamp},
		{"op": "replace", "path": "/status/phase", "value": "Switchover in progress"},
		{"op": "add", "path": "/status/phaseReason", "value": "Switching over to " + target},
		{"op": "replace", "path": "/status/conditions", "value": updated},
	}, nil
}
