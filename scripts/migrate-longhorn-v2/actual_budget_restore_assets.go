package main

import "encoding/json"

// Restore only the archived claim. Velero v1.18 creates the DataDownload
// before applying the claim modifier, and that selector then binds the claim
// to the volume prepared by the data mover. Never run an archived app Pod.
func actualBudgetRestoreAssets(run, backupRun string) ([]object, error) {
	if !benchmarkRunPattern.MatchString(run) || !benchmarkRunPattern.MatchString(backupRun) || run == backupRun {
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
		"patches": []any{object{"op": "replace", "path": "/spec/storageClassName", "value": "longhorn-v2"}},
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
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": object{"name": "restore-isolation", "namespace": name, "labels": labels},
			"spec":     object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}},
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
