package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

func (p preparation) node(ctx context.Context, original bool) (object, string, error) {
	n, err := p.c.get(ctx, "node", "", p.target, false)
	if err != nil {
		return nil, "", err
	}
	address, err := verifyNode(n, p.j, original)
	if err == nil {
		err = p.c.verifyHost(ctx, address, n)
	}
	return n, address, err
}

func (p preparation) storageNode(ctx context.Context) (object, error) {
	n, err := p.c.get(ctx, "nodes.longhorn.io", "longhorn-system", p.target, false)
	if err != nil || text(n, "metadata", "uid") != p.j.LonghornUID {
		return nil, errors.New("longhorn node identity changed or is unreadable")
	}
	return n, nil
}

func (p preparation) desiredLayout(ctx context.Context, address string) error {
	raw, err := p.c.talosResource(ctx, address, "volumeconfig", "r-longhorn")
	if err != nil {
		return err
	}
	ephemeral, err := p.c.talosResource(ctx, address, "volumeconfig", "EPHEMERAL")
	if err != nil {
		return err
	}
	return verifyDesiredLayout(raw, ephemeral)
}

func (p *preparation) begin(ctx context.Context) error {
	cm, _, err := p.readRecord(ctx)
	if err != nil {
		return err
	}
	if cm != nil {
		return p.claimRecord(ctx, journal{})
	}
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = verifyRedundancy(s); err != nil {
		return err
	}
	if err = verifyMigrationCapacity(s.nodes, p.pools); err != nil {
		return err
	}
	if err = verifyTemporaryStorageEnabled(s, p.pools); err != nil {
		return err
	}
	if err = verifyRebuildBudget(p.target, s); err != nil {
		return err
	}
	if err = verifyBudgetSettings(ctx, p.c); err != nil {
		return err
	}
	n, err := p.c.get(ctx, "node", "", p.target, false)
	if err != nil {
		return err
	}
	if err = verifyDiskDefaults(n); err != nil {
		return err
	}
	lh, err := p.c.get(ctx, "nodes.longhorn.io", "longhorn-system", p.target, false)
	if err != nil {
		return err
	}
	status, _ := value(n, "status").(map[string]any)
	if !ready(status) || value(n, "spec", "unschedulable") == true || value(n, "metadata", "annotations", fenceAnnotation) != nil ||
		value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") != false {
		return errors.New("target is already cordoned, fenced, unready or evacuating")
	}
	disks, _ := value(lh, "spec", "disks").(map[string]any)
	j := journal{Schema: 1, Target: p.target, Source: p.source, OwnerRun: p.owner, Phase: "evacuating", NodeUID: text(n, "metadata", "uid"),
		ProviderID: text(n, "spec", "providerID"), MachineUUID: text(n, "status", "nodeInfo", "systemUUID"), LonghornUID: text(lh, "metadata", "uid"), OriginalDisks: disks}
	address, err := verifyNode(n, j, true)
	if err != nil {
		return err
	}
	if err = p.c.verifyHost(ctx, address, n); err != nil {
		return err
	}
	if err = p.desiredLayout(ctx, address); err != nil {
		return err
	}
	ephemeral, err := p.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil || text(ephemeral, "spec", "phase") != "ready" || text(ephemeral, "spec", "encryptionProvider") != "luks2" {
		return errors.New("original encrypted ephemeral partition is not ready")
	}
	j.EphemeralUUID = text(ephemeral, "spec", "uuid")
	return p.claimRecord(ctx, j)
}

func (p *preparation) evacuate(ctx context.Context) error {
	n, _, err := p.node(ctx, true)
	if err != nil {
		return err
	}
	fence := text(n, "metadata", "annotations", fenceAnnotation)
	if fence != "" && fence != p.name || value(n, "spec", "unschedulable") == true && fence != p.name {
		return errors.New("worker cordon is owned by another operation")
	}
	changes := annotationPatch(n, p.name)
	changes = append(changes, object{"op": "add", "path": "/spec/unschedulable", "value": true})
	if err = p.mutate(ctx, "node", "", n, changes...); err != nil {
		return err
	}
	lh, err := p.storageNode(ctx)
	if err != nil || !reflect.DeepEqual(value(lh, "spec", "disks"), p.j.OriginalDisks) {
		return errors.New("original filesystem disk configuration changed")
	}
	if err = p.mutate(ctx, "nodes.longhorn.io", "longhorn-system", lh,
		object{"op": "replace", "path": "/spec/allowScheduling", "value": false},
		object{"op": "replace", "path": "/spec/evictionRequested", "value": true}); err != nil {
		return err
	}
	replicas, err := p.c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, r := range replicas {
		if text(r, "spec", "nodeID") != p.target {
			continue
		}
		if _, err = p.c.kubectl(ctx, nil, "wait", "--for=delete", "replicas.longhorn.io/"+text(r, "metadata", "name"), "--namespace=longhorn-system", "--timeout=30m"); err != nil {
			return errors.New("replica evacuation did not complete; worker remains fenced")
		}
	}
	return p.advance(ctx, "draining")
}

