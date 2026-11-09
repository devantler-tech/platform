package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const v1RepairWorker = "prod-worker-1"

type v1ReservationRepair struct {
	node     object
	disk     string
	diskUUID string
	reserve  int64
	volumes  map[string]string
}

func planV1ReservationRepair(s inventory) (v1ReservationRepair, error) {
	refused := errors.New("the fixed V1 repair does not match current volume, disk and capacity evidence")
	if err := verifyHealthyCopies(s, 2, v1RepairWorker); err != nil {
		return v1ReservationRepair{}, err
	}
	nodes, err := indexed(s.nodes)
	if err != nil {
		return v1ReservationRepair{}, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return v1ReservationRepair{}, err
	}
	for _, name := range []string{v1RepairWorker, "prod-worker-2", "prod-worker-3"} {
		status, _ := value(nodes[name], "status").(object)
		if !ready(status) || value(nodes[name], "spec", "unschedulable") == true || text(nodes[name], "metadata", "annotations", fenceAnnotation) != "" {
			return v1ReservationRepair{}, refused
		}
	}
	node := storage[v1RepairWorker]
	disk, oldReserve, status, ok := v1RepairDisk(node)
	if !ok || text(node, "metadata", "resourceVersion") == "" {
		return v1ReservationRepair{}, refused
	}
	_, peerA, _, aOK := v1RepairDisk(storage["prod-worker-2"])
	_, peerB, _, bOK := v1RepairDisk(storage["prod-worker-3"])
	if !aOK || !bOK || peerA <= 0 || peerA != peerB {
		return v1ReservationRepair{}, refused
	}
	maximum, maxOK := v1CapacityBytes(status["storageMaximum"])
	available, freeOK := v1CapacityBytes(status["storageAvailable"])
	scheduled, scheduledOK := v1CapacityBytes(status["storageScheduled"])
	ledger, ledgerOK := status["scheduledReplica"].(object)
	if !maxOK || !freeOK || !scheduledOK || !ledgerOK || maximum == 0 || available > maximum || scheduled > maximum || oldReserve != maximum*30/100 {
		return v1ReservationRepair{}, refused
	}
	var counted int64
	entries := 0
	for _, replica := range s.replicas {
		if text(replica, "spec", "nodeID") != v1RepairWorker {
			continue
		}
		bytes, valid := v1VolumeBytes(text(replica, "spec", "volumeSize"))
		allocated, present := v1CapacityBytes(ledger[text(replica, "metadata", "name")])
		if !valid || !present || bytes != allocated || text(replica, "spec", "diskID") != text(status, "diskUUID") || text(replica, "spec", "dataEngine") != "v1" || counted > maximum-bytes {
			return v1ReservationRepair{}, refused
		}
		counted += bytes
		entries++
	}
	if counted != scheduled || entries != len(ledger) {
		return v1ReservationRepair{}, refused
	}

	const gib = int64(1 << 30)
	expected := map[string]int64{"substrate-db-2": 5 * gib, "rustfs-data": 10 * gib}
	volumes := map[string]string{}
	var demand int64
	for _, volume := range s.volumes {
		if text(volume, "spec", "dataEngine") != "v1" {
			return v1ReservationRepair{}, refused
		}
		if text(volume, "status", "robustness") != "degraded" {
			continue
		}
		name := text(volume, "metadata", "name")
		claim := text(volume, "status", "kubernetesStatus", "pvcName")
		size, valid := v1VolumeBytes(text(volume, "spec", "size"))
		if !valid || expected[claim] != size || text(volume, "status", "kubernetesStatus", "namespace") != "ate-system" || !v1InsufficientCapacity(volume) {
			return v1ReservationRepair{}, refused
		}
		for _, engine := range s.engines {
			if text(engine, "spec", "volumeName") == name && value(engine, "spec", "active") == true {
				modes, _ := value(engine, "status", "replicaModeMap").(object)
				if len(modes) != 2 {
					return v1ReservationRepair{}, refused
				}
			}
		}
		for _, replica := range s.replicas {
			if text(replica, "spec", "volumeName") == name && text(replica, "spec", "nodeID") == v1RepairWorker {
				return v1ReservationRepair{}, refused
			}
		}
		delete(expected, claim)
		demand += size
		volumes[name] = text(volume, "metadata", "uid")
	}
	// Admit precisely the missing logical allocations. Keep the existing
	// physical free-space floor plus four GiB even if each copy fills its claim.
	reserve := maximum - scheduled - demand
	if len(expected) != 0 || len(volumes) != 2 || reserve < peerA || reserve >= oldReserve || available-demand <= maximum/4+4*gib {
		return v1ReservationRepair{}, refused
	}
	return v1ReservationRepair{node: node, disk: disk, diskUUID: text(status, "diskUUID"), reserve: reserve, volumes: volumes}, nil
}

