package main

import (
	"fmt"
	"strconv"
	"testing"
)

func recoveryCapacityFixture() ([]object, []object, []cnpgRecoveryDiskBinding) {
	var nodes, replicas []object
	var bindings []cnpgRecoveryDiskBinding
	for i, selection := range temporaryV2Selections {
		name, uuid, manager := fmt.Sprintf("storage-%d", i), fmt.Sprintf("disk-%d", i), fmt.Sprintf("im-%d", i)
		n := resource(name)
		disks := blockDisksFor(temporaryRawID(selection))
		path := text(disks, "longhorn-v2", "path")
		n["spec"] = object{"disks": disks, "allowScheduling": true, "evictionRequested": false}
		status := object{"diskUUID": uuid, "diskPath": path, "diskType": "block", "diskDriver": "aio", "instanceManagerName": manager,
			"storageMaximum": float64(95 * recoveryGiB), "storageAvailable": float64(70 * recoveryGiB), "storageScheduled": float64(20 * recoveryGiB),
			"scheduledReplica": object{fmt.Sprintf("replica-%d", i): float64(20 * recoveryGiB)}, "scheduledBackingImage": object{},
			"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "True"}}}
		n["status"] = object{"conditions": readyConditions(), "diskStatus": object{"longhorn-v2": status}}
		nodes = append(nodes, n)
		bindings = append(bindings, cnpgRecoveryDiskBinding{Selection: selection, Node: name, NodeUID: text(n, "metadata", "uid"), DiskUUID: uuid, InstanceManager: manager})
		r := resource(fmt.Sprintf("replica-%d", i))
		r["spec"] = object{"dataEngine": "v2", "nodeID": name, "diskID": uuid, "diskPath": path, "volumeSize": "21474836480"}
		replicas = append(replicas, r)
	}
	// Retained V1 filesystem data is not an allocation on the V2 disk UUIDs.
	v1 := resource("retained-v1")
	v1["spec"] = object{"dataEngine": "v1", "diskID": "old-filesystem", "volumeSize": "107374182400"}
	replicas = append(replicas, v1)
	return nodes, replicas, bindings
}

func recoveryDisk(n object) object {
	return value(n, "status", "diskStatus", "longhorn-v2").(object)
}

func TestQualifiedRetainedReservePreservesArchiveDiskBindings(t *testing.T) {
	_, _, bindings := recoveryCapacityFixture()
	archived := cnpgStorageEnvironment{
		Activation: benchmarkResource{Name: temporaryActivationJournal, Namespace: "longhorn-system", UID: "activation"},
		Targets:    make(map[string]temporaryV2Binding),
		Disks:      bindings,
	}
	var extra cnpgRecoveryDiskBinding
	for _, disk := range bindings {
		archived.Targets[disk.Selection] = temporaryV2Binding{Selection: disk.Selection, Name: disk.Node}
		if disk.Selection == temporaryReserveSelection {
			extra = disk
		}
	}
	extra.DiskName, extra.DiskUUID = "longhorn-v2-retained", "retained-disk"
	qualified := archived
	qualified.Disks = append(append([]cnpgRecoveryDiskBinding{}, bindings...), extra)
	if !recoveryStoragePreservesArchive(qualified, archived) || !recoveryStoragePreservesArchive(archived, archived) {
		t.Fatal("the current storage should preserve the original archive bindings")
	}
	if recoveryStoragePreservesArchive(archived, qualified) {
		t.Fatal("the archived reserve disk disappeared")
	}
	for name, mutate := range map[string]func(*cnpgStorageEnvironment){
		"replaced primary disk": func(s *cnpgStorageEnvironment) { s.Disks[0].DiskUUID = "replacement" },
		"duplicate disk UUID":   func(s *cnpgStorageEnvironment) { s.Disks[3].DiskUUID = s.Disks[0].DiskUUID },
		"wrong reserve host":    func(s *cnpgStorageEnvironment) { s.Disks[3].Node = "other-host" },
		"changed activation":    func(s *cnpgStorageEnvironment) { s.Activation.UID = "replacement" },
		"different reserve":     func(s *cnpgStorageEnvironment) { s.Disks[3].DiskName = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := qualified
			candidate.Disks = append([]cnpgRecoveryDiskBinding{}, qualified.Disks...)
			mutate(&candidate)
			if recoveryStoragePreservesArchive(candidate, archived) {
				t.Fatal("a changed storage identity was treated as continuous")
			}
		})
	}
}

