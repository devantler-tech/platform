package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
	"github.com/devantler-tech/platform/internal/longhornmigration"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
)

//go:embed retained-staging-layout.yaml
var retainedStagingLayout string

const closeRetainedRawConfig = `apiVersion: v1alpha1
kind: RawVolumeConfig
name: longhorn
$patch: delete
`

func (p temporaryPreparation) retainsRaw() bool {
	return p.temporarySelection == "temporary-v2-1"
}

func (p temporaryPreparation) verifyRetainedLayoutConfig(ctx context.Context, address string, oldOpen bool) error {
	configs, err := p.readRetainedVolumeConfigs(ctx, address)
	if err != nil ||
		!retainedConfigRange(configs[longhornmigration.RetainedStageRawID], stagingRawDiskGiB, stagingRawDiskGiB) ||
		!retainedConfigRange(configs["EPHEMERAL"], 40, 44) {
		return errInventory
	}
	raw := configs["r-longhorn"]
	if oldOpen && !retainedConfigRange(raw, permanentRawDiskGiB, permanentRawDiskGiB) || !oldOpen && raw != nil {
		return errInventory
	}
	return nil
}

// Read the complete native configuration. An unknown user volume could claim
// the freed root extent after reset even when it has no physical partition yet.
func (p temporaryPreparation) readRetainedVolumeConfigs(ctx context.Context, address string) (map[string]object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	payload, err := p.c.run(ctx, nil, "talosctl", "--nodes="+address, "get", "volumeconfig", "--output=json")
	if err != nil || !commandContextActive(ctx) || len(payload) > 512<<10 {
		return nil, errInventory
	}
	configs := map[string]object{}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	for {
		var config object
		err := decoder.Decode(&config)
		if errors.Is(err, io.EOF) {
			break
		}
		id := text(config, "metadata", "id")
		if err != nil || id == "" || configs[id] != nil || len(configs) >= 128 ||
			text(config, "metadata", "namespace") != string(block.NamespaceName) ||
			text(config, "metadata", "type") != string(block.VolumeConfigType) ||
			text(config, "metadata", "phase") != "running" {
			return nil, errInventory
		}
		if (strings.HasPrefix(id, "r-") || strings.HasPrefix(id, "u-")) &&
			id != "r-longhorn" && id != longhornmigration.RetainedStageRawID {
			return nil, errInventory
		}
		configs[id] = config
	}
	if !commandContextActive(ctx) {
		return nil, errInventory
	}
	return configs, nil
}

func (p temporaryPreparation) retainedProfile(ctx context.Context, address string, receipt storageRootReceipt) (bool, error) {
	if receipt.retainedLayout.StageStatusPresent {
		return true, p.verifyRetainedLayoutConfig(ctx, address, true)
	}
	configs, err := p.readRetainedVolumeConfigs(ctx, address)
	if err != nil || configs[longhornmigration.RetainedStageRawID] != nil {
		return false, errInventory
	}
	return false, verifyDesiredLayout(configs["r-longhorn"], configs["EPHEMERAL"])
}

// Install only the two declarations that prepare new storage. The original
// raw volume, its backing extent and the old root remain untouched here.
func (p temporaryPreparation) installRetainedStageLayout(ctx context.Context, address string) error {
	before, err := p.observeRetainedPreparation(ctx, true, false)
	if err != nil {
		return err
	}
	staged, err := p.retainedProfile(ctx, address, before)
	if staged || err != nil {
		return err
	}
	node, currentAddress, err := p.node(ctx, true)
	if err != nil || currentAddress != address || text(node, "status", "nodeInfo", "bootID") != p.j.OriginalBootID ||
		value(node, "spec", "unschedulable") != true || text(node, "metadata", "annotations", fenceAnnotation) != p.name {
		return errInventory
	}
	if err = p.checkOwner(ctx); err != nil {
		return err
	}
	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err = p.c.run(applyCtx, nil, "talosctl", "--nodes="+address, "patch", "machineconfig", "--mode=no-reboot",
		"--patch="+retainedStagingLayout); err != nil {
		return errors.New("the retained-worker staging layout change could not be confirmed")
	}
	return waitReadyWithin(ctx, time.Minute, func(ctx context.Context) error {
		after, err := p.observeRetainedPreparation(ctx, true, false)
		if err != nil || !after.retainedLayout.StageStatusPresent {
			return errInventory
		}
		return p.verifyRetainedLayoutConfig(ctx, address, true)
	})
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
		identity.EphemeralUUID != p.j.EphemeralUUID || identity.StagePartitionUUID != "" || closed && !identity.StageStatusPresent ||
		(p.j.RetainedRawUUID != "" && !p.sameRetainedExtent(identity)) {
		return storageRootReceipt{}, errInventory
	}
	return receipt, nil
}

func (p temporaryPreparation) sameRetainedExtent(identity longhorninventory.RetainedLayoutIdentity) bool {
	return identity.PartitionUUID == p.j.RetainedRawUUID && identity.ExtentDigest == p.j.RetainedExtentDigest
}

func (p temporaryPreparation) closeRetainedForReset(ctx context.Context, address string) error {
	if p.j.Schema != 3 || p.j.Phase != "draining" {
		return errInventory
	}
	if _, err := p.observeRetainedPreparation(ctx, true, true); err == nil {
		return p.verifyRetainedLayoutConfig(ctx, address, false)
	}
	if err := p.installRetainedStageLayout(ctx, address); err != nil {
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
