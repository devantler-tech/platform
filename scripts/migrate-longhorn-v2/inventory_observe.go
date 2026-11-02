package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/devantler-tech/platform/internal/longhorninventory"
)

type nativeReplicaReader interface {
	ListReplicas(context.Context) (longhorninventory.Listing, error)
	BootID(context.Context) (string, error)
}

type inventoryReaderFactory func(context.Context, string) (nativeReplicaReader, func() error, error)

type inventoryReceipt struct {
	Schema              int                      `json:"schema"`
	ObservationOnly     bool                     `json:"observationOnly"`
	Registered          bool                     `json:"registered"`
	APIProjectionDigest string                   `json:"apiProjectionDigest"`
	Report              longhorninventory.Report `json:"report"`
}

func (c client) inventoryProjection(ctx context.Context, target string) (inventoryProjection, object, error) {
	var nodes, storage, replicas []object
	for _, query := range []struct {
		kind, namespace string
		destination     *[]object
	}{
		{"nodes", "", &nodes}, {"nodes.longhorn.io", "longhorn-system", &storage}, {"replicas.longhorn.io", "longhorn-system", &replicas},
	} {
		args := append([]string{"get", query.kind, "--output=json", "--chunk-size=0"}, namespaceArgs(query.namespace)...)
		data, err := c.kubectl(ctx, nil, args...)
		if err != nil {
			return inventoryProjection{}, nil, errInventory
		}
		*query.destination, err = decodeInventory(data)
		if err != nil {
			return inventoryProjection{}, nil, err
		}
	}
	var node, lh object
	for _, item := range nodes {
		if text(item, "metadata", "name") == target {
			node = item
		}
	}
	for _, item := range storage {
		if text(item, "metadata", "name") == target {
			lh = item
		}
	}
	p, err := projectInventory(target, node, lh, replicas)
	if err != nil {
		return inventoryProjection{}, nil, err
	}
	for _, item := range nodes {
		if text(item, "metadata", "name") == target {
			continue
		}
		if strings.EqualFold(text(item, "status", "nodeInfo", "systemUUID"), text(node, "status", "nodeInfo", "systemUUID")) {
			return inventoryProjection{}, nil, errInventory
		}
		addresses, ok := value(item, "status", "addresses").([]any)
		if !ok {
			return inventoryProjection{}, nil, errInventory
		}
		for _, raw := range addresses {
			address, ok := raw.(map[string]any)
			if !ok || text(address, "address") == p.Address {
				return inventoryProjection{}, nil, errInventory
			}
		}
	}
	return p, node, nil
}

// observeReplicaInventory publishes counts and commitments only. Two unchanged
// observations are not an atomic disk snapshot or evidence of recoverability.
func observeReplicaInventory(ctx context.Context, c client, target string, factory inventoryReaderFactory) (receipt inventoryReceipt, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	expires, _ := ctx.Deadline()
	active := func() bool { return ctx.Err() == nil && time.Now().Before(expires) }
	if !active() {
		return inventoryReceipt{}, stageDeadline
	}
	before, node, err := c.inventoryProjection(ctx, target)
	if err != nil {
		return inventoryReceipt{}, stageProjectionBefore
	}
	reader, closeReader, err := factory(ctx, before.Address)
	if err != nil || reader == nil || closeReader == nil {
		return inventoryReceipt{}, stageClient
	}
	defer func() {
		if closeErr := closeReader(); closeErr != nil {
			receipt = inventoryReceipt{}
			err = errors.Join(err, stageClose)
		}
		if !active() {
			receipt = inventoryReceipt{}
			err = errors.Join(err, stageDeadline)
		}
	}()
	if err = c.verifyHost(ctx, before.Address, node); err != nil {
		return inventoryReceipt{}, stageHostBefore
	}
	boot, err := reader.BootID(ctx)
	if err != nil || boot != before.BootID {
		return inventoryReceipt{}, stageBootBefore
	}
	first, err := reader.ListReplicas(ctx)
	if err != nil {
		return inventoryReceipt{}, stageListingBefore
	}
	firstReport, err := longhorninventory.Analyze(first, before.Replicas)
	if err != nil {
		return inventoryReceipt{}, stageOwnershipBefore
	}
	second, err := reader.ListReplicas(ctx)
	if err != nil {
		return inventoryReceipt{}, stageListingAfter
	}
	after, node, err := c.inventoryProjection(ctx, target)
	if err != nil {
		return inventoryReceipt{}, stageProjectionAfter
	}
	if before.Digest != after.Digest {
		return inventoryReceipt{}, stageAPIConsistency
	}
	if err = c.verifyHost(ctx, after.Address, node); err != nil {
		return inventoryReceipt{}, stageHostAfter
	}
	boot, err = reader.BootID(ctx)
	if err != nil || boot != before.BootID || ctx.Err() != nil {
		return inventoryReceipt{}, stageBootAfter
	}
	report, err := longhorninventory.Analyze(second, after.Replicas)
	if err != nil {
		return inventoryReceipt{}, stageOwnershipAfter
	}
	if report.Digest != firstReport.Digest {
		return inventoryReceipt{}, stageListingConsistency
	}
	if !active() {
		return inventoryReceipt{}, stageDeadline
	}
	return inventoryReceipt{Schema: 1, ObservationOnly: true, Registered: before.Registered, APIProjectionDigest: before.Digest, Report: report}, nil
}
