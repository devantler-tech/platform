package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

const temporaryReserveRegistrationOperation = "register-retained-v2-capacity"

func authorizeTemporaryReserveRegistration(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != temporaryReserveRegistrationOperation ||
		getenv("LONGHORN_V2_NODE") != temporaryReserveSelection || getenv("LONGHORN_V2_DATABASE") != "" {
		return errTemporaryActivation
	}
	return authorizeWorkflow(getenv, temporaryReserveRegistrationOperation+":"+temporaryReserveSelection)
}

func retainedReserveDiskSpec(schedulable bool) object {
	spec := blockDisks()["longhorn-v2"].(object)
	spec["allowScheduling"] = schedulable
	return spec
}

func reviewedReserveDisk(raw any) bool {
	disk, ok := raw.(object)
	enabled, hasFlag := disk["allowScheduling"].(bool)
	return ok && hasFlag && reviewedBlockDisk(raw, retainedReserveDiskSpec(enabled))
}

func retainedReserveRegistrationChanges(storage object, receiptUID string) []object {
	var changes []object
	if value(storage, "metadata", "annotations") == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/annotations", "value": object{}})
	}
	return append(changes,
		object{"op": "add", "path": "/metadata/annotations/" + strings.ReplaceAll(temporaryReserveOwner, "/", "~1"), "value": receiptUID},
		object{"op": "add", "path": "/spec/disks/" + temporaryReserveDiskName, "value": retainedReserveDiskSpec(false)},
	)
}

func (a temporaryV2Activation) reserveRegistrationSnapshot(ctx context.Context, receipt object, record temporaryReserveRegistration) (object, error) {
	if err := verifyBudgetSettings(ctx, a.c); err != nil {
		return nil, err
	}
	if err := verifyV2Settings(ctx, a.c); err != nil {
		return nil, err
	}
	s, err := a.c.snapshot(ctx)
	if err != nil || a.verifyTemporaryReserveNative(ctx, s, record) != nil {
		return nil, errTemporaryActivation
	}
	storage, err := projectReserveRegistration(&s, receipt, record)
	if err != nil {
		return nil, err
	}
	managers, err := a.c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil || verifyV2Runtime(s, managers, a.pools, v2ManagerImage) != nil {
		return nil, errTemporaryActivation
	}
	return storage, nil
}

// The unsealed addition must stay empty and scheduling-disabled. A sealed
// disk stays part of the runtime even when its scheduling flag is off.
func projectReserveRegistration(s *inventory, receipt object, record temporaryReserveRegistration) (object, error) {
	var storage object
	for i, item := range s.storageNodes {
		spec := valueObject(item, "spec", "disks")
		disk, exists := spec[temporaryReserveDiskName]
		owner := text(item, "metadata", "annotations", temporaryReserveOwner)
		if text(item, "metadata", "name") != record.Disk.Node {
			if exists || owner != "" || value(item, "status", "diskStatus", temporaryReserveDiskName) != nil {
				return nil, errTemporaryActivation
			}
			continue
		}
		if storage != nil || text(item, "metadata", "uid") != record.Disk.NodeUID || value(item, "metadata", "deletionTimestamp") != nil ||
			record.ActivationUID == "" || text(item, "metadata", "annotations", temporaryActivationOwner) != record.ActivationUID ||
			text(item, "status", "diskStatus", "longhorn-v2", "instanceManagerName") != record.Disk.InstanceManager {
			return nil, errTemporaryActivation
		}
		storage = item
		if !exists {
			if owner != "" || record.Disk.DiskUUID != "" || value(item, "status", "diskStatus", temporaryReserveDiskName) != nil {
				return nil, errTemporaryActivation
			}
			continue
		}
		schedulable, ok := value(valueObject(spec, temporaryReserveDiskName), "allowScheduling").(bool)
		if !ok || owner == "" || owner != text(receipt, "metadata", "uid") || !reviewedReserveDisk(disk) {
			return nil, errTemporaryActivation
		}
		if value(receipt, "immutable") == true {
			if record.Disk.DiskUUID == "" {
				return nil, errTemporaryActivation
			}
			s.reserve = &record.Disk
			continue
		}
		if schedulable || record.Disk.DiskUUID != "" {
			return nil, errTemporaryActivation
		}
		diskUUID := text(item, "status", "diskStatus", temporaryReserveDiskName, "diskUUID")
		diskPath := text(retainedReserveDiskSpec(false), "path")
		for _, node := range s.storageNodes {
			for key, status := range valueObject(node, "status", "diskStatus") {
				details, _ := status.(object)
				owned := text(node, "metadata", "uid") == record.Disk.NodeUID && key == temporaryReserveDiskName
				if !owned && diskUUID != "" && text(details, "diskUUID") == diskUUID {
					return nil, errTemporaryActivation
				}
			}
		}
		for _, replica := range s.replicas {
			if (diskUUID != "" && text(replica, "spec", "diskID") == diskUUID) ||
				(text(replica, "spec", "nodeID") == record.Disk.Node && text(replica, "spec", "diskPath") == diskPath) {
				return nil, errTemporaryActivation
			}
		}
		copyNode := maps.Clone(item)
		copySpec, copyStatus := maps.Clone(valueObject(item, "spec")), maps.Clone(valueObject(item, "status"))
		copyDisks, copyDiskStatus := maps.Clone(spec), maps.Clone(valueObject(copyStatus, "diskStatus"))
		delete(copyDisks, temporaryReserveDiskName)
		delete(copyDiskStatus, temporaryReserveDiskName)
		copySpec["disks"], copyStatus["diskStatus"] = copyDisks, copyDiskStatus
		copyNode["spec"], copyNode["status"] = copySpec, copyStatus
		s.storageNodes[i] = copyNode
	}
	if storage == nil {
		return nil, errTemporaryActivation
	}
	return storage, nil
}

