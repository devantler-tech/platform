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
	if err != nil || size == 0 || size > 96 || units != strconv.FormatUint(size, 10) || len(bindings) != 3 || len(backingImages) != 0 || len(shards) != 0 {
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
	byNode, byDisk := map[string]cnpgRecoveryDiskBinding{}, map[string]cnpgRecoveryDiskBinding{}
	bySelection := map[string]bool{}
	paths := map[string]string{}
	for _, b := range bindings {
		if !slices.Contains(temporaryV2Selections[:], b.Selection) || bySelection[b.Selection] ||
			b.Node == "" || b.NodeUID == "" || b.DiskUUID == "" || b.InstanceManager == "" || byNode[b.Node].Node != "" || byDisk[b.DiskUUID].DiskUUID != "" {
			return refused
		}
		bySelection[b.Selection] = true
		paths[b.DiskUUID] = text(blockDisksFor(temporaryRawID(b.Selection)), "longhorn-v2", "path")
		byNode[b.Node], byDisk[b.DiskUUID] = b, b
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
		if engine != "v2" || !bound || text(r, "spec", "nodeID") != b.Node || text(r, "spec", "diskPath") != paths[disk] {
			return refused
		}
		// Native accounting charges every assigned Replica, including failed,
		// stopped, inactive and retained copies. Do not filter by health/state.
		bytes, parseErr := strconv.ParseUint(text(r, "spec", "volumeSize"), 10, 64)
		if parseErr != nil || bytes == 0 || bytes > stagingRawDiskGiB*recoveryGiB {
			return refused
		}
		assigned[disk][text(r, "metadata", "name")] = float64(bytes)
	}
	for _, b := range bindings {
		n := nodeIndex[b.Node]
		status, _ := value(n, "status").(object)
		disks, _ := value(n, "status", "diskStatus").(object)
		d, _ := disks["longhorn-v2"].(object)
		if text(n, "metadata", "uid") != b.NodeUID || value(n, "spec", "allowScheduling") != true || value(n, "spec", "evictionRequested") == true || !reviewedBlockDisksFor(value(n, "spec", "disks"), temporaryRawID(b.Selection)) || !ready(status) || len(disks) != 1 || !ready(d) ||
			!cnpgCondition(object{"status": d}, "Schedulable") || text(d, "diskUUID") != b.DiskUUID || text(d, "diskPath") != paths[b.DiskUUID] || text(d, "diskType") != "block" || text(d, "diskDriver") != "aio" || text(d, "instanceManagerName") != b.InstanceManager {
			return refused
		}
		maximum, maxOK := nativeObservationUint(d["storageMaximum"])
		available, freeOK := nativeObservationUint(d["storageAvailable"])
		scheduled, scheduledOK := nativeObservationUint(d["storageScheduled"])
		ledger, ledgerOK := d["scheduledReplica"].(object)
		backing, backingOK := d["scheduledBackingImage"].(object)
		if !maxOK || !freeOK || !scheduledOK || maximum == 0 || maximum > stagingRawDiskGiB*recoveryGiB || available > maximum || scheduled > maximum || !ledgerOK || !reflect.DeepEqual(ledger, assigned[b.DiskUUID]) || (d["scheduledBackingImage"] != nil && !backingOK) || len(backing) != 0 {
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
		// Stronger than the native scheduler: retain both reviewed reserves in
		// physical headroom as well as logical commitments. Strict > matches
		// Longhorn's physical free-space rule; SPDK usable maximum may be less
		// than the independently measured raw partition.
		if total != scheduled || maximum < reserve || scheduled+request+margin > maximum-reserve || available <= request+reserve+margin || available-request-reserve-margin <= maximum/4 {
			return refused
		}
	}
	return nil
}
