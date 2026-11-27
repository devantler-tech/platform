package main

import (
	"errors"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

// Verify the original extent on the single prepared staging worker. This
// only establishes disk identity. Registration still requires an exclusive
// allocation, a fresh native read bracket, and multi-disk scheduler checks.
func verifyUnusedTemporaryReserve(current temporaryV2Binding, prepared journal, layout longhorninventory.RetainedLayoutIdentity,
	raw, stage, ephemeral object, discovered []object) error {
	unverified := errors.New("the unused temporary storage extent is unverified")
	if current.Selection != "temporary-v2-1" || prepared.Schema != 3 || prepared.Phase != "prepared" ||
		!prepared.StagingLayout || prepared.TemporarySelection != current.Selection || prepared.Target != current.Name ||
		prepared.NodeUID != current.NodeUID || prepared.ProviderID != current.ProviderID || prepared.MachineUUID != current.MachineUUID ||
		prepared.LonghornUID != current.LonghornUID || len(prepared.OriginalDisks) != 0 ||
		!inventoryBoot.MatchString(prepared.OriginalBootID) || prepared.OriginalBootID == current.BootID ||
		!retentionDigest.MatchString(prepared.RetainedExtentDigest) || layout.ExtentDigest != prepared.RetainedExtentDigest ||
		!isolationUID.MatchString(prepared.RetainedRawUUID) || layout.PartitionUUID != prepared.RetainedRawUUID ||
		layout.EphemeralUUID != current.EphemeralUUID || !layout.StageStatusPresent || layout.StagePartitionUUID != current.RawPartitionUUID ||
		prepared.PreparedRawUUID != current.RawPartitionUUID || prepared.RetainedRawUUID == prepared.PreparedRawUUID {
		return unverified
	}

	rawMapper := text(raw, "spec", "mountLocation")
	if verifyNamedEncryptedRawDisk(raw, ephemeral, "r-longhorn", permanentRawDiskGiB) != nil ||
		verifyNamedEncryptedRawDisk(stage, ephemeral, temporaryRawID(current.Selection), stagingRawDiskGiB) != nil ||
		text(raw, "spec", "partitionUUID") != prepared.RetainedRawUUID ||
		text(stage, "spec", "partitionUUID") != prepared.PreparedRawUUID ||
		text(raw, "spec", "location") == text(stage, "spec", "location") || rawMapper == text(stage, "spec", "mountLocation") {
		return unverified
	}

	matches := 0
	for _, device := range discovered {
		if text(device, "spec", "dev_path") != rawMapper {
			continue
		}
		if text(device, "metadata", "type") != "DiscoveredVolumes.block.talos.dev" || text(device, "spec", "type") != "disk" ||
			!absentOrEmptyString(value(device, "spec", "name")) || !absentOrEmptyString(value(device, "spec", "label")) ||
			!absentOrEmptyString(value(device, "spec", "uuid")) {
			return unverified
		}
		matches++
	}
	if matches != 1 {
		return unverified
	}
	return nil
}
