package main

import (
	"context"
	"errors"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/devantler-tech/platform/internal/longhornmigration"
)

const closeRetainedRawConfig = `apiVersion: v1alpha1
kind: RawVolumeConfig
name: longhorn
$patch: delete
`

func (p temporaryPreparation) retainsRaw() bool {
	return p.temporarySelection == "temporary-v2-1"
}

// The staging and root configuration must already be loaded by the reviewed
// deployment. This operation only closes and later restores the old definition.
func (p temporaryPreparation) verifyRetainedLayoutConfig(ctx context.Context, address string, oldOpen bool) error {
	staged, err := p.c.talosResource(ctx, address, "volumeconfig", longhornmigration.RetainedStageRawID)
	if err != nil || !retainedConfigRange(staged, stagingRawDiskGiB, stagingRawDiskGiB) {
		return errInventory
	}
	ephemeral, err := p.c.talosResource(ctx, address, "volumeconfig", "EPHEMERAL")
	if err != nil || !retainedConfigRange(ephemeral, 40, 44) {
		return errInventory
	}
	if oldOpen {
		raw, err := p.c.talosResource(ctx, address, "volumeconfig", "r-longhorn")
		if err != nil || !retainedConfigRange(raw, permanentRawDiskGiB, permanentRawDiskGiB) {
			return errInventory
		}
	}
	return nil
}

func retainedConfigRange(config object, minGiB, maxGiB uint64) bool {
	return verifyEncryptedPartitionConfig(config) == nil &&
		value(config, "spec", "provisioning", "partitionSpec", "minSize") == float64(minGiB<<30) &&
		value(config, "spec", "provisioning", "partitionSpec", "maxSize") == float64(maxGiB<<30) &&
		value(config, "spec", "provisioning", "partitionSpec", "negativeMaxSize") != true
}

func (p temporaryPreparation) observeRetainedPreparation(ctx context.Context, owned, closed bool) (storageRootReceipt, error) {
	scope := storageLayout
	if closed {
		scope = storageRetainedClosed
	}
	receipt, err := p.observeTemporaryStorage(ctx, owned, scope)
	if err != nil {
		return storageRootReceipt{}, err
	}
	layout, identity := receipt.BlockLayout, receipt.retainedLayout
	// 144 GiB for new storage, 44 GiB for root and 8 GiB for placement.
	if layout.EphemeralPhase != "ready" || !layout.EphemeralLUKS2 || !layout.EphemeralLockedToState ||
		!layout.EphemeralNodeIDOnly || layout.EphemeralSizeBytes < 196<<30 ||
		identity.EphemeralUUID != p.j.EphemeralUUID || identity.StagePartitionUUID != "" || !identity.StageStatusPresent ||
		(p.j.RetainedRawUUID != "" && !p.sameRetainedExtent(identity)) {
		return storageRootReceipt{}, errInventory
	}
	return receipt, nil
}

func (p temporaryPreparation) sameRetainedExtent(identity longhorninventory.RetainedLayoutIdentity) bool {
	return identity.PartitionUUID == p.j.RetainedRawUUID && identity.ExtentDigest == p.j.RetainedExtentDigest
}

func (p temporaryPreparation) closeRetainedForReset(ctx context.Context, address string) error {
	if p.j.Schema != 3 || p.j.Phase != "draining" || p.verifyRetainedLayoutConfig(ctx, address, false) != nil {
		return errInventory
	}
	if _, err := p.observeRetainedPreparation(ctx, true, true); err == nil {
		return nil
	}
	if _, err := p.observeRetainedPreparation(ctx, true, false); err != nil {
		return err
	}
	if err := p.verifyRetainedLayoutConfig(ctx, address, true); err != nil {
		return err
	}
	if err := p.checkOwner(ctx); err != nil {
		return err
	}
	if _, err := p.c.run(ctx, nil, "talosctl", "--nodes="+address, "patch", "machineconfig", "--mode=no-reboot", "--patch="+closeRetainedRawConfig); err != nil {
		return errors.New("the original raw mapper closure could not be confirmed")
	}
	return waitReadyWithin(ctx, time.Minute, func(ctx context.Context) error {
		_, err := p.observeRetainedPreparation(ctx, true, true)
		return err
	})
}

