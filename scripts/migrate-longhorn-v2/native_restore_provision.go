package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
)

type nativeRestoreRun struct {
	c          client
	config     []byte
	plan       nativeRestoreAdmission
	state      nativeRestoreState
	source     string
	journalUID string
}

// Provision an independent V2 claim before asking Velero to restore the
// consumer. An ambiguous write leaves its original intent in the journal.
func (r *nativeRestoreRun) provision(ctx context.Context) error {
	r.state = r.plan.State
	for _, asset := range r.plan.Assets {
		actual, err := r.asset(ctx, asset, true)
		if err != nil || actual != nil {
			return errNativeRestoreEvidence
		}
	}
	pod, err := r.c.get(ctx, "pods", benchmarkNamespace, "fio", true)
	if err != nil || pod != nil {
		return errNativeRestoreEvidence
	}
	if err := r.checkActivation(ctx); err != nil {
		return err
	}
	r.journalUID, err = createNativeRestoreJournal(ctx, r.c, r.plan.Seed, r.state)
	if err != nil {
		return err
	}
	if err := r.tagTargets(ctx); err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{"NetworkPolicy", "policy"}, {"StorageClass", "class"}, {"PersistentVolumeClaim", "claim"},
	} {
		if err := r.createAsset(ctx, pair[0], pair[1]); err != nil {
			return err
		}
	}
	return waitReady(ctx, r.recordStorageBinding)
}

func (r *nativeRestoreRun) guard(ctx context.Context) error {
	source, executor, err := nativeRestoreGrant(os.Getenv, r.state)
	if err != nil || source != r.source {
		return errNativeRestoreEvidence
	}
	journal, record, seed, state, err := readNativeRestoreJournal(ctx, r.c)
	if err != nil || text(journal, "metadata", "uid") != r.journalUID || !reflect.DeepEqual(state, r.state) ||
		!reflect.DeepEqual(record, r.plan.Seed.Record) || seed != r.plan.Seed.Seed || !nativeRestoreExecutorMatches(journal, executor) {
		return errNativeRestoreEvidence
	}
	if err := r.checkActivation(ctx); err != nil {
		return err
	}
	return sourceCurrent(ctx, r.c.run, source)
}

func (r *nativeRestoreRun) checkActivation(ctx context.Context) error {
	if !nativeRestoreFixedRoster(r.state.Targets) {
		return errNativeRestoreEvidence
	}
	planned := r.state.Targets[0].Preparation
	activation, err := r.c.get(ctx, "configmap", "longhorn-system", temporaryActivationJournal, false)
	if err != nil || value(activation, "metadata", "deletionTimestamp") != nil ||
		!sameNativeRestoreActivation(temporaryActivationReceipt{Journal: planned.Activation, RecordSHA256: planned.ActivationSHA256}, activation) {
		return errNativeRestoreEvidence
	}
	return nil
}

func (r *nativeRestoreRun) advance(ctx context.Context, phase string, update func(*nativeRestoreState)) error {
	next := r.state
	next.Phase = phase
	next.Resources = maps.Clone(r.state.Resources)
	if update != nil {
		update(&next)
	}
	if err := saveNativeRestoreState(ctx, r.c, r.journalUID, r.state, next); err != nil {
		return err
	}
	r.state = next
	return nil
}

func (r *nativeRestoreRun) tagTargets(ctx context.Context) error {
	tag := "longhorn-native-v2-" + r.state.Run
	s, err := r.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if _, err = qualifyNativeRestoreTargets(r.state.Targets, s.nodes, s.storageNodes, tag, false, r.config); err != nil {
		return err
	}
	if err := r.advance(ctx, "target-tags-requested", nil); err != nil {
		return err
	}
	for _, target := range r.state.Targets {
		if err := r.guard(ctx); err != nil {
			return err
		}
		n, err := r.c.get(ctx, "nodes.longhorn.io", "longhorn-system", target.Name, false)
		registered, registrationErr := temporaryRegistrationState(n, target.Preparation.Activation.UID, target.Preparation.selection())
		oldTags := value(n, "spec", "tags")
		if err != nil || registrationErr != nil || !registered || text(n, "metadata", "uid") != target.Preparation.LonghornUID ||
			text(n, "status", "diskStatus", "longhorn-v2", "diskUUID") != target.Preparation.DiskUUID ||
			text(n, "metadata", "annotations", benchmarkOwner) != "" || oldTags != nil && !reflect.DeepEqual(oldTags, []any{}) {
			return errNativeRestoreEvidence
		}
		changes := []object{{"op": "add", "path": "/spec/tags", "value": []any{tag}}}
		changes = append(changes, metadataChanges(n, "annotations", map[string]string{benchmarkOwner: tag})...)
		if err := r.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", n, changes...); err != nil {
			return fmt.Errorf("temporary storage tag write could not be confirmed: %w", err)
		}
	}
	s, err = r.c.snapshot(ctx)
	if err != nil {
		return err
	}
	if _, err = qualifyNativeRestoreTargets(r.state.Targets, s.nodes, s.storageNodes, tag, true, r.config); err != nil {
		return err
	}
	return r.advance(ctx, "target-tags-owned", nil)
}

