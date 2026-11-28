package main

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const recoveryGiB = uint64(1024 * 1024 * 1024)

// Captured after independent verification of the prepared encrypted raw
// partition and running instance-manager process. This is a continuity binding,
// not evidence that a caller may infer from the capacity response itself.
type cnpgRecoveryDiskBinding struct {
	Selection, Node, NodeUID, DiskUUID, InstanceManager string
	DiskName                                            string `json:",omitempty"`
}

// Checks one fresh allocation snapshot, not an atomic reservation. The caller
// must prove current 100% overprovisioning / 25% minimum-free settings, the V2
// runtime, complete inventories and exclusive proof allocation ownership; then
// re-read immediately before creation and verify all three actual replicas.
// Physical free space already includes snapshots and orphans: subtracting
// Volume.status.actualSize here would count existing storage twice.
func verifyRecoveryClaimCapacity(claimSize string, nodes, replicas, backingImages, shards []object, bindings []cnpgRecoveryDiskBinding) error {
	refused := errors.New("isolated recovery lacks current capacity on three bound raw disks")
	// The reviewed recoveries use whole GiB claims. This strict subset is already
	// above Longhorn's CSI minimum and aligned to its 2 MiB allocation boundary.
	// Other Kubernetes quantity forms require a reviewed sizing change.
	if !strings.HasSuffix(claimSize, "Gi") {
		return refused
	}
	units := strings.TrimSuffix(claimSize, "Gi")
	size, err := strconv.ParseUint(units, 10, 64)
	if err != nil || size == 0 || size > 96 || units != strconv.FormatUint(size, 10) ||
		(len(bindings) != 3 && len(bindings) != 4) || len(backingImages) != 0 || len(shards) != 0 {
		return refused
	}
	request := size * recoveryGiB
	nodeIndex, err := indexed(nodes)
	if err != nil {
		return refused
	}
	if _, err = indexed(replicas); err != nil {
		return refused
	}
	type capacityDisk struct {
		cnpgRecoveryDiskBinding
		name, path string
		maximum    uint64
	}
	byDisk := map[string]capacityDisk{}
	primaries := map[string]cnpgRecoveryDiskBinding{}
	byNode, byNodeUID := map[string]bool{}, map[string]bool{}
	specs := map[string]map[string]object{}
	var retained cnpgRecoveryDiskBinding
	for _, b := range bindings {
		if !slices.Contains(temporaryV2Selections[:], b.Selection) || b.Node == "" || b.NodeUID == "" ||
			b.DiskUUID == "" || b.InstanceManager == "" || byDisk[b.DiskUUID].DiskUUID != "" {
			return refused
		}
		diskName, rawID, maximum := "longhorn-v2", temporaryRawID(b.Selection), uint64(stagingRawDiskGiB)
		if b.DiskName == "" {
			if primaries[b.Selection].Node != "" || byNode[b.Node] || byNodeUID[b.NodeUID] {
				return refused
			}
			primaries[b.Selection], byNode[b.Node], byNodeUID[b.NodeUID] = b, true, true
		} else {
			if b.DiskName != "longhorn-v2-retained" || b.Selection != "temporary-v2-1" || retained.Node != "" {
				return refused
			}
			retained = b
			diskName, rawID, maximum = b.DiskName, "r-longhorn", permanentRawDiskGiB
		}
		expected := blockDisksFor(rawID)["longhorn-v2"].(object)
		if specs[b.Node] == nil {
			specs[b.Node] = map[string]object{}
		}
		specs[b.Node][diskName] = expected
		byDisk[b.DiskUUID] = capacityDisk{b, diskName, text(expected, "path"), maximum * recoveryGiB}
	}
	if len(primaries) != len(temporaryV2Selections) {
		return refused
	}
	if retained.Node != "" {
		primary := primaries[retained.Selection]
		if retained.Node != primary.Node || retained.NodeUID != primary.NodeUID || retained.InstanceManager != primary.InstanceManager {
			return refused
		}
	}
	for name, expected := range specs {
		node := nodeIndex[name]
		disks := valueObject(node, "spec", "disks")
		if len(disks) != len(expected) || len(valueObject(node, "status", "diskStatus")) != len(expected) {
			return refused
		}
		for diskName, spec := range expected {
			valid := reviewedBlockDisk(disks[diskName], spec)
			if diskName == temporaryReserveDiskName {
				valid = reviewedReserveDisk(disks[diskName])
			}
			if !valid {
				return refused
			}
		}
	}
	assigned := map[string]object{}
	for disk := range byDisk {
		assigned[disk] = object{}
	}
	for _, r := range replicas {
		engine, disk := text(r, "spec", "dataEngine"), text(r, "spec", "diskID")
		if engine == "v1" {
			if byDisk[disk].DiskUUID != "" {
				return refused
			}
			continue
		}
		b, bound := byDisk[disk]
		if engine != "v2" || !bound || text(r, "spec", "nodeID") != b.Node || text(r, "spec", "diskPath") != b.path {
			return refused
		}
		// Native accounting charges every assigned Replica, including failed,
		// stopped, inactive and retained copies. Do not filter by health/state.
		bytes, parseErr := strconv.ParseUint(text(r, "spec", "volumeSize"), 10, 64)
		if parseErr != nil || bytes == 0 || bytes > b.maximum {
			return refused
		}
		assigned[disk][text(r, "metadata", "name")] = float64(bytes)
	}
	eligibleHosts := map[string]bool{}
	for _, b := range byDisk {
		n := nodeIndex[b.Node]
		status, _ := value(n, "status").(object)
		d := valueObject(n, "status", "diskStatus", b.name)
		if text(n, "metadata", "uid") != b.NodeUID || value(n, "spec", "allowScheduling") != true || value(n, "spec", "evictionRequested") == true || !ready(status) || !ready(d) ||
			text(d, "diskUUID") != b.DiskUUID || text(d, "diskPath") != b.path || text(d, "diskType") != "block" || text(d, "diskDriver") != "aio" || text(d, "instanceManagerName") != b.InstanceManager {
			return refused
		}
		maximum, maxOK := nativeObservationUint(d["storageMaximum"])
		available, freeOK := nativeObservationUint(d["storageAvailable"])
		scheduled, scheduledOK := nativeObservationUint(d["storageScheduled"])
		ledger, ledgerOK := d["scheduledReplica"].(object)
		backing, backingOK := d["scheduledBackingImage"].(object)
		if !maxOK || !freeOK || !scheduledOK || maximum == 0 || maximum > b.maximum || available > maximum || scheduled > maximum || !ledgerOK || !reflect.DeepEqual(ledger, assigned[b.DiskUUID]) || (d["scheduledBackingImage"] != nil && !backingOK) || len(backing) != 0 {
			return refused
		}
		var total uint64
		for _, raw := range ledger {
			bytes, valid := nativeObservationUint(raw)
			if !valid || bytes == 0 || bytes > maximum || total > maximum-bytes {
				return refused
			}
			total += bytes
		}
		const reserve, margin = 4 * recoveryGiB, 2 * recoveryGiB
		if total != scheduled || maximum < reserve {
			return refused
		}
		if value(n, "spec", "disks", b.name, "allowScheduling") == false {
			continue
		}
		if !cnpgCondition(object{"status": d}, "Schedulable") {
			conditions, _ := value(d, "conditions").([]any)
			pressure := false
			for _, raw := range conditions {
				condition, _ := raw.(object)
				if text(condition, "type") == "Schedulable" && text(condition, "status") == "False" && text(condition, "reason") == "DiskPressure" {
					pressure = true
				}
			}
			if !pressure {
				return refused
			}
			continue
		}
		// Longhorn excludes disks below its native limit before choosing a
		// placement. Another disk on this host can cover the allocation.
		if scheduled+request > maximum-reserve || available <= maximum/4 {
			continue
		}
		// Stronger than the native scheduler: retain both reviewed reserves in
		// physical headroom as well as logical commitments. Strict > matches
		// Longhorn's physical free-space rule; SPDK usable maximum may be less
		// than the independently measured raw partition.
		if scheduled+request+margin > maximum-reserve || available <= request+reserve+margin || available-request-reserve-margin <= maximum/4 {
			return refused
		}
		eligibleHosts[b.Node] = true
	}
	if len(eligibleHosts) != len(temporaryV2Selections) {
		return refused
	}
	return nil
}
