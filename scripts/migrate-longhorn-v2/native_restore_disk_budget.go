package main

import "reflect"

type nativeRestoreDiskCapacity struct {
	Maximum, Available, Scheduled, Reserved int64
}

// This is the disk part of admission for one new four-GiB, three-replica claim.
// Complete native inventories, scheduling settings, host memory, runtime and
// production redundancy remain independent prerequisites in the observer.
func planNativeRestoreDiskBudget(s inventory, targets []nativeRestoreTarget, tag string, tagged bool, config []byte) (map[string]nativeRestoreDiskCapacity, error) {
	receipts, err := qualifyNativeRestoreTargets(targets, s.nodes, s.storageNodes, tag, tagged, config)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	volumes, err := indexed(s.volumes)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	usedUIDs, names := map[string]bool{}, map[string]bool{}
	for _, receipt := range receipts {
		usedUIDs[receipt.Node.UID], usedUIDs[receipt.Storage.UID], usedUIDs[receipt.Preparation.Journal.UID] = true, true, true
		usedUIDs[receipt.Preparation.Activation.UID] = true
	}
	for _, volume := range s.volumes {
		uid := text(volume, "metadata", "uid")
		if !nativeRestoreUID(uid) || usedUIDs[uid] {
			return nil, errNativeRestoreEvidence
		}
		usedUIDs[uid] = true
	}
	for _, replica := range s.replicas {
		name, uid := text(replica, "metadata", "name"), text(replica, "metadata", "uid")
		if !kubeName.MatchString(name) || !nativeRestoreUID(uid) || names[name] || usedUIDs[uid] {
			return nil, errNativeRestoreEvidence
		}
		names[name], usedUIDs[uid] = true, true
	}
	result := map[string]nativeRestoreDiskCapacity{}
	for _, target := range targets {
		lh := storage[target.Name]
		status := valueObject(lh, "status", "diskStatus", "longhorn-v2")
		maximum, maxOK := v1CapacityBytes(status["storageMaximum"])
		available, freeOK := v1CapacityBytes(status["storageAvailable"])
		scheduled, scheduledOK := v1CapacityBytes(status["storageScheduled"])
		reserved, reserveOK := v1CapacityBytes(value(lh, "spec", "disks", "longhorn-v2", "storageReserved"))
		// SPDK reports usable capacity after encryption and storage metadata;
		// it must not exceed the independently qualified raw partition.
		if !maxOK || !freeOK || !scheduledOK || !reserveOK || maximum == 0 || maximum > int64(stagingRawDiskGiB)<<30 ||
			available > maximum || reserved != v1ScratchBytes || reserved > maximum || scheduled > maximum-reserved ||
			v1ScratchBytes > maximum-reserved-scheduled || available <= maximum/4+2*v1ScratchBytes {
			return nil, errNativeRestoreEvidence
		}
		ledger, ok := status["scheduledReplica"].(object)
		if !ok {
			return nil, errNativeRestoreEvidence
		}
		if backing := status["scheduledBackingImage"]; backing != nil {
			entries, valid := backing.(object)
			if !valid || len(entries) != 0 {
				return nil, errNativeRestoreEvidence
			}
		}
		var counted int64
		entries := 0
		for _, replica := range s.replicas {
			node, disk := text(replica, "spec", "nodeID"), text(replica, "spec", "diskID")
			label := text(replica, "metadata", "labels", "longhorndiskuuid")
			if node != target.Name && disk != receipts[target.Name].DiskUUID && label != receipts[target.Name].DiskUUID {
				continue
			}
			volume := volumes[text(replica, "spec", "volumeName")]
			bytes, bytesOK := v1VolumeBytes(text(replica, "spec", "volumeSize"))
			allocated, entryOK := v1CapacityBytes(ledger[text(replica, "metadata", "name")])
			owner := []any{object{"apiVersion": "longhorn.io/v1beta2", "kind": "Volume", "name": text(volume, "metadata", "name"), "uid": text(volume, "metadata", "uid")}}
			// Do not filter inactive, failed or terminating replicas: the disk's
			// scheduler still charges them until their allocations disappear.
			if node != target.Name || disk != receipts[target.Name].DiskUUID || label != disk || text(replica, "spec", "diskPath") != text(status, "diskPath") ||
				text(replica, "apiVersion") != "longhorn.io/v1beta2" || text(replica, "kind") != "Replica" ||
				text(replica, "metadata", "namespace") != "longhorn-system" || text(replica, "metadata", "resourceVersion") == "" ||
				text(replica, "spec", "dataEngine") != "v2" || !reflect.DeepEqual(value(replica, "metadata", "ownerReferences"), owner) ||
				text(volume, "apiVersion") != "longhorn.io/v1beta2" || text(volume, "kind") != "Volume" ||
				text(volume, "metadata", "namespace") != "longhorn-system" || text(volume, "metadata", "resourceVersion") == "" ||
				text(volume, "spec", "dataEngine") != "v2" || text(volume, "spec", "backingImage") != "" ||
				text(volume, "spec", "size") != text(replica, "spec", "volumeSize") || !bytesOK || bytes%(1<<20) != 0 ||
				!entryOK || allocated != bytes || bytes > scheduled-counted {
				return nil, errNativeRestoreEvidence
			}
			counted += bytes
			entries++
		}
		// Backing images, shards and unknown allocations cannot be silently
		// omitted from the reported total or the replica ledger.
		if counted != scheduled || entries != len(ledger) {
			return nil, errNativeRestoreEvidence
		}
		result[target.Name] = nativeRestoreDiskCapacity{Maximum: maximum, Available: available, Scheduled: scheduled, Reserved: reserved}
	}
	return result, nil
}
