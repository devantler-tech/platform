package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

const v1BackupScratchBytes = int64(10 << 30)

var v1BackupWorkers = [...]string{v1RepairWorker, "prod-worker-2", "prod-worker-3"}

type v1BackupDisk struct {
	node                                        object
	disk                                        string
	identity                                    v1ScratchDiskIdentity
	maximum, scheduled, currentReserve, reserve int64
}

type v1BackupHeadroom struct {
	disks   [3]v1BackupDisk
	volumes map[string]string
}

// planV1BackupHeadroom admits one 10 GiB temporary replica per original
// filesystem disk. It uses only the gap between existing durable allocations
// and actual free space, leaving the physical floor and four-GiB margin.
func planV1BackupHeadroom(s inventory) (v1BackupHeadroom, error) {
	refused := errors.New("the original V1 disks do not prove ten GiB of backup staging capacity")
	if err := verifyRedundancy(s); err != nil {
		return v1BackupHeadroom{}, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return v1BackupHeadroom{}, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return v1BackupHeadroom{}, err
	}
	plan := v1BackupHeadroom{volumes: make(map[string]string, len(s.volumes))}
	for _, volume := range s.volumes {
		uid := text(volume, "metadata", "uid")
		if text(volume, "spec", "dataEngine") != "v1" || uid == "" {
			return v1BackupHeadroom{}, refused
		}
		plan.volumes[text(volume, "metadata", "name")] = uid
	}
	for _, replica := range s.replicas {
		if !slices.Contains(v1BackupWorkers[:], text(replica, "spec", "nodeID")) {
			return v1BackupHeadroom{}, refused
		}
	}
	nodeIDs := make(map[string]bool, len(v1BackupWorkers))
	storageIDs := make(map[string]bool, len(v1BackupWorkers))
	diskIDs := make(map[string]bool, len(v1BackupWorkers))
	for i, name := range v1BackupWorkers {
		node, lh := nodes[name], storage[name]
		disk, reserve, status, ok := v1RepairDisk(lh)
		maximum, maxOK := v1CapacityBytes(status["storageMaximum"])
		available, freeOK := v1CapacityBytes(status["storageAvailable"])
		scheduled, scheduledOK := v1CapacityBytes(status["storageScheduled"])
		identity := v1ScratchDiskIdentity{text(node, "metadata", "uid"), text(lh, "metadata", "uid"), text(status, "diskUUID")}
		remaining := maximum - scheduled - reserve
		target := min(reserve, maximum-scheduled-v1BackupScratchBytes)
		if !ready(valueObject(node, "status")) || value(node, "spec", "unschedulable") == true ||
			text(node, "metadata", "annotations", fenceAnnotation) != "" || identity.nodeUID == "" ||
			identity.storageUID == "" || text(lh, "metadata", "resourceVersion") == "" ||
			nodeIDs[identity.nodeUID] || storageIDs[identity.storageUID] || diskIDs[identity.diskUUID] ||
			!ok || !maxOK || !freeOK || !scheduledOK || maximum == 0 || available > maximum || remaining < 0 || target <= 0 ||
			available-v1BackupScratchBytes <= maximum/4+v1ScratchBytes ||
			!v1DiskLedgerMatches(s.replicas, name, status, maximum, scheduled) {
			return v1BackupHeadroom{}, refused
		}
		nodeIDs[identity.nodeUID], storageIDs[identity.storageUID], diskIDs[identity.diskUUID] = true, true, true
		plan.disks[i] = v1BackupDisk{node: lh, disk: disk, identity: identity, maximum: maximum,
			scheduled: scheduled, currentReserve: reserve, reserve: target}
	}
	return plan, nil
}

func authorizeV1BackupHeadroom(getenv func(string) string) error {
	if getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" ||
		getenv("LONGHORN_V2_OPERATION") != "reserve-v1-backup-scratch" {
		return errors.New("the fixed V1 backup reservation operation is required")
	}
	return authorizeWorkflow(getenv, "reserve-longhorn-v1-backup-scratch")
}

func (c client) observeV1BackupHeadroom(ctx context.Context) (v1BackupHeadroom, error) {
	if err := verifyV1ReservationSettings(ctx, c); err != nil {
		return v1BackupHeadroom{}, err
	}
	v2, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", "v2-data-engine", false)
	if err != nil || text(v2, "value") != "false" {
		return v1BackupHeadroom{}, errors.New("V1 backup reservation requires V2 to remain disabled")
	}
	backups, err := c.list(ctx, "backups.velero.io", "velero")
	if err != nil || len(backups) == 0 {
		return v1BackupHeadroom{}, errors.New("the backup activity inventory is incomplete")
	}
	for _, backup := range backups {
		switch text(backup, "status", "phase") {
		case "Completed", "Failed", "PartiallyFailed", "FailedValidation":
		default:
			return v1BackupHeadroom{}, errors.New("a backup is active or its terminal state is unknown")
		}
	}
	uploads, err := c.list(ctx, "datauploads.velero.io", "velero")
	if err != nil {
		return v1BackupHeadroom{}, errors.New("the backup upload inventory is incomplete")
	}
	for _, upload := range uploads {
		switch text(upload, "status", "phase") {
		case "Completed", "Failed", "Canceled":
		default:
			return v1BackupHeadroom{}, errors.New("a backup upload is active or its terminal state is unknown")
		}
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return v1BackupHeadroom{}, err
	}
	return planV1BackupHeadroom(s)
}

func sameBackupHeadroomPlan(expected, current v1BackupHeadroom) bool {
	if !maps.Equal(expected.volumes, current.volumes) {
		return false
	}
	for i, disk := range current.disks {
		prior := expected.disks[i]
		if disk.identity != prior.identity || disk.disk != prior.disk || disk.maximum != prior.maximum ||
			disk.scheduled != prior.scheduled || disk.currentReserve != prior.currentReserve || disk.reserve != prior.reserve {
			return false
		}
	}
	return true
}

// reserveV1BackupHeadroom applies at most one conditional change per disk.
// Each successful write is read back before another disk can be touched.
func reserveV1BackupHeadroom(ctx context.Context, c client, source string) error {
	if c.context != "admin@prod" {
		return errors.New("the protected production context is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	expected, err := c.observeV1BackupHeadroom(ctx)
	if err != nil {
		return err
	}
	current := expected
	writes := 0
	for i := range current.disks {
		disk := current.disks[i]
		if disk.reserve == disk.currentReserve {
			continue
		}
		if err := sourceCurrent(ctx, c.run, source); err != nil {
			return err
		}
		if err := c.patch(ctx, "nodes.longhorn.io", "longhorn-system", disk.node,
			object{"op": "replace", "path": "/spec/disks/" + disk.disk + "/storageReserved", "value": disk.reserve}); err != nil {
			return errors.New("backup reservation write could not be confirmed")
		}
		expected.disks[i].currentReserve = disk.reserve
		current, err = c.observeV1BackupHeadroom(ctx)
		if err != nil || !sameBackupHeadroomPlan(expected, current) {
			return errors.New("backup reservation changed but the original allocation graph or capacity did not verify")
		}
		writes++
	}
	fmt.Printf("LONGHORN_V1_BACKUP_HEADROOM=READY capacityGiB=10 disks=3 writes=%d\n", writes)
	return nil
}