func inactiveReserveDisk(storage object, planned cnpgRecoveryDiskBinding) (cnpgRecoveryDiskBinding, error) {
	status := valueObject(storage, "status", "diskStatus", temporaryReserveDiskName)
	uuid := text(status, "diskUUID")
	maximum, maxOK := nativeObservationUint(status["storageMaximum"])
	available, availableOK := nativeObservationUint(status["storageAvailable"])
	scheduled, scheduledOK := nativeObservationUint(status["storageScheduled"])
	replicas, replicasOK := status["scheduledReplica"].(object)
	backing, backingOK := status["scheduledBackingImage"].(object)
	if !isolationUID.MatchString(uuid) || (planned.DiskUUID != "" && planned.DiskUUID != uuid) ||
		uuid == text(storage, "status", "diskStatus", "longhorn-v2", "diskUUID") ||
		text(status, "instanceManagerName") != planned.InstanceManager ||
		text(status, "diskPath") != text(retainedReserveDiskSpec(false), "path") ||
		text(status, "diskType") != "block" || text(status, "diskDriver") != "aio" || !ready(status) ||
		!cnpgCondition(object{"status": status}, "Schedulable") ||
		!maxOK || !availableOK || !scheduledOK || maximum > permanentRawDiskGiB*recoveryGiB ||
		maximum < (permanentRawDiskGiB-1)*recoveryGiB || available > maximum || scheduled > maximum ||
		!replicasOK || (status["scheduledBackingImage"] != nil && !backingOK) || len(backing) != 0 {
		return cnpgRecoveryDiskBinding{}, errTemporaryActivation
	}
	// Only an unsealed disk must be empty. A resume has already matched the
	// sealed identity and every retained replica in the runtime snapshot.
	if planned.DiskUUID == "" && (maximum-available > 2*recoveryGiB || scheduled != 0 || len(replicas) != 0) {
		return cnpgRecoveryDiskBinding{}, errTemporaryActivation
	}
	planned.DiskUUID = uuid
	return planned, nil
}

func disabledReserveReplicasMatch(binding cnpgRecoveryDiskBinding, status object, replicas []object) bool {
	ledger, ok := status["scheduledReplica"].(object)
	maximum, maxOK := nativeObservationUint(status["storageMaximum"])
	scheduled, scheduledOK := nativeObservationUint(status["storageScheduled"])
	if !ok || !maxOK || !scheduledOK || maximum == 0 || maximum > permanentRawDiskGiB*recoveryGiB || scheduled > maximum {
		return false
	}
	path := text(status, "diskPath")
	seen := map[string]bool{}
	var total uint64
	for _, replica := range replicas {
		diskID := text(replica, "spec", "diskID")
		sameLocation := text(replica, "spec", "nodeID") == binding.Node && text(replica, "spec", "diskPath") == path
		if diskID != binding.DiskUUID && !sameLocation {
			continue
		}
		name := text(replica, "metadata", "name")
		size, err := strconv.ParseUint(text(replica, "spec", "volumeSize"), 10, 64)
		assigned, known := nativeObservationUint(ledger[name])
		if !recoveredNativeObject(replica, "longhorn.io/v1beta2", "Replica", "longhorn-system") ||
			diskID != binding.DiskUUID || !sameLocation || text(replica, "spec", "dataEngine") != "v2" ||
			name == "" || seen[name] || err != nil || size == 0 || size > maximum-total || !known || assigned != size {
			return false
		}
		seen[name] = true
		total += size
	}
	return len(seen) == len(ledger) && total == scheduled
}