// Read only after a new boot. An absent old mapper can qualify the physical
// recovery, while the completed journal always requires it to be reopened.
func (p temporaryPreparation) readRetainedRecovery(ctx context.Context, address, boot string, closed bool) (identity longhorninventory.RetainedLayoutIdentity, err error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	factory := p.reader
	if factory == nil {
		factory = newInventoryReader
	}
	reader, closeReader, err := factory(ctx, address)
	if err != nil || reader == nil || closeReader == nil {
		return longhorninventory.RetainedLayoutIdentity{}, errInventory
	}
	defer func() {
		if closeReader() != nil || !commandContextActive(ctx) {
			identity, err = longhorninventory.RetainedLayoutIdentity{}, errInventory
		}
	}()
	before, err := reader.BootID(ctx)
	layoutReader, ok := reader.(nativeRetainedBlockLayoutReader)
	if err != nil || before != boot || before == p.j.OriginalBootID || !ok {
		return longhorninventory.RetainedLayoutIdentity{}, errInventory
	}
	if err = p.verifyRetainedLayoutConfig(ctx, address, !closed); err != nil {
		return longhorninventory.RetainedLayoutIdentity{}, err
	}
	layout, identity, err := layoutReader.ListRetainedBlockLayout(ctx, closed)
	if err != nil || !layout.RawExtentObserved || !p.sameRetainedExtent(identity) ||
		!inventoryBoot.MatchString(identity.StagePartitionUUID) || identity.EphemeralUUID == "" ||
		identity.EphemeralUUID == p.j.EphemeralUUID || layout.EphemeralPhase != "ready" ||
		layout.EphemeralSizeBytes < 40<<30 || layout.EphemeralSizeBytes > 44<<30 ||
		!layout.EphemeralLUKS2 || !layout.EphemeralLockedToState || !layout.EphemeralNodeIDOnly ||
		(p.j.PreparedRawUUID != "" && p.j.PreparedRawUUID != identity.StagePartitionUUID) {
		return longhorninventory.RetainedLayoutIdentity{}, errInventory
	}
	after, err := reader.BootID(ctx)
	if err != nil || after != before {
		return longhorninventory.RetainedLayoutIdentity{}, errInventory
	}
	return identity, nil
}

func (p temporaryPreparation) reopenRetained(ctx context.Context) (string, error) {
	node, address, err := p.node(ctx, true)
	if err != nil || p.j.Schema != 3 || p.j.Phase != "reset-requested" {
		return "", errInventory
	}
	boot := text(node, "status", "nodeInfo", "bootID")
	open, err := p.readRetainedRecovery(ctx, address, boot, false)
	if err == nil {
		return open.StagePartitionUUID, nil
	}
	closed, err := p.readRetainedRecovery(ctx, address, boot, true)
	if err != nil {
		return "", err
	}
	current, currentAddress, err := p.node(ctx, true)
	if err != nil || currentAddress != address || text(current, "status", "nodeInfo", "bootID") != boot ||
		value(current, "spec", "unschedulable") != true || text(current, "metadata", "annotations", fenceAnnotation) != p.name {
		return "", errInventory
	}
	storage, err := p.storageNode(ctx)
	if err != nil || !unusedStorageFenced(storage) {
		return "", errInventory
	}
	if err = p.checkOwner(ctx); err != nil {
		return "", err
	}
	if _, err = p.c.run(ctx, nil, "talosctl", "--nodes="+address, "patch", "machineconfig", "--mode=no-reboot",
		"--patch=@talos/workers/reserve-longhorn-data.yaml"); err != nil {
		return "", errors.New("the original raw mapper reopening could not be confirmed")
	}
	err = waitReadyWithin(ctx, time.Minute, func(ctx context.Context) error {
		open, err = p.readRetainedRecovery(ctx, address, boot, false)
		if err != nil || open.StagePartitionUUID != closed.StagePartitionUUID {
			return errInventory
		}
		return nil
	})
	return open.StagePartitionUUID, err
}
