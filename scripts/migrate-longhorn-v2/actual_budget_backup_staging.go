package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
)

// Budget staging uses Velero's disposable CSI clone. The running application
// stays on its original claim and the daily backup still takes snapshots.
type actualBudgetStaging struct {
	Config, StorageClass                        benchmarkResource
	ConfigResourceVersion, ClassResourceVersion string
}

func observeActualBudgetStaging(ctx context.Context, c client) (actualBudgetStaging, error) {
	refused := errors.New("the disposable single-replica CSI staging configuration is unverified")
	config, err := c.get(ctx, "configmap", "velero", "node-agent-config", false)
	if err != nil || !recoveredNativeObject(config, "v1", "ConfigMap", "velero") {
		return actualBudgetStaging{}, refused
	}
	class, err := c.get(ctx, "storageclass", "", v1BackupStagingClass, false)
	if err != nil || !recoveredNativeObject(class, "storage.k8s.io/v1", "StorageClass", "") ||
		text(class, "metadata", "name") != v1BackupStagingClass ||
		text(class, "provisioner") != "driver.longhorn.io" ||
		text(class, "volumeBindingMode") != "Immediate" || text(class, "reclaimPolicy") != "Delete" ||
		text(class, "parameters", "numberOfReplicas") != "1" || text(class, "parameters", "dataEngine") != "v1" ||
		text(class, "parameters", "dataLocality") != "disabled" {
		return actualBudgetStaging{}, refused
	}
	var effective object
	if decodeIsolationJSON([]byte(text(config, "data", "node-agent-config.json")), &effective) != nil ||
		!reflect.DeepEqual(value(effective, "backupPVC", "longhorn"), object{"storageClass": v1BackupStagingClass}) ||
		!reflect.DeepEqual(value(effective, "backupPVC", "longhorn-wffc"), object{"storageClass": "longhorn-wffc"}) {
		return actualBudgetStaging{}, refused
	}
	return actualBudgetStaging{
		Config: benchmarkIdentity(config), StorageClass: benchmarkIdentity(class),
		ConfigResourceVersion: text(config, "metadata", "resourceVersion"),
		ClassResourceVersion:  text(class, "metadata", "resourceVersion"),
	}, nil
}

func verifyActualBudgetStagingCapacity(ctx context.Context, c client) error {
	s, err := observeV1BackupStagingInventory(ctx, c)
	if err != nil {
		return err
	}
	return verifyActualBudgetStagingPlacement(s)
}

func verifyActualBudgetStagingPlacement(s inventory) error {
	nodes, err := indexed(s.nodes)
	if err != nil {
		return err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return err
	}
	// The staging class has no placement selector. An additional V1 disk
	// could receive the clone even when the three expected disks are healthy.
	for worker, node := range storage {
		if slices.Contains(v1BackupWorkers[:], worker) {
			continue
		}
		for _, raw := range valueObject(node, "spec", "disks") {
			disk, _ := raw.(object)
			if text(disk, "diskType") != "block" {
				return errors.New("an unqualified Longhorn disk could receive the V1 staging replica")
			}
		}
	}
	eligible := 0
	for _, worker := range v1BackupWorkers {
		_, reserve, disk, ok := actualBudgetScratchDisk(nodes[worker], storage[worker])
		maximum, maxOK := v1CapacityBytes(disk["storageMaximum"])
		scheduled, scheduledOK := v1CapacityBytes(disk["storageScheduled"])
		if !ok || !maxOK || !scheduledOK || maximum <= 0 || reserve > maximum ||
			!v1DiskLedgerMatches(s.replicas, worker, disk, maximum, scheduled) {
			return errors.New("the V1 disk allocation is unverified")
		}
		// With the verified 100% setting, a disk outside Longhorn's logical
		// limit cannot receive this 10 GiB clone. Include every other disk
		// regardless of preference, and charge the full clone's physical size.
		if maximum-reserve-scheduled < v1BackupScratchBytes {
			continue
		}
		if !fitsActualBudgetSnapshot(disk, reserve, s.replicas, worker) {
			return errors.New("an eligible V1 disk cannot preserve the physical floor and margin")
		}
		eligible++
	}
	if eligible == 0 {
		return errors.New("no stable V1 disk can stage one 10 GiB snapshot while retaining the physical floor and margin")
	}
	return nil
}

func observeV1BackupStagingInventory(ctx context.Context, c client) (inventory, error) {
	if err := verifyV1ReservationSettings(ctx, c); err != nil {
		return inventory{}, err
	}
	uploads, err := c.list(ctx, "datauploads.velero.io", "velero")
	if err != nil {
		return inventory{}, errors.New("existing Velero uploads cannot be verified")
	}
	for _, upload := range uploads {
		switch text(upload, "status", "phase") {
		case "Completed", "Failed", "Canceled":
		default:
			return inventory{}, errors.New("an existing Velero upload is active or unverified")
		}
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return inventory{}, err
	}
	if err = verifyRedundancy(s); err != nil {
		return inventory{}, err
	}
	return s, nil
}

func fitsActualBudgetSnapshot(disk object, reserved int64, replicas []object, worker string) bool {
	target, ok := actualBudgetRequiredReservation(disk, reserved, replicas, worker)
	return ok && target == reserved
}

// Open only the measured logical gap for one disposable clone. The physical
// floor and margin continue to apply to the full clone size.
func actualBudgetRequiredReservation(disk object, reserved int64, replicas []object, worker string) (int64, bool) {
	maximum, maxOK := v1CapacityBytes(disk["storageMaximum"])
	available, availOK := v1CapacityBytes(disk["storageAvailable"])
	scheduled, schedOK := v1CapacityBytes(disk["storageScheduled"])
	const margin = int64(4 << 30)
	const qualifiedShortfall = int64(2 << 30)
	target := min(reserved, maximum-scheduled-v1BackupScratchBytes)
	if !maxOK || !availOK || !schedOK || reserved < 0 || maximum <= 0 || available > maximum ||
		target < 0 || target == 0 && reserved > 0 || reserved-target > qualifiedShortfall ||
		available-v1BackupScratchBytes <= maximum/4+margin ||
		!v1DiskLedgerMatches(replicas, worker, disk, maximum, scheduled) {
		return 0, false
	}
	return target, true
}