func (r *nativeRestoreRun) asset(ctx context.Context, asset object, optional bool) (object, error) {
	kind := map[string]string{
		"NetworkPolicy": "networkpolicies", "StorageClass": "storageclasses", "PersistentVolumeClaim": "persistentvolumeclaims",
		"ConfigMap": "configmaps", "Restore": "restores.velero.io",
	}[text(asset, "kind")]
	return r.c.get(ctx, kind, text(asset, "metadata", "namespace"), text(asset, "metadata", "name"), optional)
}

func (r *nativeRestoreRun) createAsset(ctx context.Context, kind, phase string) error {
	var desired object
	for _, asset := range r.plan.Assets {
		if text(asset, "kind") == kind {
			desired = asset
			break
		}
	}
	if desired == nil {
		return errNativeRestoreEvidence
	}
	if err := r.advance(ctx, phase+"-create-requested", nil); err != nil {
		return err
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	created, err := r.c.create(ctx, r.source, desired)
	if err != nil {
		return fmt.Errorf("%s creation could not be confirmed: %w", kind, err)
	}
	actual, err := r.asset(ctx, desired, false)
	id := benchmarkIdentity(created)
	if err != nil || id.UID == "" || benchmarkIdentity(actual) != id ||
		text(actual, "kind") != kind || value(actual, "metadata", "deletionTimestamp") != nil {
		return errNativeRestoreEvidence
	}
	if kind == "NetworkPolicy" && !reflect.DeepEqual(actual["spec"], desired["spec"]) ||
		kind == "ConfigMap" && !reflect.DeepEqual(actual["data"], desired["data"]) {
		return errNativeRestoreEvidence
	}
	return r.advance(ctx, phase+"-owned", func(next *nativeRestoreState) {
		next.Resources[kind] = id
	})
}

func (r *nativeRestoreRun) recordStorageBinding(ctx context.Context) error {
	if err := r.guard(ctx); err != nil {
		return errors.Join(errReadinessTerminal, err)
	}
	claimID, classID := r.state.Resources["PersistentVolumeClaim"], r.state.Resources["StorageClass"]
	claim, err := r.c.get(ctx, "persistentvolumeclaims", claimID.Namespace, claimID.Name, false)
	if err != nil || benchmarkIdentity(claim) != claimID || text(claim, "status", "phase") != "Bound" {
		return errNativeRestoreEvidence
	}
	class, err := r.c.get(ctx, "storageclasses", "", classID.Name, false)
	if err != nil || benchmarkIdentity(class) != classID {
		return errNativeRestoreEvidence
	}
	pv, err := r.c.get(ctx, "persistentvolumes", "", text(claim, "spec", "volumeName"), false)
	if err != nil {
		return errNativeRestoreEvidence
	}
	volume, err := r.c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(pv, "spec", "csi", "volumeHandle"), false)
	if err != nil {
		return errNativeRestoreEvidence
	}
	storage := r.state
	storage.Phase = "storage-bound"
	storage.Resources = maps.Clone(r.state.Resources)
	storage.Resources["PersistentVolume"], storage.Resources["LonghornVolume"] = benchmarkIdentity(pv), benchmarkIdentity(volume)
	if err := verifyNativeRestoreStorage(r.plan.Seed.Record, r.plan.Seed.Seed, storage, claim, class, pv, volume); err != nil {
		return err
	}
	if err := saveNativeRestoreState(ctx, r.c, r.journalUID, r.state, storage); err != nil {
		return errors.Join(errReadinessTerminal, fmt.Errorf("recording the bound V2 storage could not be confirmed: %w", err))
	}
	r.state = storage
	return nil
}
