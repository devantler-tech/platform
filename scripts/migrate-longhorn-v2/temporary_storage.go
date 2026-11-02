package main

import (
	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	"strings"
)

// Check the complete native block-resource inventory, never a CLI error string.
func verifyUnusedTemporaryPartition(statuses, discovered cosiresource.List, ephemeralUUID string) error {
	if ephemeralUUID == "" || len(statuses.Items) == 0 || len(statuses.Items) > 128 || len(discovered.Items) == 0 || len(discovered.Items) > 512 {
		return errInventory
	}
	seen := map[string]bool{}
	var original *block.VolumeStatusSpec
	for _, item := range statuses.Items {
		volume, ok := item.(*block.VolumeStatus)
		if !ok || volume == nil || volume.Metadata().Namespace() != block.NamespaceName || volume.Metadata().Phase() != cosiresource.PhaseRunning || seen[volume.Metadata().ID()] {
			return errInventory
		}
		id := volume.Metadata().ID()
		seen[id] = true
		spec := volume.TypedSpec()
		if id == "EPHEMERAL" {
			original = spec
		}
		if strings.HasPrefix(id, "r-") || strings.HasPrefix(id, "u-") {
			if id != "r-longhorn" || spec.Phase != block.VolumePhaseWaiting || spec.Location != "" || spec.MountLocation != "" || spec.ParentLocation != "" || spec.UUID != "" || spec.PartitionUUID != "" || spec.PartitionIndex != 0 || spec.Size != 0 {
				return errInventory
			}
		}
	}
	if original == nil || original.Phase != block.VolumePhaseReady || original.Type != block.VolumeTypePartition || original.UUID != ephemeralUUID ||
		original.Location == "" || original.ParentLocation == "" || !strings.HasPrefix(original.MountLocation, "/dev/dm-") ||
		original.EncryptionProvider != block.EncryptionProviderLUKS2 || !original.EncryptionLockedToState ||
		len(original.ConfiguredEncryptionKeys) != 1 || original.ConfiguredEncryptionKeys[0] != "nodeID" || original.Size < 136<<30 {
		return errInventory
	}
	seen = map[string]bool{}
	found := false
	for _, item := range discovered.Items {
		volume, ok := item.(*block.DiscoveredVolume)
		if !ok || volume == nil || volume.Metadata().Namespace() != block.NamespaceName || volume.Metadata().Phase() != cosiresource.PhaseRunning || seen[volume.Metadata().ID()] {
			return errInventory
		}
		seen[volume.Metadata().ID()] = true
		spec := volume.TypedSpec()
		if spec.DevPath == "" || spec.Size == 0 || strings.HasPrefix(spec.PartitionLabel, "r-") || strings.HasPrefix(spec.PartitionLabel, "u-") {
			return errInventory
		}
		if spec.PartitionLabel == "EPHEMERAL" {
			if found || spec.DevPath != original.Location || spec.ParentDevPath != original.ParentLocation || spec.Size != original.Size || spec.PartitionUUID == "" {
				return errInventory
			}
			found = true
		}
	}
	if !found {
		return errInventory
	}
	return nil
}
