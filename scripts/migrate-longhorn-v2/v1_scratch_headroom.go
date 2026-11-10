package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

const v1ScratchBytes = int64(4 << 30)

type v1ScratchDiskIdentity struct {
	nodeUID, storageUID, diskUUID string
}

type v1ScratchHeadroom struct {
	node    object
	disk    string
	reserve int64
	write   bool
	disks   [3]v1ScratchDiskIdentity
	volumes map[string]string
}

// planV1ScratchHeadroom proves a single four-GiB allocation on three permanent
// disks while retaining the existing replica graph and physical margin.
func planV1ScratchHeadroom(s inventory) (v1ScratchHeadroom, error) {
	refused := errors.New("the three permanent disks do not prove the reviewed V1 scratch budget")
	if err := verifyRedundancy(s); err != nil {
		return v1ScratchHeadroom{}, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return v1ScratchHeadroom{}, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return v1ScratchHeadroom{}, err
	}
	workers := []string{v1RepairWorker, "prod-worker-2", "prod-worker-3"}
	plan := v1ScratchHeadroom{volumes: make(map[string]string, len(s.volumes))}
	for _, volume := range s.volumes {
		if text(volume, "spec", "dataEngine") != "v1" {
			return v1ScratchHeadroom{}, refused
		}
		plan.volumes[text(volume, "metadata", "name")] = text(volume, "metadata", "uid")
	}
	for _, replica := range s.replicas {
		if !slices.Contains(workers, text(replica, "spec", "nodeID")) {
			return v1ScratchHeadroom{}, refused
		}
	}

	var peerReserve int64
	for i, name := range workers {
		node := nodes[name]
		nodeStatus, _ := value(node, "status").(object)
		if !ready(nodeStatus) || value(node, "spec", "unschedulable") == true || text(node, "metadata", "annotations", fenceAnnotation) != "" {
			return v1ScratchHeadroom{}, refused
		}
		longhornNode := storage[name]
		disk, reserve, status, ok := v1RepairDisk(longhornNode)
		maximum, maxOK := v1CapacityBytes(status["storageMaximum"])
		available, freeOK := v1CapacityBytes(status["storageAvailable"])
		scheduled, scheduledOK := v1CapacityBytes(status["storageScheduled"])
		if !ok || !maxOK || !freeOK || !scheduledOK || maximum == 0 || available > maximum || scheduled > maximum ||
			available-v1ScratchBytes <= maximum/4+v1ScratchBytes || !v1DiskLedgerMatches(s.replicas, name, status, maximum, scheduled) {
			return v1ScratchHeadroom{}, refused
		}
		plan.disks[i] = v1ScratchDiskIdentity{text(node, "metadata", "uid"), text(longhornNode, "metadata", "uid"), text(status, "diskUUID")}
		for _, earlier := range plan.disks[:i] {
			if earlier.diskUUID == plan.disks[i].diskUUID {
				return v1ScratchHeadroom{}, refused
			}
		}
		logical := maximum - scheduled - reserve
		if i != 0 {
			if logical < v1ScratchBytes || reserve == 0 || (peerReserve != 0 && reserve != peerReserve) {
				return v1ScratchHeadroom{}, refused
			}
			peerReserve = reserve
			continue
		}
		if text(longhornNode, "metadata", "resourceVersion") == "" || (logical != 0 && logical < v1ScratchBytes) {
			return v1ScratchHeadroom{}, refused
		}
		plan.node, plan.disk, plan.reserve = longhornNode, disk, reserve
		if logical == 0 {
			plan.reserve -= v1ScratchBytes
			plan.write = true
		}
	}
	if plan.reserve < peerReserve {
		return v1ScratchHeadroom{}, refused
	}
	return plan, nil
}

// authorizeV1ScratchHeadroom admits only the fixed scratch-capacity operation.
func authorizeV1ScratchHeadroom(getenv func(string) string, target string) error {
	if target != v1RepairWorker || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "reserve-v1-scratch" {
		return errors.New("the fixed V1 scratch reservation operation is required")
	}
	return authorizeWorkflow(getenv, "reserve-longhorn-v1-scratch")
}

// observeV1ScratchHeadroom binds the planner to the live scheduling policy and
// the complete V1 storage graph.
func (c client) observeV1ScratchHeadroom(ctx context.Context) (v1ScratchHeadroom, error) {
	if err := verifyV1ReservationSettings(ctx, c); err != nil {
		return v1ScratchHeadroom{}, err
	}
	v2, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", "v2-data-engine", false)
	if err != nil || text(v2, "value") != "false" {
		return v1ScratchHeadroom{}, errors.New("scratch reservation requires V2 to remain disabled")
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return v1ScratchHeadroom{}, errors.New("current storage inventory is unreadable")
	}
	return planV1ScratchHeadroom(s)
}

// reserveV1ScratchHeadroom applies at most one conditional reservation change.
// Success requires a fresh capacity observation on the original disks and volumes.
func reserveV1ScratchHeadroom(ctx context.Context, c client, source string) error {
	if c.context != "admin@prod" {
		return errors.New("the protected production context is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	plan, err := c.observeV1ScratchHeadroom(ctx)
	if err != nil {
		return err
	}
	if !plan.write {
		fmt.Println("LONGHORN_V1_SCRATCH_HEADROOM=READY capacityGiB=4 writes=0")
		return nil
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err = c.patch(ctx, "nodes.longhorn.io", "longhorn-system", plan.node, object{
		"op": "replace", "path": "/spec/disks/" + plan.disk + "/storageReserved", "value": plan.reserve,
	}); err != nil {
		return errors.New("scratch reservation write could not be confirmed")
	}
	current, err := c.observeV1ScratchHeadroom(ctx)
	if err != nil || current.write || current.reserve != plan.reserve || current.disk != plan.disk ||
		current.disks != plan.disks || !maps.Equal(current.volumes, plan.volumes) {
		return errors.New("scratch reservation changed but original storage and capacity remain unverified")
	}
	fmt.Println("LONGHORN_V1_SCRATCH_HEADROOM=READY capacityGiB=4 writes=1")
	return nil
}