func TestRecoveryCapacityReservesFullDatabaseSizeOnEveryReplicaDisk(t *testing.T) {
	nodes, replicas, bindings := recoveryCapacityFixture()
	if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err != nil {
		t.Fatal(err)
	}
	// 4 GiB canary space fits here; the actual 10 GiB restore plus reviewed
	// reserves and the strict 25% physical free floor does not.
	recoveryDisk(nodes[2])["storageAvailable"] = float64(39 * recoveryGiB)
	if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err == nil {
		t.Fatal("one insufficient replica disk accepted for the full database restore")
	}
}

func TestRecoveryCapacityIncludesOnlyTheBoundTemporaryReserve(t *testing.T) {
	nodes, replicas, bindings := recoveryCapacityFixture()
	for i, primary := range bindings {
		if primary.Selection != "temporary-v2-1" {
			continue
		}
		extra := primary
		extra.DiskName, extra.DiskUUID = "longhorn-v2-retained", "retained-disk"
		bindings = append(bindings, extra)
		disks := valueObject(nodes[i], "spec", "disks")
		disks[extra.DiskName] = blockDisksFor("r-longhorn")["longhorn-v2"]
		status := object{
			"diskUUID": extra.DiskUUID, "diskPath": text(disks, extra.DiskName, "path"),
			"diskType": "block", "diskDriver": "aio", "instanceManagerName": extra.InstanceManager,
			"storageMaximum": float64(95 * recoveryGiB), "storageAvailable": float64(70 * recoveryGiB),
			"storageScheduled": float64(0), "scheduledReplica": object{},
			"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "True"}},
		}
		valueObject(nodes[i], "status", "diskStatus")[extra.DiskName] = status
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err != nil {
			t.Fatalf("known reserve on the verified temporary host was rejected: %v", err)
		}
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings[:len(bindings)-1]); err == nil {
			t.Fatal("an enabled raw disk outside the native inventory was accepted")
		}
		ledger := recoveryDisk(nodes[i])
		conditions := ledger["conditions"]
		free := ledger["storageAvailable"]
		ledger["conditions"] = []any{object{"type": "Ready", "status": "True"}, object{"type": "Schedulable", "status": "False", "reason": "DiskPressure"}}
		ledger["storageAvailable"] = float64((95 * recoveryGiB) / 4)
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err != nil {
			t.Fatalf("the scheduler excludes the pressured primary, and the reserve covers that host: %v", err)
		}
		ledger["conditions"], ledger["storageAvailable"] = conditions, free
		name := text(replicas[i], "metadata", "name")
		ledger["storageScheduled"] = float64(87 * recoveryGiB)
		ledger["scheduledReplica"] = object{name: float64(87 * recoveryGiB)}
		valueObject(replicas[i], "spec")["volumeSize"] = strconv.FormatUint(87*recoveryGiB, 10)
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err != nil {
			t.Fatalf("the verified reserve should satisfy the original host's placement: %v", err)
		}
		status["storageAvailable"] = float64(35 * recoveryGiB)
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err == nil {
			t.Fatal("a host without a disk that preserves the physical reserve was accepted")
		}
		status["storageAvailable"] = float64(70 * recoveryGiB)
		other := (i + 1) % len(nodes)
		recoveryDisk(nodes[other])["storageAvailable"] = float64(20 * recoveryGiB)
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err == nil {
			t.Fatal("another disk on the same host cannot replace the missing third host")
		}
		recoveryDisk(nodes[other])["storageAvailable"] = float64(70 * recoveryGiB)
		bindings[len(bindings)-1].NodeUID = "foreign-node"
		if err := verifyRecoveryClaimCapacity("10Gi", nodes, replicas, nil, nil, bindings); err == nil {
			t.Fatal("a supplemental disk with a different host identity was accepted")
		}
		return
	}
	t.Fatal("missing the reviewed temporary reserve host")
}

