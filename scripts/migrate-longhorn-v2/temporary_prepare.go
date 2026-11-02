package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
)

type temporaryPreparation struct {
	preparation
	reader inventoryReaderFactory
}

func (p *temporaryPreparation) prepare(ctx context.Context) error {
	if err := p.beginUnused(ctx); err != nil {
		return err
	}
	if p.j.Phase == "evacuating" {
		if err := p.protectUnusedCapacity(ctx); err != nil {
			return err
		}
		n, _, err := p.node(ctx, true)
		if err != nil {
			return err
		}
		fence := text(n, "metadata", "annotations", fenceAnnotation)
		if (fence != "" && fence != p.name) || (value(n, "spec", "unschedulable") == true && fence != p.name) {
			return errInventory
		}
		patch := append(annotationPatch(n, p.name), object{"op": "add", "path": "/spec/unschedulable", "value": true})
		if err = p.mutate(ctx, "node", "", n, patch...); err != nil {
			return err
		}
		if err = p.advance(ctx, "draining"); err != nil {
			return err
		}
	}
	if p.j.Phase == "draining" {
		if err := p.resetUnused(ctx); err != nil {
			return err
		}
	}
	if p.j.Phase == "reset-requested" {
		if err := waitReady(ctx, func(ctx context.Context) error { _, err := p.unusedPostState(ctx); return err }); err != nil {
			return err
		}
		n, err := p.unusedPostState(ctx)
		if err != nil {
			return err
		}
		fence := text(n, "metadata", "annotations", fenceAnnotation)
		if fence == p.name && value(n, "spec", "unschedulable") == true {
			if err = p.mutate(ctx, "node", "", n, object{"op": "add", "path": "/spec/unschedulable", "value": false}, object{"op": "remove", "path": "/metadata/annotations/storage.platform.devantler.tech~1longhorn-v2-preparation"}); err != nil {
				return err
			}
		} else if fence != "" || value(n, "spec", "unschedulable") == true {
			return errInventory
		}
		if err = p.advance(ctx, "prepared"); err != nil {
			return err
		}
	}
	if p.j.Phase != "prepared" {
		return errInventory
	}
	n, err := p.unusedPostState(ctx)
	if err != nil || value(n, "spec", "unschedulable") == true || text(n, "metadata", "annotations", fenceAnnotation) != "" {
		return errInventory
	}
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = p.unusedCapacity(s, true); err != nil {
		return err
	}
	if err = verifyRedundancy(s); err != nil {
		return err
	}
	fmt.Println("Unused temporary worker recovered with encrypted raw storage; Longhorn storage remains fenced and unregistered.")
	return nil
}

func (p temporaryPreparation) unusedCapacity(s inventory, pinned bool) error {
	nodes := s.nodes
	for _, n := range nodes {
		if text(n, "metadata", "name") == p.target && value(n, "spec", "unschedulable") == true {
			var err error
			nodes, err = temporaryInventoryNodes(nodes, &p.j)
			if err != nil {
				return err
			}
			break
		}
	}
	if err := verifyCapacityRoster(nodes, p.pools, false); err != nil {
		return err
	}
	for _, n := range nodes {
		role := text(n, "metadata", "labels", migrationPoolLabel)
		if role != "" {
			owner, pin := text(n, "metadata", "annotations", capacityOwnerAnnotation), text(n, "metadata", "annotations", scaleDownDisabled)
			if (owner != "" && owner != capacityOwnerValue) || (pin != "" && pin != "true") || (pinned && (owner != capacityOwnerValue || pin != "true")) {
				return errInventory
			}
		}
		if role != "" || inventoryTarget.MatchString(text(n, "metadata", "name")) {
			if !hugepagesReady(n) {
				return errInventory
			}
		}
	}
	return nil
}