func v1RepairDisk(node object) (string, int64, object, bool) {
	disks, _ := value(node, "spec", "disks").(object)
	status, _ := value(node, "status").(object)
	if len(disks) != 1 || !ready(status) || value(node, "spec", "allowScheduling") != true || value(node, "spec", "evictionRequested") != false {
		return "", 0, nil, false
	}
	for name, raw := range disks {
		disk, _ := raw.(object)
		observed, _ := value(node, "status", "diskStatus", name).(object)
		reserve, ok := v1CapacityBytes(disk["storageReserved"])
		if ok && kubeName.MatchString(name) && (name == "default-disk" || strings.HasPrefix(name, "default-disk-")) &&
			text(disk, "diskType") == "filesystem" && text(disk, "path") == "/var/lib/longhorn" &&
			value(disk, "allowScheduling") == true && value(disk, "evictionRequested") == false &&
			ready(observed) && text(v1Condition(observed, "Schedulable"), "status") == "True" &&
			text(observed, "diskUUID") != "" {
			return name, reserve, observed, true
		}
	}
	return "", 0, nil, false
}

func v1CapacityBytes(raw any) (int64, bool) {
	n, ok := raw.(float64)
	if !ok || n < 0 || n > 1<<40 || n != float64(int64(n)) {
		return 0, false
	}
	return int64(n), true
}

func v1VolumeBytes(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0 && n <= 1<<40 && strconv.FormatInt(n, 10) == raw
}

func v1InsufficientCapacity(volume object) bool {
	status, _ := value(volume, "status").(object)
	condition := v1Condition(status, "Scheduled")
	message := text(condition, "message")
	return text(condition, "status") == "False" && text(condition, "reason") == "ReplicaSchedulingFailure" &&
		(message == "insufficient storage" || message == "precheck new replica failed: insufficient storage")
}

func v1Condition(status object, kind string) object {
	conditions, _ := status["conditions"].([]any)
	var result object
	for _, raw := range conditions {
		condition, _ := raw.(object)
		if text(condition, "type") == kind {
			if result != nil {
				return nil
			}
			result = condition
		}
	}
	return result
}

func authorizeV1ReservationRepair(getenv func(string) string, target string) error {
	if target != v1RepairWorker || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "repair-v1-reservation" {
		return errors.New("the fixed V1 reservation repair operation is required")
	}
	return authorizeWorkflow(getenv, "repair-longhorn-v1-reservation")
}

// repairV1Reservation makes one resource-version-fenced change and waits for
// the original volumes to regain three healthy copies.
func repairV1Reservation(ctx context.Context, c client, source string) error {
	if c.context != "admin@prod" {
		return errors.New("the protected production context is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	for name, expected := range map[string]string{"storage-over-provisioning-percentage": "100", "storage-minimal-available-percentage": "25"} {
		setting, err := c.get(ctx, "settings.longhorn.io", "longhorn-system", name, false)
		if err != nil || text(setting, "value") != expected {
			return errors.New("live scheduling settings differ from the repair budget")
		}
	}
	s, err := c.snapshot(ctx)
	if err != nil {
		return errors.New("current storage inventory is unreadable")
	}
	if verifyRedundancy(s) == nil {
		fmt.Println("LONGHORN_V1_REPLICA_REPAIR=ALREADY_HEALTHY writes=0")
		return nil
	}
	plan, err := planV1ReservationRepair(s)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	if err = c.patch(ctx, "nodes.longhorn.io", "longhorn-system", plan.node, object{
		"op": "replace", "path": "/spec/disks/" + plan.disk + "/storageReserved", "value": plan.reserve,
	}); err != nil {
		return errors.New("reservation write could not be confirmed")
	}
	err = waitReadyWithin(ctx, 10*time.Minute, func(ctx context.Context) error {
		current, readErr := c.snapshot(ctx)
		if readErr != nil || verifyRedundancy(current) != nil {
			return errors.New("three healthy copies have not been confirmed")
		}
		volumes, e := indexed(current.volumes)
		if e != nil {
			return e
		}
		for name, uid := range plan.volumes {
			if text(volumes[name], "metadata", "uid") != uid {
				return errors.New("an original volume identity changed")
			}
		}
		nodes, e := indexed(current.storageNodes)
		if e != nil {
			return e
		}
		node := nodes[v1RepairWorker]
		disk, reserve, status, valid := v1RepairDisk(node)
		if !valid || disk != plan.disk || text(status, "diskUUID") != plan.diskUUID ||
			reserve != plan.reserve || text(node, "metadata", "uid") != text(plan.node, "metadata", "uid") {
			return errors.New("the recorded reservation has not been read back")
		}
		return nil
	})
	if err != nil {
		return errors.New("reservation changed but three-copy recovery remains unverified")
	}
	fmt.Println("LONGHORN_V1_REPLICA_REPAIR=RECOVERED volumes=2 writes=1")
	return nil
}