func (a temporaryV2Activation) requireReserveRegistration(ctx context.Context, receipt object, record temporaryReserveRegistration) error {
	current, latest, err := readTemporaryReserveRegistration(ctx, a.c, false)
	if err != nil || benchmarkIdentity(current) != benchmarkIdentity(receipt) || latest != record ||
		text(current, "metadata", "resourceVersion") != text(receipt, "metadata", "resourceVersion") {
		return errTemporaryActivation
	}
	return sourceCurrent(ctx, a.c.run, a.source)
}

func (a temporaryV2Activation) reserveRegistrationPlan(ctx context.Context) (object, temporaryReserveRegistration, error) {
	if err := a.observeUnusedReserve(ctx); err != nil {
		return nil, temporaryReserveRegistration{}, err
	}
	qualified, err := a.observeRecoveryStorage(ctx, nil)
	if err != nil {
		return nil, temporaryReserveRegistration{}, err
	}
	target := qualified.Targets[temporaryReserveSelection]
	for _, disk := range qualified.Disks {
		if disk.Selection == temporaryReserveSelection && disk.DiskName == "" {
			disk.DiskName, disk.DiskUUID = temporaryReserveDiskName, ""
			record := temporaryReserveRegistration{Version: 1, Source: a.source, Run: a.owner, Disk: disk,
				ActivationUID: qualified.Activation.UID, PreparationUID: target.PreparationUID}
			data, err := json.Marshal(record)
			if err != nil {
				return nil, temporaryReserveRegistration{}, err
			}
			plan, err := a.c.create(ctx, a.source, object{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": object{"namespace": "longhorn-system", "name": temporaryReserveJournal},
				"data":     object{"record": string(data)},
			})
			if err != nil {
				return nil, temporaryReserveRegistration{}, err
			}
			return plan, record, a.requireReserveRegistration(ctx, plan, record)
		}
	}
	return nil, temporaryReserveRegistration{}, errTemporaryActivation
}

func (a temporaryV2Activation) registerRetainedReserve(ctx context.Context) (err error) {
	phase := "registration-record"
	defer func() {
		if err != nil {
			err = fmt.Errorf("retained reserve registration held: phase=%s: %w", phase, err)
		}
	}()
	if a.c.context != "admin@prod" || !validBenchmarkOwner(a.owner, a.source) {
		return errTemporaryActivation
	}
	receipt, record, err := readTemporaryReserveRegistration(ctx, a.c, true)
	if err != nil {
		return err
	}
	if receipt == nil {
		phase = "reserve-plan"
		receipt, record, err = a.reserveRegistrationPlan(ctx)
	} else if record.Run != a.owner || record.Source != a.source {
		err = verifyTemporaryReserveRun(ctx, a.c, record)
	}
	if err != nil {
		return err
	}
	phase = "storage-runtime"
	storage, err := a.reserveRegistrationSnapshot(ctx, receipt, record)
	if err != nil {
		if value(receipt, "immutable") == true {
			return a.disableUnprovenReserve(ctx, receipt, record, err)
		}
		return err
	}
	if value(storage, "spec", "disks", temporaryReserveDiskName) == nil {
		phase = "disabled-registration"
		if err := a.observeUnusedReserve(ctx); err != nil {
			return err
		}
		if err := a.requireReserveRegistration(ctx, receipt, record); err != nil {
			return err
		}
		if err := a.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", storage, retainedReserveRegistrationChanges(storage, text(receipt, "metadata", "uid"))...); err != nil {
			return err
		}
	}
	if value(storage, "spec", "disks", temporaryReserveDiskName, "allowScheduling") == true {
		return a.requireReserveRegistration(ctx, receipt, record)
	}
	phase = "ready-disk"
	var disk cnpgRecoveryDiskBinding
	if err := waitReadyWithin(ctx, 4*time.Minute, func(ctx context.Context) error {
		storage, err = a.reserveRegistrationSnapshot(ctx, receipt, record)
		if err != nil {
			return err
		}
		disk, err = inactiveReserveDisk(storage, record.Disk)
		return err
	}); err != nil {
		return err
	}
	if record.Disk.DiskUUID == "" {
		phase = "seal-identity"
		receipt, record, err = a.sealReserveRegistration(ctx, receipt, record, disk)
		if err != nil {
			return err
		}
	}
	phase = "enable-scheduling"
	storage, err = a.reserveRegistrationSnapshot(ctx, receipt, record)
	fresh, diskErr := inactiveReserveDisk(storage, record.Disk)
	if err != nil || diskErr != nil || fresh != record.Disk {
		return errTemporaryActivation
	}
	if err := a.requireReserveRegistration(ctx, receipt, record); err != nil {
		return err
	}
	path := "/spec/disks/" + temporaryReserveDiskName + "/allowScheduling"
	if err := a.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", storage,
		object{"op": "test", "path": path, "value": false},
		object{"op": "replace", "path": path, "value": true}); err != nil {
		return a.disableUnprovenReserve(ctx, receipt, record, err)
	}
	phase = "readback"
	err = waitReadyWithin(ctx, 4*time.Minute, func(ctx context.Context) error {
		current, err := a.reserveRegistrationSnapshot(ctx, receipt, record)
		if err != nil || value(current, "spec", "disks", temporaryReserveDiskName, "allowScheduling") != true {
			return errTemporaryActivation
		}
		return a.requireReserveRegistration(ctx, receipt, record)
	})
	if err != nil {
		return a.disableUnprovenReserve(ctx, receipt, record, err)
	}
	return nil
}