func (p preparation) verifyEmpty(ctx context.Context) error {
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = verifyMigrationCapacity(s.nodes, p.pools); err != nil {
		return err
	}
	if err = verifyRebuildBudget(p.target, s); err != nil {
		return err
	}
	if err = verifyBudgetSettings(ctx, p.c); err != nil {
		return err
	}
	return verifyEvacuated(p.target, s)
}

func (p *preparation) drainAndReset(ctx context.Context) error {
	n, address, err := p.node(ctx, true)
	if err != nil {
		return err
	}
	if value(n, "spec", "unschedulable") != true || text(n, "metadata", "annotations", fenceAnnotation) != p.name {
		return errors.New("the owned worker cordon is missing")
	}
	if err = verifyDiskDefaults(n); err != nil {
		return err
	}
	if err = p.checkOwner(ctx); err != nil {
		return err
	}
	if _, err = p.c.kubectl(ctx, nil, "drain", p.target, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=40m"); err != nil {
		return errors.New("drain was blocked; no partition reset was requested")
	}
	if err = p.verifyEmpty(ctx); err != nil {
		return err
	}
	lh, err := p.storageNode(ctx)
	if err != nil || value(lh, "spec", "allowScheduling") != false || value(lh, "spec", "evictionRequested") != true {
		return errors.New("storage evacuation fence changed")
	}
	disks := value(lh, "spec", "disks")
	original := reflect.DeepEqual(disks, p.j.OriginalDisks)
	disabled := reflect.DeepEqual(disks, disabledDisks(p.j.OriginalDisks))
	if !original && !disabled && !reflect.DeepEqual(disks, object{}) && !reviewedBlockDisks(disks) {
		return errors.New("a foreign disk configuration prevents reset")
	}
	if original || disabled {
		if err = p.removeOriginalDisks(ctx, lh, original); err != nil {
			return err
		}
	}
	if err = p.desiredLayout(ctx, address); err != nil {
		return err
	}
	ephemeral, err := p.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil || text(ephemeral, "spec", "uuid") != p.j.EphemeralUUID || text(ephemeral, "spec", "phase") != "ready" {
		return errors.New("original partition changed before reset authorization")
	}
	// This second observation is after drain, disk removal and loaded-config proof.
	if err = p.verifyEmpty(ctx); err != nil {
		return err
	}
	if _, _, err = p.node(ctx, true); err != nil {
		return err
	}
	// advance proves ownership and source freshness before writing. Nothing may
	// fail between this intent and the reset, or the journal records a reset that
	// was never sent and the worker needs a reviewed recovery.
	if err = p.advance(ctx, "reset-requested"); err != nil {
		return err
	}
	// A response can be lost after acceptance. Recovery from reset-requested only
	// inspects post-state; it never repeats this destructive request automatically.
	_, resetErr := p.c.run(ctx, nil, "talosctl", "--nodes="+address, "reset", "--system-labels-to-wipe=EPHEMERAL", "--reboot", "--graceful=true", "--wait", "--timeout=15m")
	if resetErr != nil {
		fmt.Println("Reset response was uncertain; requiring positive post-reset state before proceeding.")
	}
	return nil
}

// disabledDisks is the original configuration with scheduling off on every disk,
// the only state from which Longhorn's webhook accepts removing a disk.
func disabledDisks(original map[string]any) map[string]any {
	disabled := map[string]any{}
	for name, raw := range original {
		disk, _ := raw.(map[string]any)
		copied := map[string]any{}
		for key, field := range disk {
			copied[key] = field
		}
		copied["allowScheduling"] = false
		disabled[name] = copied
	}
	return disabled
}

// Longhorn also refuses removal until the disk reports no scheduled storage, which
// can lag the evacuation, so the removal is retried within the recovery window.
func (p preparation) removeOriginalDisks(ctx context.Context, lh object, schedulable bool) error {
	if schedulable {
		changes := []object{}
		for name := range p.j.OriginalDisks {
			changes = append(changes, object{"op": "replace", "path": "/spec/disks/" + name + "/allowScheduling", "value": false})
		}
		if err := p.mutate(ctx, "nodes.longhorn.io", "longhorn-system", lh, changes...); err != nil {
			return err
		}
	}
	return waitReady(ctx, func() error {
		current, err := p.storageNode(ctx)
		if err != nil {
			return err
		}
		return p.mutate(ctx, "nodes.longhorn.io", "longhorn-system", current, object{"op": "replace", "path": "/spec/disks", "value": object{}})
	})
}

