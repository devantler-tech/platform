package main

import (
	"strconv"
	"strings"
)

// Allow the normal CSI reclaim transition on the original PV while keeping its
// binding immutable. A replacement claim or a second PV for this backend
// cannot enter through the controllers while these two policies are bound.
func nativeRestoreReclaimFences(record benchmarkRecord, seed canarySeedState, state nativeRestoreState) ([]object, error) {
	if state.Phase != "completed" || state.validate(record, seed) != nil {
		return nil, errNativeRestoreEvidence
	}
	pv := state.Resources["PersistentVolume"]
	claim := state.Resources["PersistentVolumeClaim"]
	backend := state.Resources["LonghornVolume"]
	quote := strconv.Quote
	class := quote(state.Resources["StorageClass"].Name)
	workspace := quote(benchmarkNamespace)
	pvRefers := func(root string) string {
		return "(" + root + ".metadata.name == " + quote(pv.Name) + " || (has(" + root + ".spec.storageClassName) && " +
			root + ".spec.storageClassName == " + class + ") || (has(" + root + ".spec.csi) && " +
			root + ".spec.csi.volumeHandle == " + quote(backend.Name) + "))"
	}
	claimRefers := func(root string) string {
		return "((request.namespace == " + workspace + " && " + root + ".metadata.name == " + quote(claim.Name) + ")" +
			" || (has(" + root + ".spec.storageClassName) && " + root + ".spec.storageClassName == " + class +
			") || (has(" + root + ".spec.volumeName) && " + root + ".spec.volumeName == " + quote(pv.Name) + "))"
	}
	pvInvariant := strings.Join([]string{
		`request.operation == "UPDATE"`,
		"dyn(object.metadata).uid == " + quote(pv.UID),
		"dyn(oldObject.metadata).uid == " + quote(pv.UID),
		"object.metadata.name == " + quote(pv.Name),
		"has(object.spec.csi) && has(oldObject.spec.csi) && object.spec.csi == oldObject.spec.csi",
		`object.spec.csi.driver == "driver.longhorn.io"`,
		"object.spec.csi.volumeHandle == " + quote(backend.Name),
		"has(object.spec.claimRef) && has(oldObject.spec.claimRef) && object.spec.claimRef == oldObject.spec.claimRef",
		"dyn(object.spec.claimRef).uid == " + quote(claim.UID),
		"object.spec.claimRef.name == " + quote(claim.Name),
		"object.spec.claimRef.namespace == " + workspace,
		"object.spec.storageClassName == " + class,
		`(object.spec.persistentVolumeReclaimPolicy == "Delete" || (object.spec.persistentVolumeReclaimPolicy == "Retain" && oldObject.spec.persistentVolumeReclaimPolicy == "Retain"))`,
	}, " && ")
	claimInvariant := strings.Join([]string{
		`request.operation == "UPDATE"`,
		"request.namespace == " + workspace,
		"dyn(object.metadata).uid == " + quote(claim.UID),
		"dyn(oldObject.metadata).uid == " + quote(claim.UID),
		"object.metadata.name == " + quote(claim.Name),
		"object.spec == oldObject.spec",
		"object.spec.storageClassName == " + class,
		"object.spec.volumeName == " + quote(pv.Name),
	}, " && ")
	return []object{
		benchmarkReferencePolicy("lh-native-restore-reclaim-pv-"+state.Run, "", "v1", "Cluster",
			[]any{"persistentvolumes", "persistentvolumes/status"}, pvRefers("object")+" || (oldObject != null && "+pvRefers("oldObject")+")", pvInvariant),
		benchmarkReferencePolicy("lh-native-restore-reclaim-pvc-"+state.Run, "", "v1", "Namespaced",
			[]any{"persistentvolumeclaims", "persistentvolumeclaims/status"}, claimRefers("object")+" || (oldObject != null && "+claimRefers("oldObject")+")", claimInvariant),
	}, nil
}
