package main

import "strconv"

const (
	openBaoCopyFenceOwner    = "storage.platform.devantler.tech/openbao-copy-owner"
	openBaoCopyFenceMessage  = "This OpenBao member is protected during its storage migration"
	openBaoClaimFenceMessage = "This OpenBao claim must use its prepared Longhorn V2 volume"
)

func openBaoCopyFence(run, source, member, executor string) (object, error) {
	if !validBenchmarkOwner(run, source) || !knownOpenBaoMember(member) || executor == "" {
		return nil, errOpenBaoProviderStorage
	}
	owner := run + ":" + source
	copyName := "lh-openbao-copy-" + run
	claim := func(prefix string) string {
		return "has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " + strconv.Quote(prefix+"-"+member)
	}
	data, audit := claim("data"), claim("audit")
	usesOriginal := "(" + data + ") || (" + audit + ")"
	podsWithSource := "(has(object.spec.volumes) && object.spec.volumes.exists(v, " + usesOriginal + "))"
	blocked := "(has(object.metadata.name) && object.metadata.name in [" + strconv.Quote(member) + ", " + strconv.Quote(copyName) + "]) || " + podsWithSource
	accessibleToCopy := "has(object.spec.volumes) && object.spec.volumes.exists_one(v, " + data + ") && " +
		"object.spec.volumes.exists_one(v, " + audit + ") && " +
		"object.spec.volumes.all(v, !(" + usesOriginal + ") || (has(v.persistentVolumeClaim.readOnly) && v.persistentVolumeClaim.readOnly))"
	correctCopy := "has(object.metadata.name) && object.metadata.name == " + strconv.Quote(copyName) +
		" && request.userInfo.username == " + strconv.Quote(executor) +
		" && has(object.metadata.annotations) && " + strconv.Quote(openBaoCopyFenceOwner) + " in object.metadata.annotations" +
		" && object.metadata.annotations[" + strconv.Quote(openBaoCopyFenceOwner) + "] == " + strconv.Quote(owner) +
		" && (" + accessibleToCopy + ")"
	return openBaoAdmissionFence("lh-openbao-storage-"+run, owner, "pods",
		"!("+blocked+") || ("+correctCopy+")", openBaoCopyFenceMessage), nil
}

func openBaoClaimFence(member, executor string, pair [2]openBaoStorageRebind) (object, error) {
	if executor == "" || !openBaoRebindPairMatches(member, pair) {
		return nil, errOpenBaoProviderStorage
	}
	approved := func(intent openBaoStorageRebind) string {
		desired := intent.replacementClaim()
		name := strconv.Quote(intent.Source.PVC.Name)
		ownerKey := strconv.Quote(openBaoVolumeRebindOwner)
		owner := strconv.Quote(text(desired, "metadata", "annotations", openBaoVolumeRebindOwner))
		return "(object.metadata.name == " + name +
			" && has(object.metadata.annotations) && " + ownerKey + " in object.metadata.annotations" +
			" && object.metadata.annotations[" + ownerKey + "] == " + owner +
			" && has(object.spec.volumeName) && object.spec.volumeName == " + strconv.Quote(intent.DestinationPV.Name) + ")"
	}
	affected := "has(object.metadata.name) && object.metadata.name in [" +
		strconv.Quote(pair[0].Source.PVC.Name) + ", " + strconv.Quote(pair[1].Source.PVC.Name) + "]"
	required := "request.userInfo.username == " + strconv.Quote(executor) +
		` && has(object.spec.storageClassName) && object.spec.storageClassName == "longhorn-v2"
		&& has(object.spec.volumeMode) && object.spec.volumeMode == "Filesystem"
		&& object.spec.accessModes == ["ReadWriteOnce"]
		&& !has(object.spec.dataSource) && !has(object.spec.dataSourceRef) && !has(object.spec.selector)`
	run, source := pair[0].Run, pair[0].SourceSHA
	return openBaoAdmissionFence("lh-openbao-claims-"+run, run+":"+source, "persistentvolumeclaims",
		"!("+affected+") || ("+required+" && ("+approved(pair[0])+" || "+approved(pair[1])+"))",
		openBaoClaimFenceMessage), nil
}

func openBaoAdmissionFence(name, owner, resource, expression, message string) object {
	return object{
		"apiVersion": benchmarkAdmissionAPI, "kind": "ValidatingAdmissionPolicy",
		"metadata": object{"name": name, "annotations": object{openBaoCopyFenceOwner: owner}},
		"spec": object{
			"failurePolicy": "Fail",
			"matchConstraints": object{
				"matchPolicy": "Equivalent", "namespaceSelector": object{}, "objectSelector": object{},
				"resourceRules": []any{object{
					"apiGroups": []any{""}, "apiVersions": []any{"v1"}, "operations": []any{"CREATE"},
					"resources": []any{resource}, "scope": "Namespaced",
				}},
			},
			"matchConditions": []any{object{"name": "openbao-namespace", "expression": `request.namespace == "openbao"`}},
			"validations":     []any{object{"reason": "Forbidden", "message": message, "expression": expression}},
		},
	}
}
