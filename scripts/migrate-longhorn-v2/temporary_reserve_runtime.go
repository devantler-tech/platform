package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
)

const (
	temporaryReserveDiskName = "longhorn-v2-retained"
	temporaryReserveJournal  = "longhorn-v2-retained-reserve-registration"
	temporaryReserveOwner    = "storage.platform.devantler.tech/longhorn-v2-retained-reserve"
)

type temporaryReserveRegistration struct {
	Version                       int
	Source, Run                   string
	ActivationUID, PreparationUID string
	Disk                          cnpgRecoveryDiskBinding
}

// Bind only the optional retained disk. An inventory with the original three
// disks does not need another journal or native read.
func observedTemporaryReserve(ctx context.Context, c client, s inventory) (*cnpgRecoveryDiskBinding, error) {
	var storage object
	for _, item := range s.storageNodes {
		if _, exists := valueObject(item, "spec", "disks")[temporaryReserveDiskName]; exists {
			if storage != nil {
				return nil, errTemporaryActivation
			}
			storage = item
		}
	}
	if storage == nil {
		return nil, nil
	}
	cm, record, err := readTemporaryReserveRegistration(ctx, c, false)
	if err != nil || value(cm, "immutable") != true ||
		text(storage, "metadata", "annotations", temporaryReserveOwner) != text(cm, "metadata", "uid") {
		return nil, errTemporaryActivation
	}
	if record.Disk.Node != text(storage, "metadata", "name") || record.Disk.NodeUID != text(storage, "metadata", "uid") {
		return nil, errTemporaryActivation
	}
	if err := verifyTemporaryReserveRun(ctx, c, record); err != nil {
		return nil, err
	}
	if err := (temporaryV2Activation{c: c}).verifyTemporaryReserveNative(ctx, s, record); err != nil {
		return nil, err
	}
	return &record.Disk, nil
}

func readTemporaryReserveRegistration(ctx context.Context, c client, optional bool) (object, temporaryReserveRegistration, error) {
	cm, err := c.get(ctx, "configmap", "longhorn-system", temporaryReserveJournal, optional)
	if err != nil || cm == nil {
		return cm, temporaryReserveRegistration{}, err
	}
	var record temporaryReserveRegistration
	decoder := json.NewDecoder(strings.NewReader(text(cm, "data", "record")))
	decoder.DisallowUnknownFields()
	var trailing any
	if !recoveredNativeObject(cm, "v1", "ConfigMap", "longhorn-system") || text(cm, "metadata", "name") != temporaryReserveJournal ||
		decoder.Decode(&record) != nil || decoder.Decode(&trailing) != io.EOF || record.Version != 1 ||
		!validBenchmarkOwner(record.Run, record.Source) || record.Disk.Selection != temporaryReserveSelection ||
		record.Disk.DiskName != temporaryReserveDiskName || !inventoryTemporaryNode.MatchString(record.Disk.Node) ||
		!isolationUID.MatchString(record.Disk.NodeUID) || !isolationUID.MatchString(record.ActivationUID) ||
		!isolationUID.MatchString(record.PreparationUID) || !kubeName.MatchString(record.Disk.InstanceManager) ||
		(value(cm, "immutable") == true) != (record.Disk.DiskUUID != "") ||
		(record.Disk.DiskUUID != "" && !isolationUID.MatchString(record.Disk.DiskUUID)) {
		return nil, temporaryReserveRegistration{}, errTemporaryActivation
	}
	return cm, record, nil
}

func verifyTemporaryReserveRun(ctx context.Context, c client, record temporaryReserveRegistration) error {
	run, err := previousMigrationRun(ctx, c, record.Run, record.Source)
	// A protected attempt can end between a successful final patch and its
	// readback. The immutable receipt and current native checks still decide
	// whether the capacity can be counted after that attempt has ended.
	if err != nil || text(run, "path") != ".github/workflows/prepare-longhorn-v2-node.yaml" {
		return errTemporaryActivation
	}
	return nil
}

func (a temporaryV2Activation) verifyTemporaryReserveNative(ctx context.Context, s inventory, record temporaryReserveRegistration) error {
	activation, primary, err := a.read(ctx)
	if err != nil || !primary.Completed || record.ActivationUID != text(activation, "metadata", "uid") {
		return errTemporaryActivation
	}
	target := primary.Targets[temporaryReserveSelection]
	if target.Name != record.Disk.Node || target.LonghornUID != record.Disk.NodeUID ||
		target.PreparationUID != record.PreparationUID || record.PreparationUID == "" {
		return errTemporaryActivation
	}
	preparationConfig, err := a.c.get(ctx, "configmap", "longhorn-system", "longhorn-v2-prepare-"+temporaryReserveSelection, false)
	if err != nil || text(preparationConfig, "metadata", "uid") != target.PreparationUID {
		return errTemporaryActivation
	}
	completed, err := preparedTemporaryBinding(preparationConfig, target)
	if err != nil || completed.Schema != 3 {
		return errTemporaryActivation
	}
	nodes, err := indexed(s.nodes)
	if err != nil || text(nodes[target.Name], "status", "nodeInfo", "bootID") != target.BootID {
		return errTemporaryActivation
	}
	address, err := verifyNode(nodes[target.Name], completed, true)
	if err != nil || a.c.verifyHost(ctx, address, nodes[target.Name]) != nil {
		return errTemporaryActivation
	}
	probe := temporaryPreparation{preparation: preparation{c: a.c, j: completed}, reader: a.reader}
	layout, err := probe.readRetainedRecovery(ctx, address, target.BootID, false)
	if err != nil || layout.EphemeralUUID != target.EphemeralUUID || layout.StagePartitionUUID != target.RawPartitionUUID {
		return errTemporaryActivation
	}
	return nil
}
