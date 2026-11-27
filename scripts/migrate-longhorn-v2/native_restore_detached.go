package main

import (
	"context"
	"os"
	"time"
)

type nativeRestoreDetachedObservation struct {
	Claim, Class, PV                   object
	Inventory                          inventory
	Frontends, Attachments, CSITickets []object
}

func observeNativeRestoreDetached(ctx context.Context, c client, record benchmarkRecord, seed canarySeedState, state nativeRestoreState) error {
	source, _, err := nativeRestoreGrant(os.Getenv, state)
	if !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed || err != nil ||
		state.validate(record, seed) != nil || !nativeRestoreDetachPhase(state.Phase) {
		return errNativeRestoreEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if sourceCurrent(ctx, c.run, source) != nil {
		return errNativeRestoreEvidence
	}
	for range 2 {
		observed, err := readNativeRestoreDetached(ctx, c, state)
		if err != nil || verifyNativeRestoreDetached(record, seed, state, observed) != nil {
			return errNativeRestoreEvidence
		}
	}
	if sourceCurrent(ctx, c.run, source) != nil || !commandContextActive(ctx) {
		return errNativeRestoreEvidence
	}
	return nil
}

func readNativeRestoreDetached(ctx context.Context, c client, state nativeRestoreState) (nativeRestoreDetachedObservation, error) {
	var observed nativeRestoreDetachedObservation
	for _, query := range []struct {
		resource, receipt string
		result            *object
	}{
		{"persistentvolumeclaim", "PersistentVolumeClaim", &observed.Claim},
		{"storageclass", "StorageClass", &observed.Class},
		{"persistentvolume", "PersistentVolume", &observed.PV},
	} {
		id := state.Resources[query.receipt]
		o, err := c.get(ctx, query.resource, id.Namespace, id.Name, false)
		if err != nil {
			return observed, err
		}
		*query.result = o
	}
	var err error
	observed.Inventory, err = c.snapshot(ctx)
	if err != nil {
		return observed, err
	}
	for _, query := range []struct {
		resource, namespace string
		result              *[]object
	}{
		{"enginefrontends.longhorn.io", "longhorn-system", &observed.Frontends},
		{"volumeattachments.longhorn.io", "longhorn-system", &observed.Attachments},
		{"volumeattachments.storage.k8s.io", "", &observed.CSITickets},
	} {
		list, err := c.list(ctx, query.resource, query.namespace)
		if err != nil {
			return observed, err
		}
		*query.result = list
	}
	return observed, nil
}

func nativeRestoreDetachPhase(phase string) bool {
	if stage := nativeReattachmentStage(phase); stage > 0 {
		return stage < nativeReattachmentStage(nativeReattachmentReplacement)
	}
	if stage := nativeRestoreExpiredStage(phase); stage > 0 {
		return stage < nativeRestoreExpiredStage(nativeExpiredReplacement)
	}
	return phase == "first-pod-delete-requested" || phase == "first-pod-deleted" || phase == "reattach-create-requested"
}

// The very first expired-consumer check proves detachment while the exact
// terminal Pod object still exists. All subsequent detach checks require it gone.
func verifyNativeRestoreExpiredDetached(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, observed nativeRestoreDetachedObservation) error {
	if state.Phase != nativeExpiredDeleteRequested || state.Expired == nil || observed.Inventory.pods == nil {
		return errNativeRestoreEvidence
	}
	found := false
	pods := make([]object, 0, len(observed.Inventory.pods))
	for _, pod := range observed.Inventory.pods {
		if benchmarkIdentity(pod) != state.Expired.Pod {
			pods = append(pods, pod)
			continue
		}
		if found || !nativeRestorePodExpired(pod) {
			return errNativeRestoreEvidence
		}
		found = true
	}
	if !found {
		return errNativeRestoreEvidence
	}
	observed.Inventory.pods = pods
	return verifyNativeRestoreDetached(record, seed, state, observed)
}

// Reattachment may reuse the claim only after the old consumer and all
// attachment requests are gone. Keep the original claim, PV and backend.
func verifyNativeRestoreDetached(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, observed nativeRestoreDetachedObservation) error {
	if !nativeRestoreDetachPhase(state.Phase) {
		return errNativeRestoreEvidence
	}
	return verifyNativeRestoreQuiescence(record, seed, state, observed)
}

func verifyNativeRestoreQuiescence(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, observed nativeRestoreDetachedObservation) error {
	s := observed.Inventory
	volumes, err := indexed(s.volumes)
	if err != nil {
		return errNativeRestoreEvidence
	}
	volume := volumes[state.Resources["LonghornVolume"].Name]
	if verifyNativeRestoreStorage(record, seed, state, observed.Claim, observed.Class, observed.PV, volume) != nil ||
		verifyRedundancy(s) != nil || verifyRecoveredBenchmarkDetached(volume, observed.Attachments, s.engines, observed.Frontends) != nil {
		return errNativeRestoreEvidence
	}
	used := nativeRestoreReceiptUIDs(record, seed, state)
	if !nativeRestoreDetachedConsumersAbsent(state, s.pods, observed.CSITickets, used) ||
		!nativeRestoreDetachedInstances(state.Targets, volume, s, observed.Frontends, used) {
		return errNativeRestoreEvidence
	}
	return nil
}

func nativeRestoreDetachedConsumersAbsent(state nativeRestoreState, pods, tickets []object, used map[string]bool) bool {
	if pods == nil || tickets == nil {
		return false
	}
	for _, pod := range pods {
		id := benchmarkIdentity(pod)
		if !nativeRestoreUID(id.UID) || id.Name == "" || id.Namespace == "" || id.Namespace == benchmarkNamespace || used[id.UID] {
			return false
		}
		used[id.UID] = true
	}
	for _, ticket := range tickets {
		id := benchmarkIdentity(ticket)
		pv := text(ticket, "spec", "source", "persistentVolumeName")
		if text(ticket, "apiVersion") != "storage.k8s.io/v1" || text(ticket, "kind") != "VolumeAttachment" ||
			!nativeRestoreUID(id.UID) || id.Name == "" || id.Namespace != "" || used[id.UID] ||
			text(ticket, "metadata", "resourceVersion") == "" || pv == "" || pv == state.Resources["PersistentVolume"].Name {
			return false
		}
		used[id.UID] = true
	}
	return true
}

func nativeRestoreDetachedInstances(targets []nativeRestoreTarget, volume object, s inventory, frontends []object, used map[string]bool) bool {
	placements := map[string]nativeRestorePreparationReceipt{}
	for _, target := range targets {
		placements[target.Name] = target.Preparation
	}
	copies := map[string]bool{}
	volumeName := text(volume, "metadata", "name")
	for _, group := range []struct {
		kind  string
		items []object
	}{{"Engine", s.engines}, {"EngineFrontend", frontends}, {"Replica", s.replicas}} {
		for _, item := range group.items {
			if text(item, "spec", "volumeName") != volumeName {
				continue
			}
			if !nativeRestoreInstanceIdentity(item, group.kind, volume, used) || text(item, "spec", "dataEngine") != "v2" ||
				text(item, "spec", "desireState") != "stopped" || text(item, "status", "currentState") != "stopped" ||
				!absentOrEmptyString(value(item, "status", "endpoint")) {
				return false
			}
			if group.kind != "Replica" || value(item, "spec", "active") != true {
				continue
			}
			node := text(item, "spec", "nodeID")
			layout := placements[node]
			path, err := layout.mapper(node)
			if err != nil || copies[node] || text(item, "spec", "diskID") != layout.DiskUUID || text(item, "spec", "diskPath") != path {
				return false
			}
			copies[node] = true
		}
	}
	return len(copies) == len(targets)
}