func (p *temporaryPreparation) beginUnused(ctx context.Context) error {
	cm, _, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if cm != nil {
		if err = p.claimRecord(ctx, journal{}); err != nil {
			return err
		}
		n, _, nodeErr := p.node(ctx, true)
		if nodeErr != nil {
			return nodeErr
		}
		lh, storageErr := p.storageNode(ctx)
		if storageErr != nil || !unusedStorageFenced(lh) {
			return errInventory
		}
		if (p.j.Phase == "evacuating" || p.j.Phase == "draining") && text(n, "status", "nodeInfo", "bootID") != p.j.OriginalBootID {
			return errInventory
		}
		return nil
	}
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = p.unusedCapacity(s, false); err != nil {
		return err
	}
	if err = verifyRedundancy(s); err != nil {
		return err
	}
	if err = requireBenchmarkBaseline(ctx, p.c, s.nodes); err != nil {
		return err
	}
	c := p.c
	c.inventoryPools = p.pools
	_, n, err := c.inventoryProjection(ctx, p.temporarySelection)
	if err != nil || text(n, "metadata", "name") != p.target || value(n, "spec", "unschedulable") == true || text(n, "metadata", "annotations", fenceAnnotation) != "" {
		return errInventory
	}
	lh, err := p.c.get(ctx, "nodes.longhorn.io", "longhorn-system", p.target, false)
	if err != nil {
		return err
	}
	p.j = journal{Schema: 2, Target: p.target, Source: p.source, OwnerRun: p.owner, Phase: "evacuating", NodeUID: text(n, "metadata", "uid"), ProviderID: text(n, "spec", "providerID"), MachineUUID: text(n, "status", "nodeInfo", "systemUUID"), LonghornUID: text(lh, "metadata", "uid"), OriginalDisks: object{}, TemporarySelection: p.temporarySelection, OriginalBootID: text(n, "status", "nodeInfo", "bootID")}
	_, address, err := p.node(ctx, true)
	if err != nil {
		return err
	}
	if err = p.desiredLayout(ctx, address); err != nil {
		return err
	}
	eph, err := p.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil || text(eph, "spec", "phase") != "ready" || text(eph, "spec", "encryptionProvider") != "luks2" {
		return errInventory
	}
	p.j.EphemeralUUID = text(eph, "spec", "uuid")
	digest, err := p.observeUnused(ctx, false)
	if err != nil {
		return err
	}
	p.j.NativeEmptyDigest, p.j.RawVolumeAbsent = digest, true
	return p.claimRecord(ctx, p.j)
}

func (p temporaryPreparation) observeUnused(ctx context.Context, owned bool) (string, error) {
	if err := sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return "", err
	}
	c := p.c
	c.inventoryPools = p.pools
	if owned {
		c.inventoryPreparation = &p.j
	}
	before, n, err := c.inventoryProjection(ctx, p.temporarySelection)
	if err != nil {
		return "", err
	}
	if _, err = verifyNode(n, p.j, true); err != nil || before.BootID != p.j.OriginalBootID || before.StorageUID != p.j.LonghornUID {
		return "", errInventory
	}
	factory := p.reader
	if factory == nil {
		factory = func(ctx context.Context, address string) (nativeReplicaReader, func() error, error) {
			return openInventoryReader(ctx, address, p.j.EphemeralUUID)
		}
	}
	receipt, err := observeReplicaInventory(ctx, c, p.temporarySelection, factory)
	if err != nil {
		return "", err
	}
	if receipt.APIProjectionDigest != before.Digest || receipt.Registered || !receipt.Report.EmptyAndAccounted {
		return "", errInventory
	}
	if err = sourceCurrent(ctx, p.c.run, p.source); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(receipt.APIProjectionDigest + ":" + receipt.Report.Digest))
	return hex.EncodeToString(digest[:]), nil
}

