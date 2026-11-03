package main

import (
	"context"
	"net/netip"
	"strings"
	"time"

	cosiresource "github.com/cosi-project/runtime/pkg/resource"
	"github.com/devantler-tech/platform/internal/longhorninventory"
	talos "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

type unusedTemporaryReader struct {
	nativeReplicaReader
	blocks                 longhorninventory.BlockCollections
	address, ephemeralUUID string
}

// Both native block inventories are collected inside the existing hardware and
// boot bracket on EACH file observation. A partial resource list never clears
// a reset, even when its returned prefix happens to contain the original disk.
func (r unusedTemporaryReader) ListReplicas(ctx context.Context) (longhorninventory.Listing, error) {
	if err := r.verifyPartition(ctx); err != nil {
		return longhorninventory.Listing{}, err
	}
	return r.nativeReplicaReader.ListReplicas(ctx)
}

func (r unusedTemporaryReader) ListStorageRoot(ctx context.Context) (longhorninventory.Listing, error) {
	if err := r.verifyPartition(ctx); err != nil {
		return longhorninventory.Listing{}, err
	}
	reader, ok := r.nativeReplicaReader.(nativeStorageRootReader)
	if !ok {
		return longhorninventory.Listing{}, errInventory
	}
	return reader.ListStorageRoot(ctx)
}

func (r unusedTemporaryReader) ListBlockLayout(ctx context.Context) (longhorninventory.BlockLayoutReport, error) {
	reader, ok := r.nativeReplicaReader.(nativeBlockLayoutReader)
	if !ok {
		return longhorninventory.BlockLayoutReport{}, errInventory
	}
	return reader.ListBlockLayout(ctx)
}

func (r unusedTemporaryReader) verifyPartition(ctx context.Context) error {
	address, addressErr := netip.ParseAddr(r.address)
	if ctx.Err() != nil || r.blocks == nil || r.nativeReplicaReader == nil || r.ephemeralUUID == "" || addressErr != nil || address.String() != r.address || !netip.MustParsePrefix("10.0.0.0/16").Contains(address) {
		return errInventory
	}
	ctx, cancel := context.WithTimeout(talos.WithNode(ctx, r.address), 20*time.Second)
	defer cancel()
	statuses, err := r.blocks.List(ctx, block.NewVolumeStatus(block.NamespaceName, "").Metadata())
	if err != nil || ctx.Err() != nil {
		return errInventory
	}
	discovered, err := r.blocks.List(ctx, block.NewDiscoveredVolume(block.NamespaceName, "").Metadata())
	if err != nil || ctx.Err() != nil || verifyUnusedTemporaryPartition(statuses, discovered, r.ephemeralUUID) != nil {
		return errInventory
	}
	return nil
}

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
			unallocated := spec.Phase == block.VolumePhaseWaiting || spec.Phase == block.VolumePhaseFailed && (spec.PreFailPhase == block.VolumePhaseWaiting || spec.PreFailPhase == block.VolumePhaseMissing)
			if id != "r-longhorn" || !unallocated || spec.Type != block.VolumeTypePartition || spec.Location != "" || spec.MountLocation != "" || spec.ParentLocation != "" || spec.UUID != "" || spec.PartitionUUID != "" || spec.PartitionIndex != 0 || spec.Size != 0 {
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
