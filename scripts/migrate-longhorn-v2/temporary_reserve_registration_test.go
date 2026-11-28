package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

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
	planned := temporaryReserveRegistration{ActivationUID: "activation-journal", Disk: cnpgRecoveryDiskBinding{
		Selection: temporaryReserveSelection, Node: name, NodeUID: uid, DiskName: temporaryReserveDiskName, InstanceManager: manager,
	}}
	diskSpec := blockDisksFor(temporaryRawID(temporaryReserveSelection))
	diskSpec[temporaryReserveDiskName] = retainedReserveDiskSpec(false)
	worker := object{
		"metadata": object{"name": name, "uid": uid, "annotations": object{temporaryReserveOwner: "reserve-journal", temporaryActivationOwner: "activation-journal"}},
		"spec":     object{"disks": diskSpec},
		"status": object{"diskStatus": object{
			"longhorn-v2":            object{"instanceManagerName": manager},
			temporaryReserveDiskName: object{"diskUUID": "retained-disk-uuid"},
		}},
	}
	receipt := object{"metadata": object{"uid": "reserve-journal"}}
	foreignOwner := planned
	foreignOwner.ActivationUID = "another-activation"
	mismatched := inventory{storageNodes: []object{worker}}
	if _, err := projectReserveRegistration(&mismatched, receipt, foreignOwner); err == nil {
		t.Fatal("registration continued under a different activation")
	}
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

func TestUnverifiedReserveEnableDisablesOnlyItsOwnDisk(t *testing.T) {
	const node = "autoscale-longhorn-v2-1234567890abcdef"
	const namespace = "longhorn-system"
	const uid = "12345678-1234-4234-8234-000000000001"
	const activation = "12345678-1234-4234-8234-000000000002"
	const preparation = "12345678-1234-4234-8234-000000000003"
	const destination = "12345678-1234-4234-8234-000000000004"
	const owner = "12345678-1234-4234-8234-000000000005"
	source := strings.Repeat("c", 40)
	record := temporaryReserveRegistration{Version: 1, Source: source, Run: "101", ActivationUID: activation, PreparationUID: preparation,
		Disk: cnpgRecoveryDiskBinding{Node: node, NodeUID: uid, DiskName: temporaryReserveDiskName, Selection: temporaryReserveSelection,
			DiskUUID: destination, InstanceManager: "instance-manager-v2"}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	receipt := object{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true, "data": object{"record": string(raw)},
		"metadata": object{"namespace": namespace, "name": temporaryReserveJournal, "uid": owner, "resourceVersion": "2"}}
	for _, state := range []string{"enabled", "already disabled", "foreign activation"} {
		t.Run(state, func(t *testing.T) {
			disk := retainedReserveDiskSpec(state != "already disabled")
			registered := activation
			if state == "foreign activation" {
				registered = preparation
			}
			storage := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Node",
				"metadata": object{"namespace": namespace, "uid": uid, "name": node, "resourceVersion": "43",
					"annotations": object{temporaryActivationOwner: registered, temporaryReserveOwner: owner}},
				"spec": object{"disks": object{temporaryReserveDiskName: disk}}}
			writes := 0
			runner := func(ctx context.Context, body []byte, program string, args ...string) ([]byte, error) {
				if ctx.Err() != nil {
					t.Fatal("the cancelled enable context was reused for rollback")
				}
				switch program {
				case "git":
					return []byte(source + "\trefs/heads/main\n"), nil
				case "kubectl":
					if len(args) < 5 || !slices.Equal(args[:2], []string{"--context=admin@prod", "--request-timeout=20s"}) {
						t.Fatalf("unbound reserve command %v", args)
					}
					switch {
					case slices.Equal(args[2:5], []string{"get", "configmap", temporaryReserveJournal}):
						return json.Marshal(receipt)
					case slices.Equal(args[2:5], []string{"get", "nodes.longhorn.io", node}):
						return json.Marshal(storage)
					case slices.Equal(args[2:5], []string{"patch", "nodes.longhorn.io", node}):
						writes++
						var patch []object
						path := "/spec/disks/" + temporaryReserveDiskName + "/allowScheduling"
						if err := json.Unmarshal(body, &patch); err != nil || len(patch) != 4 ||
							text(patch[0], "value") != uid || text(patch[1], "value") != "43" ||
							text(patch[2], "op") != "test" || text(patch[2], "path") != path || value(patch[2], "value") != true ||
							text(patch[3], "op") != "replace" || text(patch[3], "path") != path || value(patch[3], "value") != false {
							t.Fatal("reserve rollback extended beyond the exact scheduling flag or lacked preconditions")
						}
						disk["allowScheduling"] = false
						return nil, nil
					}
				}
				t.Fatalf("unexpected command %s %v", program, args)
				return nil, nil
			}
			a := temporaryV2Activation{c: client{context: "admin@prod", run: runner}, source: source, owner: "101"}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cause := errors.New("native boot readback failed")
			err := a.disableUnprovenReserve(ctx, receipt, record, cause)
			if !errors.Is(err, cause) {
				t.Fatalf("rollback lost the original failure: %v", err)
			}
			if state == "enabled" && (writes != 1 || disk["allowScheduling"] != false) {
				t.Fatal("the uncertain scheduling enable was not reverted")
			}
			if state != "enabled" && writes != 0 {
				t.Fatal("an unnecessary or unowned scheduling write was submitted")
			}
		})
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
