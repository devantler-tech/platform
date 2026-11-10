package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// read accepts only the fixed activation journal with a complete native API
// identity and a strictly decoded three-member plan.
func (a temporaryV2Activation) read(ctx context.Context) (object, temporaryActivationRecord, error) {
	cm, err := a.c.get(ctx, "configmap", "longhorn-system", temporaryActivationJournal, true)
	if err != nil || cm == nil {
		return cm, temporaryActivationRecord{}, err
	}
	if text(cm, "metadata", "name") != temporaryActivationJournal || text(cm, "metadata", "namespace") != "longhorn-system" || text(cm, "metadata", "uid") == "" || text(cm, "metadata", "resourceVersion") == "" || value(cm, "metadata", "deletionTimestamp") != nil {
		return nil, temporaryActivationRecord{}, errTemporaryActivation
	}
	record, err := decodeTemporaryActivationRecord([]byte(text(cm, "data", "record")))
	return cm, record, err
}

// persist conditionally writes the plan on current main, then verifies the
// same journal identity and complete record through a fresh native read.
func (a temporaryV2Activation) persist(ctx context.Context, cm object, record temporaryActivationRecord) (object, error) {
	if record.Validate() != nil || record.Source != a.source || record.OwnerRun != a.owner {
		return nil, errTemporaryActivation
	}
	data, err := json.Marshal(record)
	if err != nil || sourceCurrent(ctx, a.c.run, a.source) != nil {
		return nil, errTemporaryActivation
	}
	if cm == nil {
		cm, err = a.c.create(ctx, a.source, object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": temporaryActivationJournal, "namespace": "longhorn-system"}, "data": object{"record": string(data)}})
	} else {
		err = a.c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(data)})
	}
	if err != nil {
		return nil, err
	}
	readback, actual, err := a.read(ctx)
	if err != nil || text(readback, "metadata", "uid") != text(cm, "metadata", "uid") || !reflect.DeepEqual(record, actual) {
		return nil, errTemporaryActivation
	}
	return readback, nil
}

// owned rebinds journal identity, owner, plan and current main immediately
// before every registration or completion write.
func (a temporaryV2Activation) owned(ctx context.Context, uid string, expected temporaryActivationRecord) (object, error) {
	cm, actual, err := a.read(ctx)
	if err != nil || uid == "" || text(cm, "metadata", "uid") != uid || actual.Source != a.source || actual.OwnerRun != a.owner || !reflect.DeepEqual(expected, actual) {
		return nil, errTemporaryActivation
	}
	return cm, sourceCurrent(ctx, a.c.run, a.source)
}

// environment preserves the original V1 data and database quorums throughout
// raw registration. All six clients need their reserved capacity and every
// temporary worker must retain its owned autoscaler pin.
func (a temporaryV2Activation) environment(ctx context.Context) (inventory, error) {
	if err := verifyBudgetSettings(ctx, a.c); err != nil {
		return inventory{}, err
	}
	if err := verifyV2Settings(ctx, a.c); err != nil {
		return inventory{}, err
	}
	s, err := a.c.snapshot(ctx)
	if err != nil || verifyMigrationCapacity(s.nodes, a.pools) != nil || verifyRedundancy(s) != nil {
		return inventory{}, errTemporaryActivation
	}
	if err = verifyStagingBudget(s); err != nil {
		return inventory{}, err
	}
	managers, err := a.c.list(ctx, "instancemanagers.longhorn.io", "longhorn-system")
	if err != nil || verifyV2Clients(s, managers, a.pools, v2ManagerImage) != nil {
		return inventory{}, errTemporaryActivation
	}
	for _, node := range s.nodes {
		if strings.HasPrefix(text(node, "metadata", "name"), "prod-control-plane-") {
			continue
		}
		if value(node, "spec", "unschedulable") == true || text(node, "metadata", "annotations", fenceAnnotation) != "" || text(node, "status", "allocatable", "cpu") != "8" {
			return inventory{}, errTemporaryActivation
		}
	}
	for _, volume := range s.volumes {
		if text(volume, "spec", "dataEngine") != "v1" {
			return inventory{}, errTemporaryActivation
		}
	}
	if _, err = (hostMemoryReboot{c: a.c}).databases(ctx, nil); err != nil {
		return inventory{}, err
	}
	if err = requireBenchmarkBaseline(ctx, a.c, s.nodes); err != nil {
		return inventory{}, err
	}
	return s, nil
}

// targets collects all native bindings before any disk is enabled. A resumed
// plan must still name the same physical workers and partitions, and no existing
// replica may be assigned to a temporary member during this pre-cutover step.
func (a temporaryV2Activation) targets(ctx context.Context, s inventory, journalUID string, expected map[string]temporaryV2Binding) (map[string]temporaryV2Binding, error) {
	nodes, err := indexed(s.nodes)
	if err != nil {
		return nil, err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return nil, err
	}
	result := map[string]temporaryV2Binding{}
	for _, selection := range temporaryV2Selections {
		name, _, err := resolveTemporaryInventoryTarget(selection, s.nodes, a.pools)
		if err != nil {
			return nil, fmt.Errorf("temporary raw registration held [stage=pool-selection]: %w", err)
		}
		for _, replica := range s.replicas {
			if text(replica, "spec", "nodeID") == name {
				return nil, errTemporaryActivation
			}
		}
		if _, err = temporaryRegistrationState(storage[name], journalUID); err != nil {
			return nil, err
		}
		role := text(nodes[name], "metadata", "labels", migrationPoolLabel)
		binding, err := a.readBinding(ctx, selection, nodes[name], storage[name], a.pools[role])
		if err != nil {
			return nil, errTemporaryActivation
		}
		binding, err = a.prepared(ctx, binding)
		if err != nil || expected != nil && !expected[selection].SameStorageIdentity(binding) {
			return nil, errTemporaryActivation
		}
		result[selection] = binding
	}
	return result, nil
}

