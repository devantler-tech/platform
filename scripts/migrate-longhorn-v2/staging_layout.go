package main

import (
	"context"
	_ "embed"
	"errors"
	"time"
)

const stagingRawDiskGiB = 144

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
	if err = p.checkOwner(ctx); err != nil {
		return err
	}
	node, currentAddress, err := p.node(ctx, true)
	if err != nil || currentAddress != address || text(node, "status", "nodeInfo", "bootID") != p.j.OriginalBootID ||
		value(node, "spec", "unschedulable") != true || text(node, "metadata", "annotations", fenceAnnotation) != p.name {
		return errInventory
	}
	applyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err = p.c.run(applyCtx, nil, "talosctl", "--nodes="+address, "patch", "machineconfig", "--mode=no-reboot", "--patch="+stagingLayoutPatch); err != nil {
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
