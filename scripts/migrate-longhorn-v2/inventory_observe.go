package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const (
	inventoryKubeNodes    = "/api/v1/nodes"
	inventoryStorageNodes = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/nodes"
	inventoryReplicas     = "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/replicas"
)

type inventoryReceipt struct {
	Schema              int                      `json:"schema"`
	ObservationOnly     bool                     `json:"observationOnly"`
	Registered          bool                     `json:"registered"`
	APIProjectionDigest string                   `json:"apiProjectionDigest"`
	Report              longhorninventory.Report `json:"report"`
}

// inventoryProjection binds a complete Kubernetes storage observation to the
// selected physical node and reviewed pool roster. A preparation journal permits
// only its own identity-bound cordon in the observation view.
func (c client) inventoryProjection(ctx context.Context, target string) (inventoryProjection, object, error) {
	return c.storageProjection(ctx, target, true)
}

// hostMemoryProjection binds counters to the selected host and registered disk,
// independently of replica scheduling. It grants no replica-accounting proof.
// Reboot recovery separately proves two peer copies before restoring scheduling.
func (c client) hostMemoryProjection(ctx context.Context, target string) (inventoryProjection, object, error) {
	if !inventoryTarget.MatchString(target) {
		return inventoryProjection{}, nil, errInventory
	}
	projection, node, err := c.storageProjection(ctx, target, false)
	if err != nil || !projection.Registered {
		return inventoryProjection{}, nil, errInventory
	}
	return projection, node, nil
}

// storageProjection shares node, hardware, disk and endpoint identity checks.
// Only the replica observer includes the complete replica ownership graph.
func (c client) storageProjection(ctx context.Context, target string, includeReplicas bool) (inventoryProjection, object, error) {
	var nodes, storage, replicas []object
	for _, query := range []struct {
		path, apiVersion, kind string
		destination            *[]object
	}{
		{inventoryKubeNodes, "v1", "NodeList", &nodes},
		{inventoryStorageNodes, "longhorn.io/v1beta2", "NodeList", &storage},
		{inventoryReplicas, "longhorn.io/v1beta2", "ReplicaList", &replicas},
	} {
		if query.path == inventoryReplicas && !includeReplicas {
			continue
		}
		// Ordinary kubectl printing flattens the typed list and discards its
		// collection revision. Preserve the server envelope with a fixed raw GET.
		data, err := c.kubectl(ctx, nil, "get", "--raw="+query.path)
		if err != nil {
			return inventoryProjection{}, nil, errInventory
		}
		*query.destination, err = decodeInventoryCollection(data, query.apiVersion, query.kind)
		if err != nil {
			return inventoryProjection{}, nil, err
		}
	}
	selectionDigest := ""
	var projectionErr error
	nodes, projectionErr = temporaryInventoryNodes(nodes, c.inventoryPreparation)
	if projectionErr != nil {
		return inventoryProjection{}, nil, projectionErr
	}
	if !inventoryTarget.MatchString(target) {
		var err error
		target, selectionDigest, err = resolveTemporaryInventoryTarget(target, nodes, c.inventoryPools)
		if err != nil {
			return inventoryProjection{}, nil, err
		}
	}
	if c.inventoryPreparation != nil && target != c.inventoryPreparation.Target {
		return inventoryProjection{}, nil, errInventory
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
	if selectionDigest != "" {
		digest := sha256.Sum256([]byte(selectionDigest + ":" + p.Digest))
		p.Digest = hex.EncodeToString(digest[:])
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
		return inventoryReceipt{}, errors.Join(stageListingBefore, err)
	}
	firstReport, err := longhorninventory.Analyze(first, before.Replicas)
	if err != nil {
		return inventoryReceipt{}, stageOwnershipBefore
	}
	second, err := reader.ListReplicas(ctx)
	if err != nil {
		return inventoryReceipt{}, errors.Join(stageListingAfter, err)
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
	if inventoryTemporarySelection.MatchString(target) && !report.EmptyAndAccounted {
		return inventoryReceipt{}, stageOwnershipAfter
	}
	if !active() {
		return inventoryReceipt{}, stageDeadline
	}
	return inventoryReceipt{Schema: 1, ObservationOnly: true, Registered: before.Registered, APIProjectionDigest: before.Digest, Report: report}, nil
}
