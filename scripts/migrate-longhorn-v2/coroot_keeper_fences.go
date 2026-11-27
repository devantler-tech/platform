package main

import "strconv"

const (
	corootKeeperFenceOwner        = "storage.platform.devantler.tech/coroot-keeper-fence-owner"
	corootKeeperPodFenceMessage   = "This Keeper member is temporarily fenced during its storage migration"
	corootKeeperClaimFenceMessage = "This Keeper claim must bind only to its prepared Longhorn V2 destination"
)

func corootKeeperAdmissionFence(name, owner, resource, expression, message string) object {
	return object{
		"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy",
		"metadata": object{"name": name, "annotations": object{corootKeeperFenceOwner: owner}},
		"spec": object{
			"failurePolicy": "Fail",
			"matchConstraints": object{
				"matchPolicy": "Equivalent", "namespaceSelector": object{}, "objectSelector": object{},
				"resourceRules": []any{object{
					"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE"},
					"resources": []any{resource}, "scope": "Namespaced",
				}},
			},
			"matchConditions": []any{object{"name": "observability-namespace", "expression": "request.namespace == " + strconv.Quote(corootKeeperNamespace)}},
			"validations":     []any{object{"message": message, "expression": expression, "reason": "Forbidden"}},
		},
	}
}

func corootKeeperPodFence(name, owner, pod, claim, copyPod, executor string) object {
	mount := "has(object.spec.volumes) && object.spec.volumes.exists(v, has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " +
		strconv.Quote(claim) + ")"
	isOriginal := "has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " + strconv.Quote(claim)
	allowedMount := "has(object.spec.volumes) && object.spec.volumes.exists_one(v, " + isOriginal +
		") && object.spec.volumes.all(v, !(" + isOriginal + ") || (has(v.persistentVolumeClaim.readOnly) && v.persistentVolumeClaim.readOnly))"
	copyMatches := "has(object.metadata.name) && object.metadata.name == " + strconv.Quote(copyPod) +
		" && request.userInfo.username == " + strconv.Quote(executor) + " && (" + allowedMount + ")"
	blocked := "(has(object.metadata.name) && object.metadata.name == " + strconv.Quote(pod) + ") || (" + mount + ")"
	return corootKeeperAdmissionFence(name, owner, "pods", "!("+blocked+") || ("+copyMatches+")", corootKeeperPodFenceMessage)
}

func corootKeeperClaimFence(name, owner, claim, targetPV string) object {
	approved := `has(object.spec.storageClassName) && object.spec.storageClassName == "longhorn-v2" && ` +
		"has(object.spec.volumeName) && object.spec.volumeName == " + strconv.Quote(targetPV) +
		` && object.spec.accessModes == ["ReadWriteOnce"] && object.spec.volumeMode == "Filesystem" && ` +
		`!has(object.spec.dataSource) && !has(object.spec.dataSourceRef)`
	affected := "has(object.metadata.name) && object.metadata.name == " + strconv.Quote(claim)
	return corootKeeperAdmissionFence(name, owner, "persistentvolumeclaims", "!("+affected+") || ("+approved+")", corootKeeperClaimFenceMessage)
}
