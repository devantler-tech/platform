package main

import (
	"reflect"
	"strings"
)

type nativeRestoreTargetReceipt struct {
	Node, Storage                     benchmarkResource
	ProviderID, MachineUUID, DiskUUID string
	Preparation                       nativeRestorePreparationReceipt
}

// Bind Kubernetes, Longhorn and disk identities without admitting a write.
// The executor must separately qualify native host geometry, V2 runtime and
// capacity, then compare these receipts again before and after attachment.
func qualifyNativeRestoreTargets(targets []nativeRestoreTarget, nodes, storage []object, tag string, tagged bool, config []byte) (map[string]nativeRestoreTargetReceipt, error) {
	if !nativeRestoreFixedRoster(targets) || !strings.HasPrefix(tag, "longhorn-native-v2-") || !benchmarkRunPattern.MatchString(strings.TrimPrefix(tag, "longhorn-native-v2-")) {
		return nil, errNativeRestoreEvidence
	}
	byNode, err := indexed(nodes)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	byStorage, err := indexed(storage)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	pools, err := configuredMigrationPools(config)
	if err != nil {
		return nil, errNativeRestoreEvidence
	}
	result := map[string]nativeRestoreTargetReceipt{}
	uids := map[string]bool{targets[0].Preparation.Activation.UID: true}
	providers, machines, disks := map[string]bool{}, map[string]bool{}, map[string]bool{}
	partitions, boots := map[string]bool{}, map[string]bool{}
	previous := ""
	for _, target := range targets {
		n, lh := byNode[target.Name], byStorage[target.Name]
		mapper, profileErr := target.Preparation.mapper(target.Name)
		selection := target.Preparation.selection()
		name, _, rosterErr := resolveTemporaryInventoryTarget(selection, nodes, pools)
		registered, registrationErr := temporaryRegistrationState(lh, target.Preparation.Activation.UID, selection)
		if profileErr != nil || rosterErr != nil || name != target.Name || registrationErr != nil || !registered ||
			!kubeName.MatchString(target.Name) || len(target.Name) > 63 || target.Name <= previous || !nativeRestoreUID(target.UID) ||
			text(n, "apiVersion") != "v1" || text(n, "kind") != "Node" || text(n, "metadata", "uid") != target.UID ||
			text(n, "metadata", "annotations", capacityOwnerAnnotation) != capacityOwnerValue || text(n, "metadata", "annotations", scaleDownDisabled) != "true" ||
			text(n, "metadata", "resourceVersion") == "" || !hugepagesReady(n) || value(n, "spec", "unschedulable") == true ||
			text(n, "metadata", "annotations", fenceAnnotation) != "" || text(n, "status", "nodeInfo", "architecture") != "amd64" || text(n, "status", "nodeInfo", "bootID") != target.Preparation.BootID ||
			text(lh, "apiVersion") != "longhorn.io/v1beta2" || text(lh, "kind") != "Node" || text(lh, "metadata", "namespace") != "longhorn-system" ||
			text(lh, "metadata", "resourceVersion") == "" || value(lh, "metadata", "deletionTimestamp") != nil {
			return nil, errNativeRestoreEvidence
		}
		if _, controlPlane := valueObject(n, "metadata", "labels")["node-role.kubernetes.io/control-plane"]; controlPlane {
			return nil, errNativeRestoreEvidence
		}
		owner := text(lh, "metadata", "annotations", benchmarkOwner)
		tags := value(lh, "spec", "tags")
		if tagged && (owner != tag || !reflect.DeepEqual(tags, []any{tag})) || !tagged && (owner != "" || tags != nil && !reflect.DeepEqual(tags, []any{})) {
			return nil, errNativeRestoreEvidence
		}
		disk := valueObject(lh, "status", "diskStatus", "longhorn-v2")
		if !nativeRestoreDiskIdentityUsable(lh, disk) || text(disk, "diskPath") != mapper || text(disk, "diskType") != "block" ||
			text(disk, "diskDriver") != "aio" {
			return nil, errNativeRestoreEvidence
		}
		receipt := nativeRestoreTargetReceipt{Node: benchmarkIdentity(n), Storage: benchmarkIdentity(lh), ProviderID: text(n, "spec", "providerID"), MachineUUID: text(n, "status", "nodeInfo", "systemUUID"), DiskUUID: text(disk, "diskUUID"), Preparation: target.Preparation}
		if receipt.ProviderID != target.Preparation.ProviderID || receipt.MachineUUID != target.Preparation.MachineUUID ||
			receipt.Storage.UID != target.Preparation.LonghornUID || receipt.DiskUUID != target.Preparation.DiskUUID {
			return nil, errNativeRestoreEvidence
		}
		if !nativeRestoreUID(receipt.Storage.UID) || uids[receipt.Node.UID] || uids[receipt.Storage.UID] || receipt.Node.UID == receipt.Storage.UID ||
			!nativeRestoreUID(receipt.ProviderID) || !nativeRestoreUID(receipt.MachineUUID) || !nativeRestoreUID(receipt.DiskUUID) ||
			providers[receipt.ProviderID] || machines[receipt.MachineUUID] || disks[receipt.DiskUUID] ||
			uids[receipt.Preparation.Journal.UID] || receipt.Preparation.Journal.UID == receipt.Node.UID || receipt.Preparation.Journal.UID == receipt.Storage.UID ||
			partitions[receipt.Preparation.RawPartitionUUID] || boots[receipt.Preparation.BootID] || receipt.Preparation.RawPartitionUUID == receipt.DiskUUID {
			return nil, errNativeRestoreEvidence
		}
		uids[receipt.Node.UID], uids[receipt.Storage.UID], providers[receipt.ProviderID], machines[receipt.MachineUUID], disks[receipt.DiskUUID] = true, true, true, true, true
		uids[receipt.Preparation.Journal.UID], partitions[receipt.Preparation.RawPartitionUUID], boots[receipt.Preparation.BootID] = true, true, true
		previous, result[target.Name] = target.Name, receipt
	}
	return result, nil
}
