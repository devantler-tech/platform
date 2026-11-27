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

func TestRecoveryCapacityMatchesProtectedArchiveSources(t *testing.T) {
	nodes, replicas, bindings := recoveryCapacityFixture()
	for _, selected := range cnpgArchiveSources() {
		t.Run(selected.Name, func(t *testing.T) {
			source := cnpgRecoverySource{Namespace: selected.Namespace, Cluster: selected.Name, Size: "10Gi"}
			if err := verifyCNPGRecoveryCapacity(source, nodes, replicas, nil, nil, bindings); err != nil {
				t.Fatal(err)
			}
			source.Namespace = "unreviewed"
			if verifyCNPGRecoveryCapacity(source, nodes, replicas, nil, nil, bindings) == nil {
				t.Fatal("an archive in the wrong namespace was accepted")
			}
		})
	}
}

func TestRecoveryCapacityReservesFullDatabaseSizeOnEveryReplicaDisk(t *testing.T) {
	nodes, replicas, bindings := recoveryCapacityFixture()
	source := cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: "10Gi"}
	if err := verifyCNPGRecoveryCapacity(source, nodes, replicas, nil, nil, bindings); err != nil {
		t.Fatal(err)
	}
	// 4 GiB canary space fits here; the actual 10 GiB restore plus reviewed
	// reserves and the strict 25% physical free floor does not.
	recoveryDisk(nodes[2])["storageAvailable"] = float64(39 * recoveryGiB)
	if err := verifyCNPGRecoveryCapacity(source, nodes, replicas, nil, nil, bindings); err == nil {
		t.Fatal("one insufficient replica disk accepted for the full database restore")
	}
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
			if err := verifyCNPGRecoveryCapacity(cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: "10Gi"}, n, r, nil, nil, b); err == nil {
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
		err := verifyCNPGRecoveryCapacity(cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: "10Gi"}, n, r, nil, nil, b)
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
		err := verifyCNPGRecoveryCapacity(cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: "10Gi"}, n, r, nil, nil, b)
		if (err == nil) != (delta == 0) {
			t.Fatalf("logical boundary with delta %d: %v", delta, err)
		}
	}
}

func TestRecoveryCapacityRefusesUnsupportedOrConcurrentAllocations(t *testing.T) {
	n, r, b := recoveryCapacityFixture()
	for _, size := range []string{"", "0Gi", "10G", "10240Mi", "1Ti", "97Gi", "18446744073709551615Gi"} {
		if err := verifyCNPGRecoveryCapacity(cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: size}, n, r, nil, nil, b); err == nil {
			t.Fatalf("unreviewed size %q accepted", size)
		}
	}
	s := cnpgRecoverySource{Namespace: "observability", Cluster: "coroot-db", Size: "10Gi"}
	if verifyCNPGRecoveryCapacity(s, n, r, []object{resource("backing")}, nil, b) == nil || verifyCNPGRecoveryCapacity(s, n, r, nil, []object{resource("shard")}, b) == nil || verifyCNPGRecoveryCapacity(s, n, r, nil, nil, b[:2]) == nil {
		t.Fatal("unreviewed backing image, shard, or incomplete replica roster accepted")
	}
}