// Stop new placements if the post-enable native proof fails. Leave the disk,
// its identity receipt and any replicas in place for a protected retry.
func (a temporaryV2Activation) disableUnprovenReserve(ctx context.Context, receipt object, record temporaryReserveRegistration, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 75*time.Second)
	defer cancel()
	held := errors.New("the reserve scheduling rollback is unverified")
	if value(receipt, "immutable") != true || record.Disk.DiskUUID == "" || a.requireReserveRegistration(ctx, receipt, record) != nil {
		return errors.Join(cause, held)
	}
	read := func(ctx context.Context) (object, bool, error) {
		storage, err := a.c.get(ctx, "nodes.longhorn.io", "longhorn-system", record.Disk.Node, false)
		if err != nil {
			return nil, false, err
		}
		spec := valueObject(storage, "spec", "disks", temporaryReserveDiskName)
		enabled, ok := value(spec, "allowScheduling").(bool)
		if !recoveredNativeObject(storage, "longhorn.io/v1beta2", "Node", "longhorn-system") ||
			text(storage, "metadata", "uid") != record.Disk.NodeUID || text(storage, "metadata", "annotations", temporaryActivationOwner) != record.ActivationUID ||
			text(storage, "metadata", "annotations", temporaryReserveOwner) != text(receipt, "metadata", "uid") ||
			!ok || !reviewedBlockDisk(spec, retainedReserveDiskSpec(enabled)) {
			return nil, false, errors.Join(errReadinessTerminal, held)
		}
		return storage, enabled, nil
	}
	path := "/spec/disks/" + temporaryReserveDiskName + "/allowScheduling"
	err := waitReadyWithin(ctx, 60*time.Second, func(ctx context.Context) error {
		storage, enabled, err := read(ctx)
		if err != nil || !enabled {
			return err
		}
		if err := a.requireReserveRegistration(ctx, receipt, record); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		if err := a.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", storage,
			object{"op": "test", "path": path, "value": true},
			object{"op": "replace", "path": path, "value": false}); err != nil {
			return err
		}
		_, enabled, err = read(ctx)
		if err != nil || enabled {
			return held
		}
		return nil
	})
	if err != nil {
		return errors.Join(cause, held, err)
	}
	return errors.Join(cause, errors.New("reserve scheduling is disabled"))
}

func (a temporaryV2Activation) sealReserveRegistration(ctx context.Context, receipt object, record temporaryReserveRegistration, disk cnpgRecoveryDiskBinding) (object, temporaryReserveRegistration, error) {
	if err := a.requireReserveRegistration(ctx, receipt, record); err != nil {
		return nil, temporaryReserveRegistration{}, err
	}
	sealed := record
	sealed.Run, sealed.Source, sealed.Disk = a.owner, a.source, disk
	data, err := json.Marshal(sealed)
	if err != nil {
		return nil, temporaryReserveRegistration{}, err
	}
	if err := a.c.patch(ctx, "configmap", "longhorn-system", receipt,
		object{"op": "replace", "path": "/data/record", "value": string(data)},
		object{"op": "add", "path": "/immutable", "value": true}); err != nil {
		return nil, temporaryReserveRegistration{}, err
	}
	current, actual, err := readTemporaryReserveRegistration(ctx, a.c, false)
	if err != nil || benchmarkIdentity(current) != benchmarkIdentity(receipt) || actual != sealed || value(current, "immutable") != true {
		return nil, temporaryReserveRegistration{}, errTemporaryActivation
	}
	return current, actual, nil
}