func (p preparation) rebuilt(ctx context.Context) error {
	n, address, err := p.node(ctx, false)
	if err != nil {
		return err
	}
	status, _ := value(n, "status").(map[string]any)
	if !ready(status) || !hugepagesReady(n) {
		return errors.New("worker has not rejoined Ready with the required huge pages")
	}
	raw, err := p.c.talosVolume(ctx, address, "r-longhorn")
	if err != nil {
		return err
	}
	ephemeral, err := p.c.talosVolume(ctx, address, "EPHEMERAL")
	if err != nil {
		return err
	}
	return verifyResetLayout(raw, ephemeral, p.j.EphemeralUUID)
}

func (p *preparation) finish(ctx context.Context) error {
	err := waitReady(ctx, func() error { return p.rebuilt(ctx) })
	if err != nil {
		return err
	}
	n, _, err := p.node(ctx, false)
	if err != nil {
		return err
	}
	fence := text(n, "metadata", "annotations", fenceAnnotation)
	if fence != p.name && (fence != "" || value(n, "spec", "unschedulable") == true) {
		return errors.New("refusing to release a foreign post-reset cordon")
	}
	lh, err := p.storageNode(ctx)
	if err != nil {
		return err
	}
	disks := value(lh, "spec", "disks")
	if !reflect.DeepEqual(disks, object{}) && !reviewedBlockDisks(disks) {
		return errors.New("post-reset Longhorn disk configuration is not the reviewed raw disk")
	}
	if err = p.mutate(ctx, "nodes.longhorn.io", "longhorn-system", lh,
		object{"op": "replace", "path": "/spec/disks", "value": blockDisks()},
		object{"op": "replace", "path": "/spec/allowScheduling", "value": true},
		object{"op": "replace", "path": "/spec/evictionRequested", "value": false}); err != nil {
		return err
	}
	n, _, err = p.node(ctx, false)
	if err != nil {
		return err
	}
	fence = text(n, "metadata", "annotations", fenceAnnotation)
	if fence != p.name {
		if fence != "" || value(n, "spec", "unschedulable") == true {
			return errors.New("refusing to release a foreign post-reset cordon")
		}
	} else if err = p.mutate(ctx, "node", "", n,
		object{"op": "add", "path": "/spec/unschedulable", "value": false},
		object{"op": "remove", "path": "/metadata/annotations/storage.platform.devantler.tech~1longhorn-v2-preparation"}); err != nil {
		return err
	}
	return p.advance(ctx, "prepared")
}

func (p preparation) verifyPrepared(ctx context.Context) error {
	if err := p.rebuilt(ctx); err != nil {
		return err
	}
	n, _, err := p.node(ctx, false)
	if err != nil || value(n, "spec", "unschedulable") == true || text(n, "metadata", "annotations", fenceAnnotation) != "" {
		return errors.New("prepared worker is still cordoned or fenced")
	}
	lh, err := p.storageNode(ctx)
	if err != nil || !reviewedBlockDisks(value(lh, "spec", "disks")) || value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") != false {
		return errors.New("prepared raw-disk configuration drifted")
	}
	s, err := p.c.snapshot(ctx)
	if err != nil {
		return err
	}
	return verifyRedundancy(s)
}

func (p *preparation) prepare(ctx context.Context) error {
	if err := p.begin(ctx); err != nil {
		return err
	}
	if p.j.Phase == "evacuating" {
		fmt.Println("Evacuating replicas with the worker fenced from new placement.")
		if err := p.evacuate(ctx); err != nil {
			return err
		}
	}
	if p.j.Phase == "draining" {
		fmt.Println("Draining clients and verifying every volume before the ephemeral-only reset.")
		if err := p.drainAndReset(ctx); err != nil {
			return err
		}
	}
	if p.j.Phase == "reset-requested" {
		fmt.Println("Verifying new encrypted partitions and worker recovery.")
		if err := p.finish(ctx); err != nil {
			return err
		}
	}
	if p.j.Phase != "prepared" {
		return errors.New("node preparation did not reach its terminal verified state")
	}
	if err := p.verifyPrepared(ctx); err != nil {
		return err
	}
	fmt.Println("Worker preparation completed; V1 retirement and data migration remain separate acceptance gates.")
	return nil
}
