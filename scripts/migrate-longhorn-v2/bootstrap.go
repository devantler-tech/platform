package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

func verifyCapacityPin(n object, required bool) error {
	owner := text(n, "metadata", "annotations", capacityOwnerAnnotation)
	pin := text(n, "metadata", "annotations", scaleDownDisabled)
	if owner == capacityOwnerValue && pin == "true" {
		return nil
	}
	if owner != "" || pin != "" || required {
		return errors.New("migration capacity has a missing, foreign or drifted scale-down pin")
	}
	return nil
}

func migrationFilesystem(ephemeral object) (object, error) {
	size, ok := value(ephemeral, "spec", "size").(float64)
	if !ok || size < 140*1024*1024*1024 || text(ephemeral, "metadata", "id") != "EPHEMERAL" ||
		text(ephemeral, "spec", "phase") != "ready" || text(ephemeral, "spec", "type") != "partition" ||
		text(ephemeral, "spec", "encryptionProvider") != "luks2" || value(ephemeral, "spec", "encryptionLockedToState") != true ||
		text(ephemeral, "spec", "uuid") == "" || !strings.HasPrefix(text(ephemeral, "spec", "mountLocation"), "/dev/dm-") {
		return nil, errors.New("large, encrypted and unlocked temporary filesystem capacity is not proven")
	}
	keys, ok := value(ephemeral, "spec", "configuredEncryptionKeys").([]any)
	if !ok || len(keys) != 1 || keys[0] != "nodeID" {
		return nil, errors.New("temporary filesystem encryption key is outside the reviewed contract")
	}
	return object{"migration-v1": object{"path": "/var/lib/longhorn", "diskType": "filesystem", "diskDriver": "", "allowScheduling": true,
		"evictionRequested": false, "storageReserved": math.Floor(size * 0.30), "tags": []any{}}}, nil
}

func sameDisks(actual any, expected object) bool {
	disks, ok := actual.(map[string]any)
	if !ok || len(disks) != len(expected) {
		return false
	}
	for name, raw := range expected {
		want := raw.(map[string]any)
		got, ok := disks[name].(map[string]any)
		if !ok {
			return false
		}
		for key := range got {
			if _, exists := want[key]; !exists {
				return false
			}
		}
		for key, v := range want {
			if key == "tags" && got[key] == nil {
				continue
			}
			if key == "diskDriver" && v == "" && got[key] == nil {
				continue
			}
			if !reflect.DeepEqual(got[key], v) {
				return false
			}
		}
	}
	return true
}

func metadataChanges(o object, field string, values map[string]string) []object {
	changes := []object{}
	if value(o, "metadata", field) == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/" + field, "value": object{}})
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		path := strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
		changes = append(changes, object{"op": "add", "path": "/metadata/" + field + "/" + path, "value": values[key]})
	}
	return changes
}

type capacityBinding struct {
	node, storage object
	disks         object
	address       string
}

func sameCapacityNode(current, original object, pool migrationPool) error {
	if err := verifyMigrationNode(current, pool); err != nil {
		return err
	}
	for _, path := range [][]string{{"metadata", "uid"}, {"spec", "providerID"}, {"status", "nodeInfo", "systemUUID"}, {"status", "nodeInfo", "bootID"}} {
		if text(current, path...) == "" || text(current, path...) != text(original, path...) {
			return errors.New("migration-capacity hardware or boot identity changed")
		}
	}
	return nil
}

