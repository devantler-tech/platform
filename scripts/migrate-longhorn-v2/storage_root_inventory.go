package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

type nativeStorageRootReader interface {
	ListStorageRoot(context.Context) (longhorninventory.Listing, error)
}
type storageRootReceipt struct {
	Schema              int                                 `json:"schema"`
	ObservationOnly     bool                                `json:"observationOnly"`
	ResetClearance      bool                                `json:"resetClearance"`
	APIProjectionDigest string                              `json:"apiProjectionDigest"`
	Report              longhorninventory.StorageRootReport `json:"report"`
}

func observeStorageRoot(ctx context.Context, c client, target string, factory inventoryReaderFactory) (receipt storageRootReceipt, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	expires, _ := ctx.Deadline()
	active := func() bool { return ctx.Err() == nil && time.Now().Before(expires) }
	if !active() {
		return storageRootReceipt{}, stageDeadline
	}
	before, node, err := c.inventoryProjection(ctx, target)
	if err != nil {
		return storageRootReceipt{}, stageProjectionBefore
	}
	reader, closeReader, err := factory(ctx, before.Address)
	if err != nil || reader == nil || closeReader == nil {
		return storageRootReceipt{}, stageClient
	}
	defer func() {
		if closeReader() != nil {
			receipt = storageRootReceipt{}
			err = errors.Join(err, stageClose)
		}
		if !active() {
			receipt = storageRootReceipt{}
			err = errors.Join(err, stageDeadline)
		}
	}()
	rootReader, ok := reader.(nativeStorageRootReader)
	if !ok {
		return storageRootReceipt{}, stageClient
	}
	if err = c.verifyHost(ctx, before.Address, node); err != nil {
		return storageRootReceipt{}, stageHostBefore
	}
	boot, err := reader.BootID(ctx)
	if err != nil || boot != before.BootID {
		return storageRootReceipt{}, stageBootBefore
	}
	first, err := rootReader.ListStorageRoot(ctx)
	if err != nil {
		return storageRootReceipt{}, errors.Join(stageListingBefore, err)
	}
	firstReport, err := longhorninventory.AnalyzeStorageRoot(first)
	if err != nil {
		return storageRootReceipt{}, stageOwnershipBefore
	}
	second, err := rootReader.ListStorageRoot(ctx)
	if err != nil {
		return storageRootReceipt{}, errors.Join(stageListingAfter, err)
	}
	after, node, err := c.inventoryProjection(ctx, target)
	if err != nil {
		return storageRootReceipt{}, stageProjectionAfter
	}
	if before.Digest != after.Digest {
		return storageRootReceipt{}, stageAPIConsistency
	}
	if err = c.verifyHost(ctx, after.Address, node); err != nil {
		return storageRootReceipt{}, stageHostAfter
	}
	boot, err = reader.BootID(ctx)
	if err != nil || boot != before.BootID {
		return storageRootReceipt{}, stageBootAfter
	}
	report, err := longhorninventory.AnalyzeStorageRoot(second)
	if err != nil {
		return storageRootReceipt{}, stageOwnershipAfter
	}
	if firstReport.Digest != report.Digest {
		return storageRootReceipt{}, stageListingConsistency
	}
	if !active() {
		return storageRootReceipt{}, stageDeadline
	}
	return storageRootReceipt{Schema: 1, ObservationOnly: true, ResetClearance: false, APIProjectionDigest: before.Digest, Report: report}, nil
}

func authorizeStorageRoot(getenv func(string) string, target string) error {
	if (!inventoryTarget.MatchString(target) && !inventoryTemporarySelection.MatchString(target)) || getenv("LONGHORN_V2_NODE") != target || getenv("LONGHORN_V2_OPERATION") != "inventory-storage-root" {
		return errors.New("the fixed storage-root observation selection is required")
	}
	return authorizeWorkflow(getenv, "inventory-longhorn-storage-root-"+target)
}

func runStorageRootInventory(ctx context.Context, target, source, runID string) error {
	c := client{run: inventoryReadCommand, context: "admin@prod"}
	if inventoryTemporarySelection.MatchString(target) {
		config, err := os.ReadFile("ksail.prod.yaml")
		if err != nil {
			return stageProjectionBefore
		}
		c.inventoryPools, err = configuredMigrationPools(config)
		if err != nil {
			return stageProjectionBefore
		}
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return stageSourceBefore
	}
	receipt, err := observeStorageRoot(ctx, c, target, newInventoryReader)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return stageSourceAfter
	}
	if err = json.NewEncoder(os.Stdout).Encode(struct {
		Source    string             `json:"source"`
		Run       string             `json:"run"`
		Inventory storageRootReceipt `json:"inventory"`
	}{source, runID, receipt}); err != nil {
		return stageOutput
	}
	return nil
}
