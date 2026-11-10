package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// These unbound policies are only part of the retirement contract. No caller
// installs them or deletes a backend. Activation on every serving API server,
// complete dependency inventories, in-flight requests and the remaining
// attachment/instance/dependency fences must be resolved before that is added.
// The backend policy is for the stage after natural Kubernetes-status clearance.
func verifyRetirementReceipts(record benchmarkRecord) error {
	if (record.Phase != "v1" && record.Phase != "v2") || !regexp.MustCompile(`^[1-9][0-9]{0,18}$`).MatchString(record.Run) || record.Namespace != "longhorn-benchmark-"+record.Phase+"-"+record.Run || record.NamespaceUID == "" || record.PendingCreate != "" || record.Cleaned || len(record.Resources) != 7 || validateBenchmarkResources(record) != nil {
		return errors.New("complete original benchmark receipts are required for retirement fences")
	}
	seen := map[string]bool{}
	for _, kind := range []string{"Namespace", "Pod", "NetworkPolicy", "PersistentVolumeClaim", "StorageClass", "PersistentVolume", "LonghornVolume"} {
		identity := record.Resources[kind]
		if !kubeName.MatchString(identity.Name) || len(identity.Name) > 253 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`).MatchString(identity.UID) || seen[identity.UID] {
			return errors.New("retirement fence identities are incomplete or ambiguous")
		}
		seen[identity.UID] = true
	}
	return nil
}

func benchmarkRetirementFences(record benchmarkRecord) ([]object, error) {
	if err := verifyRetirementReceipts(record); err != nil {
		return nil, err
	}
	quote := strconv.Quote
	pv, backend, claim := record.Resources["PersistentVolume"], record.Resources["LonghornVolume"], record.Resources["PersistentVolumeClaim"]
	refers := func(root string) string {
		return "(" + root + ".metadata.name == " + quote(pv.Name) + " || (has(" + root + ".spec.csi) && " + root + ".spec.csi.volumeHandle == " + quote(backend.Name) + "))"
	}
	pvMatch := refers("object") + " || (oldObject != null && " + refers("oldObject") + ")"
	pvInvariant := strings.Join([]string{
		`request.operation == "UPDATE"`,
		"dyn(object.metadata).uid == " + quote(pv.UID),
		"dyn(oldObject.metadata).uid == " + quote(pv.UID),
		"object.metadata.name == " + quote(pv.Name),
		"has(object.spec.csi) && has(oldObject.spec.csi) && object.spec.csi == oldObject.spec.csi",
		"object.spec.csi.volumeHandle == " + quote(backend.Name),
		"has(object.spec.claimRef)",
		"dyn(object.spec.claimRef).uid == " + quote(claim.UID),
		"object.spec.claimRef.name == " + quote(claim.Name),
		"object.spec.claimRef.namespace == " + quote(record.Namespace),
		`object.spec.persistentVolumeReclaimPolicy == "Retain"`,
		"object.spec.storageClassName == " + quote(record.Resources["StorageClass"].Name),
	}, " && ")
	backendMatch := "request.namespace == \"longhorn-system\" && (object.metadata.name == " + quote(backend.Name) + " || (oldObject != null && oldObject.metadata.name == " + quote(backend.Name) + "))"
	backendTerms := []string{
		`request.operation == "UPDATE"`,
		"dyn(object.metadata).uid == " + quote(backend.UID),
		"dyn(oldObject.metadata).uid == " + quote(backend.UID),
		"object.metadata.name == " + quote(backend.Name),
		`(!has(object.metadata.namespace) || object.metadata.namespace == "longhorn-system")`,
		"object.spec == oldObject.spec",
		"object.spec.dataEngine == " + quote(record.Phase),
		"has(object.status.kubernetesStatus)",
	}
	empty := func(path string) string { return "(!has(" + path + ") || " + path + " == \"\")" }
	for _, field := range []string{"nodeID", "engineNodeID", "migrationNodeID", "fromBackup", "dataSource"} {
		backendTerms = append(backendTerms, empty("object.spec."+field))
	}
	backendTerms = append(backendTerms, `(!has(object.spec.Standby) || object.spec.Standby == false)`)
	for _, field := range []string{"currentNodeID", "currentEngineNodeID", "currentMigrationNodeID", "switchoverState"} {
		backendTerms = append(backendTerms, empty("object.status."+field))
	}
	backendTerms = append(backendTerms,
		empty("object.status.kubernetesStatus.pvName"),
		"("+empty("object.status.kubernetesStatus.pvcName")+" || (has(object.status.kubernetesStatus.lastPVCRefAt) && object.status.kubernetesStatus.lastPVCRefAt != \"\"))",
	)
	policy := func(name, group, version, scope string, resources []any, match, invariant string) object {
		return object{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy",
			"metadata": object{"name": name + "-" + record.Phase + "-" + record.Run},
			"spec": object{
				"failurePolicy": "Fail",
				"matchConstraints": object{
					"matchPolicy": "Equivalent", "namespaceSelector": object{}, "objectSelector": object{},
					"resourceRules": []any{object{"apiGroups": []any{group}, "apiVersions": []any{version}, "resources": resources, "operations": []any{"CREATE", "UPDATE"}, "scope": scope}},
				},
				"matchConditions": []any{object{"name": "references-original-benchmark", "expression": match}},
				"validations":     []any{object{"expression": invariant, "reason": "Forbidden", "message": benchmarkReferenceFenceMessage}},
			},
		}
	}
	return []object{
		policy("lh-benchmark-retire-pv", "", "v1", "Cluster", []any{"persistentvolumes", "persistentvolumes/status"}, pvMatch, pvInvariant),
		policy("lh-benchmark-retire-backend", "longhorn.io", "v1beta2", "Namespaced", []any{"volumes", "volumes/status"}, backendMatch, strings.Join(backendTerms, " && ")),
	}, nil
}