func (p temporaryPreparation) protectUnusedCapacity(ctx context.Context) error {
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = p.unusedCapacity(s, false); err != nil {
		return err
	}
	for _, expected := range s.nodes {
		role := text(expected, "metadata", "labels", migrationPoolLabel)
		if role == "" {
			continue
		}
		n, err := p.c.get(ctx, "node", "", text(expected, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if _, err = verifyNode(n, journal{Target: text(expected, "metadata", "name"), NodeUID: text(expected, "metadata", "uid"), ProviderID: text(expected, "spec", "providerID"), MachineUUID: text(expected, "status", "nodeInfo", "systemUUID")}, true); err != nil {
			return err
		}
		view := []object{n}
		if value(n, "spec", "unschedulable") == true {
			view, err = temporaryInventoryNodes(view, &p.j)
			if err != nil {
				return err
			}
		}
		if err = verifyMigrationNode(view[0], p.pools[role]); err != nil {
			return err
		}
		annotations := maps.Clone(valueObject(n, "metadata", "annotations"))
		if annotations == nil {
			annotations = object{}
		}
		owner, pin := text(n, "metadata", "annotations", capacityOwnerAnnotation), text(n, "metadata", "annotations", scaleDownDisabled)
		if owner != "" && owner != capacityOwnerValue || pin != "" && pin != "true" {
			return errInventory
		}
		if owner == capacityOwnerValue && pin == "true" {
			continue
		}
		annotations[capacityOwnerAnnotation], annotations[scaleDownDisabled] = capacityOwnerValue, "true"
		if err = p.mutate(ctx, "node", "", n, object{"op": "add", "path": "/metadata/annotations", "value": annotations}); err != nil {
			return err
		}
	}
	current, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	return p.unusedCapacity(current, true)
}

func (p *temporaryPreparation) resetUnused(ctx context.Context) error {
	n, address, err := p.node(ctx, true)
	if err != nil {
		return err
	}
	if value(n, "spec", "unschedulable") != true || text(n, "metadata", "annotations", fenceAnnotation) != p.name {
		return errInventory
	}
	if err = p.checkOwner(ctx); err != nil {
		return err
	}
	if _, err = p.c.kubectl(ctx, nil, "drain", p.target, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=40m"); err != nil {
		return errors.New("temporary worker drain was blocked; no reset was requested")
	}
	for range 2 {
		s, err := p.c.snapshot(ctx)
		if err != nil {
			return err
		}
		if err = p.unusedCapacity(s, true); err != nil {
			return err
		}
		if err = verifyEvacuated(p.target, s); err != nil {
			return err
		}
		if err = requireBenchmarkBaseline(ctx, p.c, s.nodes); err != nil {
			return err
		}
		if err = p.desiredLayout(ctx, address); err != nil {
			return err
		}
		if _, err = p.observeUnused(ctx, true); err != nil {
			return err
		}
	}
	if _, _, err = p.node(ctx, true); err != nil {
		return err
	}
	if err = p.advance(ctx, "reset-requested"); err != nil {
		return err
	}
	_, resetErr := p.c.run(ctx, nil, "talosctl", "--nodes="+address, "reset", "--system-labels-to-wipe=EPHEMERAL", "--reboot", "--graceful=true", "--wait", "--timeout=15m")
	if resetErr != nil {
		fmt.Println("Temporary reset response was uncertain; only positive recovery can advance the journal.")
	}
	return nil
}

func (p temporaryPreparation) unusedPostState(ctx context.Context) (node object, err error) {
	n, address, err := p.node(ctx, true)
	if err != nil {
		return nil, err
	}
	status := valueObject(n, "status")
	boot := text(n, "status", "nodeInfo", "bootID")
	if !ready(status) || !hugepagesReady(n) || !inventoryBoot.MatchString(boot) || boot == p.j.OriginalBootID {
		return nil, errInventory
	}
	factory := p.reader
	if factory == nil {
		factory = newInventoryReader
	}
	reader, closeReader, err := factory(ctx, address)
	if err != nil || reader == nil || closeReader == nil {
		return nil, errInventory
	}
	defer func() {
		if closeReader() != nil {
			node = nil
			err = errInventory
		}
	}()
	actualBoot, err := reader.BootID(ctx)
	if err != nil || actualBoot != boot {
		return nil, errInventory
	}
	if err = p.desiredLayout(ctx, address); err != nil {
		return nil, err
	}
	raw, err := p.c.talosVolume(ctx, address, "r-longhorn")
	if err != nil {
		return nil, err
	}
	eph, err := p.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil {
		return nil, err
	}
	if text(eph, "spec", "uuid") == p.j.EphemeralUUID {
		return nil, errInventory
	}
	if err = verifyEncryptedLocalLayout(raw, eph); err != nil {
		return nil, err
	}
	lh, err := p.storageNode(ctx)
	if err != nil {
		return nil, err
	}
	if !unusedStorageFenced(lh) {
		return nil, errInventory
	}
	return n, nil
}

func unusedStorageFenced(lh object) bool {
	disks, ok := value(lh, "spec", "disks").(map[string]any)
	diskStatus, statusOK := value(lh, "status", "diskStatus").(map[string]any)
	return ok && statusOK && len(disks) == 0 && len(diskStatus) == 0 && value(lh, "spec", "allowScheduling") == false && value(lh, "spec", "evictionRequested") == true && text(lh, "metadata", "labels", migrationEnabledLabel) == ""
}
