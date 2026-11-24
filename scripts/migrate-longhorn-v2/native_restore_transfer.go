package main

import "reflect"

type nativeRestoreTransferReceipt struct {
	Pod, Transfer benchmarkResource
	Node          string
}

// Bind Velero's completed copy to the original seed snapshot and a fresh
// consumer of the independently owned claim. The executor must also rebind
// storage, targets and source absence, qualify the executable Pod, and read
// the original payload checksum before advancing its restoration journal.
func verifyNativeRestoreTransfer(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, restore, pod object, transfers, repositories []object) (nativeRestoreTransferReceipt, error) {
	fail := func() (nativeRestoreTransferReceipt, error) {
		return nativeRestoreTransferReceipt{}, errNativeRestoreEvidence
	}
	rank := nativeRestorePhaseIndex(state.Phase)
	if state.validate(record, seed) != nil || rank < nativeRestorePhaseIndex("restore-owned") || rank > nativeRestorePhaseIndex("write-verified") ||
		benchmarkIdentity(restore) != state.Resources["Restore"] || text(restore, "apiVersion") != "velero.io/v1" || text(restore, "kind") != "Restore" ||
		!seedVeleroIdentity(restore) || cleanVeleroResult(restore) != nil || transfers == nil || repositories == nil {
		return fail()
	}
	targets := map[string]string{}
	for _, target := range state.Targets {
		targets[target.Name] = target.UID
	}
	assets, err := nativeRestoreAssets(record, seed, state.Run, targets)
	if err != nil {
		return fail()
	}
	for _, asset := range assets {
		if text(asset, "kind") != "Restore" {
			continue
		}
		for key, expected := range valueObject(asset, "spec") {
			if !reflect.DeepEqual(value(restore, "spec", key), expected) {
				return fail()
			}
		}
	}
	for _, key := range []string{"namespaceMapping", "hooks", "orLabelSelectors", "restoreStatus"} {
		if value(restore, "spec", key) != nil {
			return fail()
		}
	}
	first := state.Targets[0].Name
	identity := benchmarkIdentity(pod)
	if text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" || identity.Name != "fio" || identity.Namespace != benchmarkNamespace ||
		!nativeRestoreUID(identity.UID) || text(pod, "metadata", "resourceVersion") == "" || value(pod, "metadata", "deletionTimestamp") != nil ||
		text(pod, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != state.Run || text(pod, "metadata", "labels", "velero.io/restore-name") != state.Resources["Restore"].Name ||
		text(pod, "spec", "nodeName") != first || text(pod, "status", "phase") != "Running" || !ready(valueObject(pod, "status")) {
		return fail()
	}
	if !nativeRestoreChildIdentity(record, seed, state, "FirstPod", identity) {
		return fail()
	}
	volumes, valid := value(pod, "spec", "volumes").([]any)
	seen := map[string]bool{}
	for _, raw := range volumes {
		volume, ok := raw.(object)
		name := text(volume, "name")
		if !ok || name == "" || seen[name] || name == "data" && (len(volume) != 2 || text(volume, "persistentVolumeClaim", "claimName") != "fio-data" || value(volume, "persistentVolumeClaim", "readOnly") == true) ||
			name != "data" && value(volume, "persistentVolumeClaim") != nil {
			return fail()
		}
		seen[name] = true
	}
	if !valid || !seen["data"] {
		return fail()
	}
	var repository object
	for _, candidate := range repositories {
		if benchmarkIdentity(candidate) == seed.Repository {
			digest, err := seedSpecDigest(candidate)
			if err != nil || digest != seed.RepositorySpec || repository != nil {
				return fail()
			}
			repository = candidate
		}
	}
	proof := canaryRun{kopiaRepository: repository}
	if repository == nil {
		return fail()
	}
	if _, err := proof.verifyFilesystemTransfer(repositories, transfers, restore, pod, seed.Snapshot, benchmarkNamespace); err != nil {
		return fail()
	}
	for _, transfer := range transfers {
		if controlledBy(transfer, restore) {
			id := benchmarkIdentity(transfer)
			if id.UID == identity.UID || !nativeRestoreChildIdentity(record, seed, state, "PodVolumeRestore", id) {
				return fail()
			}
			return nativeRestoreTransferReceipt{Pod: identity, Transfer: id, Node: first}, nil
		}
	}
	return fail()
}

func nativeRestoreChildIdentity(record benchmarkRecord, seed canarySeedState, state nativeRestoreState, kind string, identity benchmarkResource) bool {
	if !nativeRestoreUID(identity.UID) {
		return false
	}
	for _, original := range record.Resources {
		if identity.UID == original.UID {
			return false
		}
	}
	for _, original := range []benchmarkResource{seed.Location, seed.Backup, seed.Transfer, seed.Repository, state.SeedJournal} {
		if identity.UID == original.UID {
			return false
		}
	}
	for _, target := range state.Targets {
		if identity.UID == target.UID {
			return false
		}
	}
	for key, owned := range state.Resources {
		if key == kind && identity != owned || key != kind && identity.UID == owned.UID {
			return false
		}
	}
	return true
}
