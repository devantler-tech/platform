package main

import (
	"context"
	_ "embed"
	"errors"
	"time"
)

const stagingRawDiskGiB = 144
const replacedStagingLayout = temporaryRecoveryGate("staged configuration after rollback to the standard layout")

//go:embed staging-layout.yaml
var stagingLayoutPatch string

func verifyStagingBudget(s inventory) error {
	return verifyRawDiskBudget("", s, stagingRawDiskGiB)
}

func verifyStagingLayout(raw, ephemeral object) error {
	return verifyRawDiskConfig(raw, ephemeral, stagingRawDiskGiB)
}

// Only an owned, drained temporary worker with positive native absence can
// request this patch. If a previous attempt already installed it, read it back
// without applying again. Configuration alone does not authorize a reset.
func (p temporaryPreparation) installStagingLayout(ctx context.Context, address string) error {
	if !p.j.StagingLayout || p.j.Phase != "draining" {
		return errInventory
	}
	raw, ephemeral, err := p.readDesiredLayout(ctx, address)
	if err != nil {
		return err
	}
	if verifyStagingLayout(raw, ephemeral) == nil {
		return nil
	}
	if err = verifyDesiredLayout(raw, ephemeral); err != nil {
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
	return p.applyStagingLayout(ctx, address)
}

func (p temporaryPreparation) applyStagingLayout(ctx context.Context, address string) error {
	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := p.c.run(applyCtx, nil, "talosctl", "--nodes="+address, "patch", "machineconfig", "--mode=no-reboot", "--patch="+stagingLayoutPatch); err != nil {
		return errors.New("the temporary worker layout change could not be confirmed")
	}
	return waitReadyWithin(ctx, time.Minute, func(ctx context.Context) error {
		return p.verifyUnusedLayout(ctx, address)
	})
}

func (p temporaryPreparation) verifyUnusedLayout(ctx context.Context, address string) error {
	if !p.staging {
		return p.desiredLayout(ctx, address)
	}
	raw, ephemeral, err := p.readDesiredLayout(ctx, address)
	if err != nil {
		return err
	}
	return verifyStagingLayout(raw, ephemeral)
}

func (p temporaryPreparation) verifyRecoveredLayout(ctx context.Context, address string) error {
	raw, ephemeral, err := p.readDesiredLayout(ctx, address)
	if err != nil {
		return temporaryRecoveryGate("readable Talos volume configuration")
	}
	if !p.staging && verifyDesiredLayout(raw, ephemeral) == nil || p.staging && verifyStagingLayout(raw, ephemeral) == nil {
		return nil
	}
	if p.staging && verifyDesiredLayout(raw, ephemeral) == nil {
		return replacedStagingLayout
	}
	return temporaryRecoveryGate("the reviewed volume configuration")
}

// Restore only a known configuration rollback after the native recovery read
// has already proven the new boot and both encrypted partitions. Recheck
// workload safety and the owner before changing configuration in place.
func (p temporaryPreparation) restoreRebuiltStagingLayout(ctx context.Context, recovered object) error {
	if !p.staging || !p.j.StagingLayout || !p.j.RawVolumeAbsent || p.j.Phase != "reset-requested" || p.retainsRaw() {
		return errInventory
	}
	s, err := p.c.snapshot(ctx)
	if err != nil || verifyStagingBudget(s) != nil || verifyEvacuated(p.target, s) != nil {
		return temporaryRecoveryGate("owned capacity and storage evacuation before restoring the layout")
	}
	if _, err = resolveTemporaryResetRecovery(p.temporarySelection, s.nodes, p.pools, p.j); err != nil {
		return temporaryRecoveryGate("the original migration pool member before restoring the layout")
	}
	previousAddress, err := workerPrivateAddress(recovered)
	if err != nil {
		return err
	}
	current, address, err := p.node(ctx, true)
	if err != nil || address != previousAddress || !ready(valueObject(current, "status")) ||
		text(current, "status", "nodeInfo", "bootID") != text(recovered, "status", "nodeInfo", "bootID") ||
		value(current, "spec", "unschedulable") != true || text(current, "metadata", "annotations", fenceAnnotation) != p.name ||
		text(current, "metadata", "annotations", capacityOwnerAnnotation) != capacityOwnerValue ||
		text(current, "metadata", "annotations", scaleDownDisabled) != "true" {
		return temporaryRecoveryGate("the recovered worker with its owned cordon and capacity pin")
	}
	if err = p.verifyRecoveredLayout(ctx, address); err != replacedStagingLayout {
		return err
	}
	if err = p.checkOwner(ctx); err != nil {
		return err
	}
	return p.applyStagingLayout(ctx, address)
}