// registrationChanges adds ownership and the canonical raw disk in one
// conditional Longhorn-node patch, preserving unrelated metadata.
func registrationChanges(storage object, journalUID string) []object {
	changes := []object{}
	if value(storage, "metadata", "labels") == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/labels", "value": object{}})
	}
	if value(storage, "metadata", "annotations") == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/annotations", "value": object{}})
	}
	changes = append(changes,
		object{"op": "add", "path": "/metadata/labels/" + strings.ReplaceAll(migrationEnabledLabel, "/", "~1"), "value": "true"},
		object{"op": "add", "path": "/metadata/annotations/" + strings.ReplaceAll(temporaryActivationOwner, "/", "~1"), "value": journalUID},
		object{"op": "replace", "path": "/spec/disks", "value": blockDisks()},
		object{"op": "replace", "path": "/spec/allowScheduling", "value": true},
		object{"op": "replace", "path": "/spec/evictionRequested", "value": false})
	return changes
}

// run registers the three temporary raw disks under one durable identity plan.
// An uncertain patch stops the invocation; only a later confirmed owner can
// adopt its exact partial result after the previous workflow has terminated.
func (a temporaryV2Activation) run(ctx context.Context) error {
	if err := sourceCurrent(ctx, a.c.run, a.source); err != nil {
		return err
	}
	s, err := a.environment(ctx)
	if err != nil {
		return err
	}
	cm, record, err := a.read(ctx)
	if err != nil {
		return err
	}
	if cm == nil {
		targets, err := a.targets(ctx, s, "", nil)
		if err != nil {
			return err
		}
		record = temporaryActivationRecord{Version: 1, Source: a.source, OwnerRun: a.owner, Targets: targets}
		cm, err = a.persist(ctx, nil, record)
		if err != nil {
			return err
		}
	} else {
		if _, err = a.targets(ctx, s, text(cm, "metadata", "uid"), record.Targets); err != nil {
			return err
		}
		if record.OwnerRun != a.owner || record.Source != a.source {
			if err = previousRunCompleted(ctx, a.c, record.OwnerRun, record.Source); err != nil {
				return err
			}
			record.OwnerRun, record.Source = a.owner, a.source
			cm, err = a.persist(ctx, cm, record)
			if err != nil {
				return err
			}
		}
	}
	uid := text(cm, "metadata", "uid")
	for _, selection := range temporaryV2Selections {
		s, err = a.environment(ctx)
		if err != nil {
			return err
		}
		if _, err = a.targets(ctx, s, uid, record.Targets); err != nil {
			return err
		}
		storage, err := a.c.get(ctx, "nodes.longhorn.io", "longhorn-system", record.Targets[selection].Name, false)
		if err != nil || text(storage, "metadata", "uid") != record.Targets[selection].LonghornUID {
			return errTemporaryActivation
		}
		registered, err := temporaryRegistrationState(storage, uid)
		if err != nil {
			return err
		}
		if registered {
			continue
		}
		if _, err = a.owned(ctx, uid, record); err != nil {
			return err
		}
		if err = a.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", storage, registrationChanges(storage, uid)...); err != nil {
			return err
		}
	}
	if err = waitReady(ctx, func(ctx context.Context) error {
		if _, err := a.owned(ctx, uid, record); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		s, err := a.environment(ctx)
		if err != nil {
			return err
		}
		if _, err = a.targets(ctx, s, uid, record.Targets); err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		_, err = v2Environment(ctx, a.c, a.pools)
		return err
	}); err != nil {
		return err
	}
	if !record.Completed {
		cm, err = a.owned(ctx, uid, record)
		if err != nil {
			return err
		}
		record.Completed = true
		if _, err = a.persist(ctx, cm, record); err != nil {
			return err
		}
	}
	fmt.Println("Temporary V2 storage is registered on three identity-bound raw disks; clients and original V1 copies are healthy.")
	return nil
}

// accepted is a read-only registration acceptance check. The completed journal must
// come from a terminated protected workflow and still match all three native
// partitions and journal-owned registrations; its cached completion bit alone
// never authorizes IO.
func (a temporaryV2Activation) accepted(ctx context.Context) error {
	cm, record, err := a.read(ctx)
	if err != nil || cm == nil || !record.Completed || previousRunCompleted(ctx, a.c, record.OwnerRun, record.Source) != nil {
		return errTemporaryActivation
	}
	s, err := a.environment(ctx)
	if err != nil {
		return err
	}
	uid := text(cm, "metadata", "uid")
	if _, err = a.targets(ctx, s, uid, record.Targets); err != nil {
		return err
	}
	storage, err := indexed(s.storageNodes)
	if err != nil {
		return err
	}
	for _, binding := range record.Targets {
		registered, err := temporaryRegistrationState(storage[binding.Name], uid)
		if err != nil || !registered {
			return errTemporaryActivation
		}
	}
	return nil
}