func TestRecoveryCapacityRejectsStaleOrIncompleteDiskAccounting(t *testing.T) {
	for name, change := range map[string]func([]object, []object, []cnpgRecoveryDiskBinding){
		"quoted native free bytes": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["storageAvailable"] = "75161927680"
		},
		"unknown allocation": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["storageScheduled"] = float64(21 * recoveryGiB)
		},
		"replica missing from ledger": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["scheduledReplica"] = object{}
		},
		"retained failed replica omitted": func(_ []object, r []object, _ []cnpgRecoveryDiskBinding) {
			r[0]["spec"].(object)["failedAt"] = "2026-11-01T00:00:00Z"
			r[0]["spec"].(object)["volumeSize"] = "22548578304"
		},
		"pending V2 placement": func(_ []object, r []object, _ []cnpgRecoveryDiskBinding) { r[0]["spec"].(object)["diskID"] = "" },
		"replaced storage node": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			n[0]["metadata"].(object)["uid"] = "replacement"
		},
		"replaced disk": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["diskUUID"] = "replacement"
		},
		"different manager": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["instanceManagerName"] = "replacement"
		},
		"disk unschedulable": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["conditions"] = readyConditions()
		},
		"wrong reserve": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			value(n[0], "spec", "disks", "longhorn-v2").(object)["storageReserved"] = float64(0)
		},
		"logical overcommit": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["storageMaximum"] = float64(35 * recoveryGiB)
		},
		"non-SPDK raw maximum": func(n []object, _ []object, _ []cnpgRecoveryDiskBinding) {
			recoveryDisk(n[0])["storageMaximum"] = float64((stagingRawDiskGiB + 1) * recoveryGiB)
		},
		"original retained raw path": func(n []object, _ []object, b []cnpgRecoveryDiskBinding) {
			for i, binding := range b {
				if binding.Selection == "temporary-v2-1" {
					recoveryDisk(n[i])["diskPath"] = "/dev/mapper/luks2-r-longhorn"
				}
			}
		},
		"duplicate selection":  func(_ []object, _ []object, b []cnpgRecoveryDiskBinding) { b[1].Selection = b[0].Selection },
		"duplicate bound disk": func(_ []object, _ []object, b []cnpgRecoveryDiskBinding) { b[1].DiskUUID = b[0].DiskUUID },
	} {
		t.Run(name, func(t *testing.T) {
			n, r, b := recoveryCapacityFixture()
			change(n, r, b)
			if err := verifyRecoveryClaimCapacity("10Gi", n, r, nil, nil, b); err == nil {
				t.Fatal("incomplete recovery capacity accepted")
			}
		})
	}
}

func TestRecoveryCapacityRequiresStrictPhysicalFreeFloor(t *testing.T) {
	n, r, b := recoveryCapacityFixture()
	const q = 10 * recoveryGiB
	floor := uint64(95*recoveryGiB) / 4
	for _, delta := range []uint64{0, 1} {
		recoveryDisk(n[0])["storageAvailable"] = float64(floor + q + 6*recoveryGiB + delta)
		err := verifyRecoveryClaimCapacity("10Gi", n, r, nil, nil, b)
		if (err == nil) != (delta == 1) {
			t.Fatalf("strict physical floor with delta %d: %v", delta, err)
		}
	}
}

func TestRecoveryCapacityPreservesLogicalReserveAtTheNativeBoundary(t *testing.T) {
	n, r, b := recoveryCapacityFixture()
	for _, delta := range []uint64{0, 1} {
		bytes := 79*recoveryGiB + delta
		recoveryDisk(n[0])["storageScheduled"] = float64(bytes)
		recoveryDisk(n[0])["scheduledReplica"] = object{"replica-0": float64(bytes)}
		r[0]["spec"].(object)["volumeSize"] = strconv.FormatUint(bytes, 10)
		err := verifyRecoveryClaimCapacity("10Gi", n, r, nil, nil, b)
		if (err == nil) != (delta == 0) {
			t.Fatalf("logical boundary with delta %d: %v", delta, err)
		}
	}
}

func TestRecoveryCapacityRefusesUnsupportedOrConcurrentAllocations(t *testing.T) {
	n, r, b := recoveryCapacityFixture()
	for _, size := range []string{"", "0Gi", "10G", "10240Mi", "1Ti", "97Gi", "18446744073709551615Gi"} {
		if err := verifyRecoveryClaimCapacity(size, n, r, nil, nil, b); err == nil {
			t.Fatalf("unreviewed size %q accepted", size)
		}
	}
	if verifyRecoveryClaimCapacity("10Gi", n, r, []object{resource("backing")}, nil, b) == nil || verifyRecoveryClaimCapacity("10Gi", n, r, nil, []object{resource("shard")}, b) == nil || verifyRecoveryClaimCapacity("10Gi", n, r, nil, nil, b[:2]) == nil {
		t.Fatal("unreviewed backing image, shard, or incomplete replica roster accepted")
	}
}
