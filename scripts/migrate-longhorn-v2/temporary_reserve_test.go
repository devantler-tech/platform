package main

import (
	"strings"
	"testing"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

func TestReservedCapacityMatchesTheUnusedPreparedExtent(t *testing.T) {
	const oldUUID = "12345678-1234-4234-8234-000000000009"
	for _, fault := range []string{"", "changed extent", "filesystem identity differs", "wrong temporary pool", "previously registered",
		"active mapper reused", "prepared UUID reused", "missing encryption", "filesystem signature discovered"} {
		t.Run(fault, func(t *testing.T) {
			node, storage, stage, ephemeral, pool := temporaryRawBindingFixture()
			pool.role, pool.name, pool.count = "v2", "autoscale-longhorn-v2", 1
			name := "autoscale-longhorn-v2-1234567890abcdef"
			valueObject(node, "metadata")["name"] = name
			valueObject(node, "metadata", "labels")[migrationPoolLabel] = "v2"
			valueObject(storage, "metadata")["name"] = name
			valueObject(stage, "metadata")["id"] = "r-longhorn-stage"
			boot := text(node, "status", "nodeInfo", "bootID")
			binding, err := projectTemporaryV2Binding("temporary-v2-1", node, storage, stage, ephemeral, pool, boot, text(ephemeral, "spec", "uuid"))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := rawFixture()
			valueObject(raw, "spec")["location"] = "/dev/sda7"
			valueObject(raw, "spec")["mountLocation"] = "/dev/dm-7"
			valueObject(raw, "spec")["partitionUUID"] = oldUUID
			record := journal{
				Schema: 3, Phase: "prepared", Target: name, TemporarySelection: binding.Selection, NodeUID: binding.NodeUID,
				ProviderID: binding.ProviderID, MachineUUID: binding.MachineUUID, LonghornUID: binding.LonghornUID,
				PreparedRawUUID: binding.RawPartitionUUID, RetainedRawUUID: oldUUID, RetainedExtentDigest: strings.Repeat("a", 64),
				OriginalDisks: object{}, StagingLayout: true, OriginalBootID: "12345678-1234-4234-8234-000000000010",
			}
			native := longhorninventory.RetainedLayoutIdentity{
				PartitionUUID: oldUUID, ExtentDigest: record.RetainedExtentDigest, EphemeralUUID: binding.EphemeralUUID,
				StageStatusPresent: true, StagePartitionUUID: binding.RawPartitionUUID,
			}
			discovered := []object{{"metadata": object{"type": "DiscoveredVolumes.block.talos.dev"},
				"spec": object{"type": "disk", "dev_path": "/dev/dm-7"}}}
			switch fault {
			case "changed extent":
				native.ExtentDigest = strings.Repeat("b", 64)
			case "filesystem identity differs":
				native.EphemeralUUID = oldUUID
			case "wrong temporary pool":
				binding.Selection = "temporary-v1-1"
			case "previously registered":
				record.OriginalDisks = object{"stray": object{}}
			case "active mapper reused":
				valueObject(raw, "spec")["mountLocation"] = text(stage, "spec", "mountLocation")
			case "prepared UUID reused":
				native.PartitionUUID = binding.RawPartitionUUID
			case "missing encryption":
				valueObject(raw, "spec")["encryptionProvider"] = ""
			case "filesystem signature discovered":
				valueObject(discovered[0], "spec")["name"] = "xfs"
			}
			if err := verifyUnusedTemporaryReserve(binding, record, native, raw, stage, ephemeral, discovered); (err != nil) != (fault != "") {
				t.Fatalf("reserve proof for %q: %v", fault, err)
			}
		})
	}
}
