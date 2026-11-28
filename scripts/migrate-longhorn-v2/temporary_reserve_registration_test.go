package main

import "testing"

func TestRetainedReserveRegistrationIsAdditiveAndFenced(t *testing.T) {
	storage := object{
		"metadata": object{"uid": "worker-storage", "resourceVersion": "11", "annotations": object{temporaryActivationOwner: "activation"}},
		"spec":     object{"disks": blockDisksFor(temporaryRawID(temporaryReserveSelection))},
	}
	changes := retainedReserveRegistrationChanges(storage, "reserve-journal")
	if len(changes) != 2 ||
		text(changes[0], "op") != "add" || text(changes[0], "path") != "/metadata/annotations/storage.platform.devantler.tech~1longhorn-v2-retained-reserve" ||
		text(changes[0], "value") != "reserve-journal" ||
		text(changes[1], "op") != "add" || text(changes[1], "path") != "/spec/disks/"+temporaryReserveDiskName ||
		value(changes[1], "value", "allowScheduling") != false ||
		text(changes[1], "value", "path") != "/dev/mapper/luks2-r-longhorn" {
		t.Fatalf("reserve registration changed existing storage or enabled placement prior to proof: %#v", changes)
	}
	if _, present := valueObject(storage, "spec", "disks")[temporaryReserveDiskName]; present {
		t.Fatal("composing the mutation altered the observed source")
	}
}

func TestRetainedReserveRegistrationCannotUseReadOnlyConfirmation(t *testing.T) {
	env := dispatchFixture()
	env["LONGHORN_V2_OPERATION"] = temporaryReserveRegistrationOperation
	env["LONGHORN_V2_NODE"] = temporaryReserveSelection
	env["LONGHORN_V2_DATABASE"] = ""
	env["LONGHORN_V2_CONFIRM"] = temporaryReserveObservation + ":" + temporaryReserveSelection
	getenv := func(key string) string { return env[key] }
	if err := authorizeTemporaryReserveRegistration(getenv); err == nil {
		t.Fatal("read-only confirmation authorized storage registration")
	}
	env["LONGHORN_V2_CONFIRM"] = temporaryReserveRegistrationOperation + ":" + temporaryReserveSelection
	if err := authorizeTemporaryReserveRegistration(getenv); err != nil {
		t.Fatal(err)
	}
	env["LONGHORN_V2_NODE"] = "temporary-v1-1"
	if err := authorizeTemporaryReserveRegistration(getenv); err == nil {
		t.Fatal("another host was authorized")
	}
}

func TestPendingRetainedReserveCannotHideAnAssignedReplica(t *testing.T) {
	const name = "autoscale-longhorn-v2-1234567890abcdef"
	const uid = "longhorn-worker-uid"
	const manager = "instance-manager-v2"
	planned := temporaryReserveRegistration{Disk: cnpgRecoveryDiskBinding{
		Selection: temporaryReserveSelection, Node: name, NodeUID: uid, DiskName: temporaryReserveDiskName, InstanceManager: manager,
	}}
	diskSpec := blockDisksFor(temporaryRawID(temporaryReserveSelection))
	diskSpec[temporaryReserveDiskName] = retainedReserveDiskSpec(false)
	worker := object{
		"metadata": object{"name": name, "uid": uid, "annotations": object{temporaryReserveOwner: "reserve-journal"}},
		"spec":     object{"disks": diskSpec},
		"status": object{"diskStatus": object{
			"longhorn-v2":            object{"instanceManagerName": manager},
			temporaryReserveDiskName: object{"diskUUID": "retained-disk-uuid"},
		}},
	}
	receipt := object{"metadata": object{"uid": "reserve-journal"}}
	occupied := inventory{storageNodes: []object{worker}, replicas: []object{
		{"spec": object{"nodeID": name, "diskID": "retained-disk-uuid"}},
	}}
	if _, err := projectReserveRegistration(&occupied, receipt, planned); err == nil {
		t.Fatal("a Replica already assigned to the pending disk was hidden")
	}
	empty := inventory{storageNodes: []object{worker}}
	if _, err := projectReserveRegistration(&empty, receipt, planned); err != nil {
		t.Fatal(err)
	}
	if value(empty.storageNodes[0], "spec", "disks", temporaryReserveDiskName) != nil ||
		value(worker, "spec", "disks", temporaryReserveDiskName) == nil {
		t.Fatal("the pending disk was not isolated from the original scheduler read")
	}
}

func TestDisabledReserveUsesLonghornDiskPressureReadback(t *testing.T) {
	const manager = "instance-manager-v2"
	const uuid = "12345678-1234-4234-8234-000000000009"
	// Longhorn 1.13 calculates the Schedulable status from disk pressure.
	// The separate spec flag is what stops scheduling before registration.
	disk := object{
		"diskUUID": uuid, "instanceManagerName": manager, "diskPath": "/dev/mapper/luks2-r-longhorn",
		"diskType": "block", "diskDriver": "aio",
		"storageMaximum": float64(96 * recoveryGiB), "storageAvailable": float64(95 * recoveryGiB),
		"storageScheduled": float64(0), "scheduledReplica": object{}, "scheduledBackingImage": object{},
		"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "True"}},
	}
	worker := object{"status": object{"diskStatus": object{temporaryReserveDiskName: disk}}}
	planned := cnpgRecoveryDiskBinding{InstanceManager: manager}
	bound, err := inactiveReserveDisk(worker, planned)
	if err != nil || bound.DiskUUID != uuid {
		t.Fatalf("Longhorn's healthy disabled disk did not qualify: %v", err)
	}
	disk["scheduledReplica"] = object{"existing-replica": float64(recoveryGiB)}
	if _, err := inactiveReserveDisk(worker, planned); err == nil {
		t.Fatal("a populated scheduling ledger was accepted as unused")
	}
}
