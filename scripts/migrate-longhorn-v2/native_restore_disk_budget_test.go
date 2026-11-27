package main

import (
	"fmt"
	"testing"
)

func nativeRestoreDiskBudgetFixture() (inventory, []nativeRestoreTarget) {
	targets, nodes, storage := nativeRestoreTargetsFixture()
	s := inventory{nodes: nodes, storageNodes: storage}
	for i, lh := range storage {
		maximum := int64(95 << 30) // Usable SPDK size is smaller than the raw partition.
		if i == 0 {
			maximum = 143 << 30
		}
		status := valueObject(lh, "status", "diskStatus", "longhorn-v2")
		status["storageMaximum"], status["storageAvailable"], status["storageScheduled"] = float64(maximum), float64(maximum-8<<30), float64(8<<30)
		name := fmt.Sprintf("existing-v2-r-%d", i)
		status["scheduledReplica"] = object{name: float64(8 << 30)}
		volumeName := fmt.Sprintf("existing-v2-%d", i)
		volume := object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "metadata": object{"name": volumeName, "namespace": "longhorn-system", "uid": fmt.Sprintf("volume-%d", i), "resourceVersion": "1"},
			"spec": object{"dataEngine": "v2", "size": "8589934592", "backingImage": ""}}
		s.volumes = append(s.volumes, volume)
		s.replicas = append(s.replicas, object{"apiVersion": "longhorn.io/v1beta2", "kind": "Replica", "metadata": object{"name": name, "namespace": "longhorn-system", "uid": fmt.Sprintf("replica-%d", i), "resourceVersion": "1", "labels": object{"longhorndiskuuid": text(status, "diskUUID")},
			"ownerReferences": []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": volumeName, "uid": text(volume, "metadata", "uid")}}},
			"spec": object{"dataEngine": "v2", "nodeID": targets[i].Name, "diskID": text(status, "diskUUID"), "diskPath": text(status, "diskPath"), "volumeName": volumeName, "volumeSize": "8589934592", "active": false, "failedAt": "2026-11-16T10:00:00Z"}})
	}
	return s, targets
}

func TestNativeRestoreDiskBudgetChargesEveryRawAllocation(t *testing.T) {
	s, targets := nativeRestoreDiskBudgetFixture()
	// Even an inactive, failed, terminating replica still occupies its ledger entry.
	valueObject(s.replicas[0], "metadata")["deletionTimestamp"] = "2026-11-16T11:00:00Z"
	got, err := planNativeRestoreDiskBudget(s, targets, "longhorn-native-v2-5678", false, poolConfigFixture())
	if err != nil || len(got) != 3 || got[targets[0].Name].Maximum != 143<<30 || got[targets[1].Name].Scheduled != 8<<30 {
		t.Fatal("the mixed raw-disk roster lost a charged allocation or required raw/usable equality:", got, err)
	}
}

func TestNativeRestoreDiskBudgetRefusesUnexplainedOrInsufficientCapacity(t *testing.T) {
	for _, fault := range []string{"missing ledger", "unexplained entry", "unexplained total", "missing replica", "duplicate replica", "duplicate uid", "replica node uid alias", "volume journal uid alias", "invalid volume uid", "wrong disk", "wrong node", "wrong disk label", "hidden labeled replica", "wrong path", "wrong engine", "missing volume", "wrong volume owner", "wrong volume size", "backing image", "zero backing allocation", "malformed backing ledger", "fractional bytes", "negative bytes", "oversized available", "oversized maximum", "logical floor", "physical floor", "reservation drift", "not prepared"} {
		t.Run(fault, func(t *testing.T) {
			s, targets := nativeRestoreDiskBudgetFixture()
			status := valueObject(s.storageNodes[1], "status", "diskStatus", "longhorn-v2")
			spec := valueObject(s.replicas[1], "spec")
			switch fault {
			case "missing ledger":
				delete(status, "scheduledReplica")
			case "unexplained entry":
				valueObject(status, "scheduledReplica")["unknown"] = float64(1 << 30)
			case "unexplained total":
				status["storageScheduled"] = float64(9 << 30)
			case "missing replica":
				s.replicas = append(s.replicas[:1], s.replicas[2:]...)
			case "duplicate replica":
				s.replicas = append(s.replicas, s.replicas[1])
			case "duplicate uid":
				valueObject(s.replicas[1], "metadata")["uid"] = text(s.replicas[0], "metadata", "uid")
			case "replica node uid alias":
				valueObject(s.replicas[1], "metadata")["uid"] = targets[1].UID
			case "volume journal uid alias", "invalid volume uid":
				uid := targets[1].Preparation.Journal.UID
				if fault == "invalid volume uid" {
					uid = "invalid\nuid"
				}
				valueObject(s.volumes[1], "metadata")["uid"] = uid
				value(s.replicas[1], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = uid
			case "wrong disk":
				spec["diskID"] = "other"
			case "wrong node":
				spec["nodeID"] = "other"
			case "wrong disk label":
				valueObject(s.replicas[1], "metadata", "labels")["longhorndiskuuid"] = "other"
			case "hidden labeled replica":
				s.replicas = append(s.replicas, object{"metadata": object{"name": "hidden-replica", "uid": "hidden-uid", "labels": object{"longhorndiskuuid": "disk-2"}}, "spec": object{"nodeID": "other", "diskID": "other"}})
			case "wrong path":
				spec["diskPath"] = "/wrong"
			case "wrong engine":
				spec["dataEngine"] = "v1"
			case "missing volume":
				s.volumes = append(s.volumes[:1], s.volumes[2:]...)
			case "wrong volume owner":
				value(s.replicas[1], "metadata", "ownerReferences").([]any)[0].(object)["uid"] = "replacement"
			case "wrong volume size":
				valueObject(s.volumes[1], "spec")["size"] = "4294967296"
			case "backing image":
				valueObject(s.volumes[1], "spec")["backingImage"] = "unsupported"
			case "zero backing allocation":
				status["scheduledBackingImage"] = object{"pending": float64(0)}
			case "malformed backing ledger":
				status["scheduledBackingImage"] = []any{}
			case "fractional bytes":
				status["storageAvailable"] = 1024.5
			case "negative bytes":
				status["storageScheduled"] = float64(-1)
			case "oversized available":
				status["storageAvailable"] = float64(96 << 30)
			case "oversized maximum":
				status["storageMaximum"] = float64((stagingRawDiskGiB + 1) << 30)
			case "logical floor":
				status["storageMaximum"] = float64(15 << 30)
				status["storageAvailable"] = float64(15 << 30)
			case "physical floor":
				status["storageAvailable"] = status["storageMaximum"].(float64)/4 + float64(8<<30)
			case "reservation drift":
				valueObject(s.storageNodes[1], "spec", "disks", "longhorn-v2")["storageReserved"] = float64(0)
			case "not prepared":
				targets[1].Preparation.RawPartitionUUID = ""
			}
			if got, err := planNativeRestoreDiskBudget(s, targets, "longhorn-native-v2-5678", false, poolConfigFixture()); err == nil || got != nil {
				t.Fatal("unsafe disk admission accepted:", fault, got)
			}
		})
	}
}