func bootstrapCapacity(ctx context.Context, c client, source string, pool migrationPool) error {
	s, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if err = verifyRedundancy(s); err != nil {
		return err
	}
	selected := []object{}
	for _, n := range s.nodes {
		if strings.HasPrefix(text(n, "metadata", "name"), pool.name+"-") || text(n, "metadata", "labels", migrationPoolLabel) == pool.role {
			if err = verifyMigrationNode(n, pool); err != nil {
				return err
			}
			selected = append(selected, n)
		}
	}
	if len(selected) != pool.count {
		return errors.New("the complete fixed-size migration pool is not Ready")
	}
	sort.Slice(selected, func(i, j int) bool {
		return text(selected[i], "metadata", "name") < text(selected[j], "metadata", "name")
	})
	plan := []capacityBinding{}
	addresses := map[string]bool{}
	for _, n := range selected {
		if err = verifyCapacityPin(n, false); err != nil {
			return err
		}
		name := text(n, "metadata", "name")
		address, addressErr := verifyNode(n, journal{Target: name, ProviderID: text(n, "spec", "providerID"), MachineUUID: text(n, "status", "nodeInfo", "systemUUID"), NodeUID: text(n, "metadata", "uid")}, true)
		if addressErr != nil || addresses[address] {
			return errors.New("migration-capacity private endpoint identities are ambiguous")
		}
		addresses[address] = true
		if err = c.verifyHost(ctx, address, n); err != nil {
			return err
		}
		ephemeral, readErr := c.talosVolume(ctx, address, "EPHEMERAL")
		if readErr != nil {
			return readErr
		}
		var disks object
		if pool.role == "v1" {
			disks, err = migrationFilesystem(ephemeral)
		} else {
			var raw object
			raw, err = c.talosVolume(ctx, address, "r-longhorn")
			if err == nil {
				err = verifyEncryptedLocalLayout(raw, ephemeral)
			}
			disks = blockDisks()
		}
		if err != nil {
			return err
		}
		lh, readErr := c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false)
		if readErr != nil {
			return readErr
		}
		enabled := text(lh, "metadata", "labels", migrationEnabledLabel)
		if enabled == "true" {
			if err = verifyCapacityPin(n, true); err != nil {
				return err
			}
			if value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") != false || !sameDisks(value(lh, "spec", "disks"), disks) {
				return errors.New("previously enabled migration storage drifted")
			}
		} else {
			if enabled != "" || value(lh, "spec", "allowScheduling") != false || value(lh, "spec", "evictionRequested") != true || !reflect.DeepEqual(value(lh, "spec", "disks"), object{}) {
				return errors.New("new migration capacity is not empty and fenced")
			}
			for _, r := range s.replicas {
				if text(r, "spec", "nodeID") == name {
					return errors.New("unowned replica already references migration capacity")
				}
			}
		}
		plan = append(plan, capacityBinding{node: n, storage: lh, disks: disks, address: address})
	}
	// Protect the entire pool before making any of its disks schedulable.
	for _, binding := range plan {
		n, readErr := c.get(ctx, "node", "", text(binding.node, "metadata", "name"), false)
		if readErr != nil {
			return readErr
		}
		if err = sameCapacityNode(n, binding.node, pool); err != nil {
			return err
		}
		if err = verifyCapacityPin(n, false); err != nil {
			return err
		}
		if err = sourceCurrent(ctx, c.run, source); err != nil {
			return err
		}
		if err = c.patch(ctx, "node", "", n, metadataChanges(n, "annotations", map[string]string{scaleDownDisabled: "true", capacityOwnerAnnotation: capacityOwnerValue})...); err != nil {
			return err
		}
	}
	// Read every pin and hardware binding back before enabling even the first disk.
	for _, binding := range plan {
		n, readErr := c.get(ctx, "node", "", text(binding.node, "metadata", "name"), false)
		if readErr != nil {
			return readErr
		}
		if err = sameCapacityNode(n, binding.node, pool); err != nil {
			return err
		}
		if err = verifyCapacityPin(n, true); err != nil {
			return err
		}
	}
	for _, binding := range plan {
		name := text(binding.node, "metadata", "name")
		n, readErr := c.get(ctx, "node", "", name, false)
		if readErr != nil {
			return readErr
		}
		if err = sameCapacityNode(n, binding.node, pool); err != nil {
			return err
		}
		if err = verifyCapacityPin(n, true); err != nil {
			return err
		}
		lh, readErr := c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false)
		if readErr != nil {
			return readErr
		}
		if text(lh, "metadata", "uid") != text(binding.storage, "metadata", "uid") || !reflect.DeepEqual(value(lh, "spec"), value(binding.storage, "spec")) {
			return errors.New("migration storage identity or fence changed before activation")
		}
		changes := metadataChanges(lh, "labels", map[string]string{migrationEnabledLabel: "true"})
		changes = append(changes, object{"op": "replace", "path": "/spec/disks", "value": binding.disks}, object{"op": "replace", "path": "/spec/allowScheduling", "value": true}, object{"op": "replace", "path": "/spec/evictionRequested", "value": false})
		if err = sourceCurrent(ctx, c.run, source); err != nil {
			return err
		}
		if err = c.patch(ctx, "nodes.longhorn.io", "longhorn-system", lh, changes...); err != nil {
			return err
		}
	}
	if err = waitReady(ctx, func() error {
		for _, binding := range plan {
			lh, readErr := c.get(ctx, "nodes.longhorn.io", "longhorn-system", text(binding.node, "metadata", "name"), false)
			if readErr != nil {
				return readErr
			}
			if text(lh, "metadata", "labels", migrationEnabledLabel) != "true" || value(lh, "spec", "allowScheduling") != true || value(lh, "spec", "evictionRequested") != false || !sameDisks(value(lh, "spec", "disks"), binding.disks) {
				return errors.New("migration storage activation is not reconciled")
			}
			if pool.role == "v1" {
				disk, _ := value(lh, "status", "diskStatus", "migration-v1").(map[string]any)
				if text(disk, "diskUUID") == "" || !ready(disk) {
					return errors.New("temporary filesystem is not Ready")
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Println("Verified migration capacity is pinned against automatic scale-down and enabled for storage.")
	return nil
}

func activateCapacityAfterBaseline(ctx context.Context, c client, source string, pool migrationPool) error {
	nodes, err := c.list(ctx, "nodes", "")
	if err != nil {
		return err
	}
	if err = requireBenchmarkBaseline(ctx, c, nodes); err != nil {
		return err
	}
	return bootstrapCapacity(ctx, c, source, pool)
}
