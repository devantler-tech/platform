package main

import "encoding/json"

// Restore only the archived claim. Velero v1.18 creates the DataDownload
// before applying the claim modifier, and that selector then binds the claim
// to the volume prepared by the data mover. Never run an archived app Pod.
func actualBudgetRestoreAssets(run, backupRun, node string) ([]object, error) {
	if !benchmarkRunPattern.MatchString(run) || !benchmarkRunPattern.MatchString(backupRun) || run == backupRun || !kubeName.MatchString(node) {
		return nil, errActualBudgetBackup
	}
	name := "actual-budget-recovery-" + run
	labels := object{
		"storage.platform.devantler.tech/recovery-run": run,
		"velero.io/exclude-from-backup":                "true",
	}
	claimSelector := object{"matchLabels": object{
		"app.kubernetes.io/name": "actualbudget", "app.kubernetes.io/instance": actualBudgetNamespace,
	}}
	modifiers, err := json.Marshal(object{"version": "v1", "resourceModifierRules": []any{object{
		"conditions": object{
			"groupResource": "persistentvolumeclaims", "resourceNameRegex": "^" + actualBudgetClaim + "$",
			"namespaces": []any{actualBudgetNamespace}, "labelSelector": claimSelector,
		},
		// The protected caller qualifies this node for the restore helper.
		// Velero's WFFC exposer reads selected-node before it creates its
		// scratch claim. The target's Velero binding selector remains intact.
		"patches": []any{
			object{"operation": "replace", "path": "/spec/storageClassName", "value": "longhorn-v2"},
			object{"operation": "add", "path": "/metadata/annotations/volume.kubernetes.io~1selected-node", "value": node},
		},
	}}})
	if err != nil {
		return nil, err
	}
	return []object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": object{"name": name, "labels": object{
			"storage.platform.devantler.tech/recovery-run": run,
			"velero.io/exclude-from-backup":                "true",
			"pod-security.kubernetes.io/enforce":           "restricted",
		}}},
		actualBudgetIsolationPolicy(name, run),
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": name + "-modifiers", "namespace": "velero", "labels": labels},
			"data": object{"modifiers.json": string(modifiers)}},
		{"apiVersion": "velero.io/v1", "kind": "Restore", "metadata": object{"name": name, "namespace": "velero", "labels": labels},
			"spec": object{
				"backupName": "actual-budget-v1-" + backupRun, "includedNamespaces": []any{actualBudgetNamespace},
				"namespaceMapping":  object{actualBudgetNamespace: name},
				"includedResources": []any{"persistentvolumeclaims"}, "excludedResources": []any{"persistentvolumes", "pods"},
				"includeClusterResources": false, "restorePVs": true, "existingResourcePolicy": "none",
				"labelSelector": claimSelector, "itemOperationTimeout": "20m0s",
				"resourceModifier": object{"kind": "ConfigMap", "name": name + "-modifiers"},
			}},
	}, nil
}

// Explicit Cilium denial isolates the proof reader even when the platform
// adds DNS or another allow policy to the namespace. Velero's data mover
// still needs its own network access and is outside this reader selector.
func actualBudgetIsolationPolicy(namespace, run string) object {
	return object{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
		"metadata": object{"name": "restore-isolation", "namespace": namespace, "labels": object{
			"storage.platform.devantler.tech/recovery-run": run,
			"velero.io/exclude-from-backup":                "true",
		}},
		"spec": object{
			"endpointSelector": object{"matchLabels": object{"storage.platform.devantler.tech/recovery-run": run}},
			"ingressDeny":      []any{object{"fromEntities": []any{"all"}}},
			"egressDeny":       []any{object{"toEntities": []any{"all"}}},
		},
	}
}
