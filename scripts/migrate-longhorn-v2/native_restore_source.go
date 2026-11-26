package main

import (
	"context"
	"os"
	"reflect"
	"time"
)

type nativeRestoreSeedReceipt struct {
	Journal benchmarkResource
	Record  benchmarkRecord
	Seed    canarySeedState
}

// Use the original seed Pod's recorded identity to verify its remote backup.
// The seed Pod has already been removed and its name may be used by the restore.
func observeNativeRestoreSeed(ctx context.Context, c client, source, run string) (nativeRestoreSeedReceipt, error) {
	if !commandContextActive(ctx) || c.run == nil || c.context != "admin@prod" || c.nativeSeed || authorizeNativeRestore(os.Getenv) != nil ||
		source != os.Getenv("GITHUB_SHA") || run != os.Getenv("GITHUB_RUN_ID") {
		return nativeRestoreSeedReceipt{}, errNativeRestoreEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if sourceCurrent(ctx, c.run, source) != nil {
		return nativeRestoreSeedReceipt{}, errNativeRestoreEvidence
	}
	seedClient := c
	seedClient.nativeSeed = true
	journal, record, seed, err := readCanarySeedJournal(ctx, seedClient)
	if err != nil || !nativeRestoreUID(benchmarkIdentity(journal).UID) || seed.Phase != "source-cleaned" || seed.Run == run {
		return nativeRestoreSeedReceipt{}, errNativeRestoreEvidence
	}
	if err := observeNativeRestoreSourceAbsent(ctx, c, record, seed); err != nil {
		return nativeRestoreSeedReceipt{}, err
	}
	pod := record.Resources["Pod"]
	archived := object{"metadata": object{"name": pod.Name, "namespace": pod.Namespace, "uid": pod.UID}}
	reader := nativeSeedRun{c: c, record: record, state: seed, created: map[string]object{"Pod": archived}}
	if err := reader.retainedBackup(ctx); err != nil {
		return nativeRestoreSeedReceipt{}, err
	}
	current, currentRecord, currentSeed, err := readCanarySeedJournal(ctx, seedClient)
	if err != nil || benchmarkIdentity(current) != benchmarkIdentity(journal) || !reflect.DeepEqual(currentRecord, record) || currentSeed != seed ||
		sourceCurrent(ctx, c.run, source) != nil || !commandContextActive(ctx) {
		return nativeRestoreSeedReceipt{}, errNativeRestoreEvidence
	}
	return nativeRestoreSeedReceipt{Journal: benchmarkIdentity(journal), Record: record, Seed: seed}, nil
}

var nativeRestoreSourceQueries = []struct{ resource, namespace, association string }{
	{"persistentvolumes", "", ""},
	{"volumes.longhorn.io", "longhorn-system", ""},
	{"engines.longhorn.io", "longhorn-system", "volumeName"},
	{"replicas.longhorn.io", "longhorn-system", "volumeName"},
	{"enginefrontends.longhorn.io", "longhorn-system", "volumeName"},
	{"volumeattachments.longhorn.io", "longhorn-system", "volume"},
	{"snapshots.longhorn.io", "longhorn-system", "volume"},
	{"volumeattachments.storage.k8s.io", "", "persistentVolumeName"},
}

// A historical cleanup receipt is necessary but cannot substitute for current
// API observations. This check admits no writes and proves neither physical
// process absence nor restoration: those require their own native evidence.
func verifyNativeRestoreSourceAbsent(record benchmarkRecord, seed canarySeedState, groups map[string][]object) error {
	if seed.validate(record) != nil || seed.Phase != "source-cleaned" || len(groups) != len(nativeRestoreSourceQueries) {
		return errNativeRestoreEvidence
	}
	backend, pv := record.Resources["LonghornVolume"], record.Resources["PersistentVolume"]
	seen := map[string]bool{}
	for _, identity := range record.Resources {
		seen[identity.UID] = true
	}
	for _, query := range nativeRestoreSourceQueries {
		items, present := groups[query.resource]
		if !present || items == nil {
			return errNativeRestoreEvidence
		}
		names := map[string]bool{}
		for _, item := range items {
			identity := benchmarkIdentity(item)
			if !nativeRestoreUID(identity.UID) || identity.Name == "" || identity.Namespace != query.namespace ||
				text(item, "metadata", "resourceVersion") == "" || seen[identity.UID] || names[identity.Name] {
				return errNativeRestoreEvidence
			}
			seen[identity.UID], names[identity.Name] = true, true
			if query.resource == "persistentvolumes" {
				if identity.Name == pv.Name || text(item, "spec", "csi", "driver") == "driver.longhorn.io" && text(item, "spec", "csi", "volumeHandle") == backend.Name {
					return errNativeRestoreEvidence
				}
				continue
			}
			if query.resource == "volumes.longhorn.io" {
				if identity.Name == backend.Name {
					return errNativeRestoreEvidence
				}
				continue
			}
			association := text(item, "spec", query.association)
			if query.resource == "volumeattachments.storage.k8s.io" {
				association = text(item, "spec", "source", query.association)
				if association == "" || association == pv.Name {
					return errNativeRestoreEvidence
				}
				continue
			}
			if association == "" || association == backend.Name || text(item, "metadata", "labels", "longhornvolume") == backend.Name ||
				query.resource == "volumeattachments.longhorn.io" && identity.Name == backend.Name {
				return errNativeRestoreEvidence
			}
			refs, valid := value(item, "metadata", "ownerReferences").([]any)
			if !valid && value(item, "metadata", "ownerReferences") != nil {
				return errNativeRestoreEvidence
			}
			for _, raw := range refs {
				ref, valid := raw.(object)
				if !valid || text(ref, "uid") == "" || text(ref, "uid") == backend.UID ||
					text(ref, "kind") == "Volume" && text(ref, "name") == backend.Name {
					return errNativeRestoreEvidence
				}
			}
		}
	}
	return nil
}

func observeNativeRestoreSourceAbsent(ctx context.Context, c client, record benchmarkRecord, seed canarySeedState) error {
	if seed.validate(record) != nil || seed.Phase != "source-cleaned" {
		return errNativeRestoreEvidence
	}
	groups := map[string][]object{}
	for _, query := range nativeRestoreSourceQueries {
		items, err := c.list(ctx, query.resource, query.namespace)
		if err != nil {
			return errNativeRestoreEvidence
		}
		groups[query.resource] = items
	}
	return verifyNativeRestoreSourceAbsent(record, seed, groups)
}
