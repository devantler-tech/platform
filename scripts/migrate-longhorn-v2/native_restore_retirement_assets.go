package main

import (
	"context"
	"reflect"
)

func (r nativeRestoreReclaim) removeVeleroArtifacts(ctx context.Context) error {
	restore, modifier, transfer, err := r.inputs.veleroArtifacts(ctx, r.c)
	if err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	const veleroPath = "/apis/velero.io/v1/namespaces/velero"
	state := r.inputs.state
	restoreName := state.Resources["Restore"].Name
	if err := r.c.deleteObservedWithPropagation(ctx, r.progress.Source, veleroPath+"/restores/"+restoreName, restore, "Orphan"); err != nil {
		return err
	}
	if err := r.waitAbsent(ctx, "restores.velero.io", "velero", restoreName); err != nil {
		return err
	}
	transferID := state.Resources["PodVolumeRestore"]
	current, err := r.c.get(ctx, "podvolumerestores.velero.io", transferID.Namespace, transferID.Name, false)
	owners, _ := value(current, "metadata", "ownerReferences").([]any)
	if err != nil || benchmarkIdentity(current) != transferID || len(owners) != 0 ||
		!reflect.DeepEqual(value(current, "spec"), value(transfer, "spec")) || cleanVeleroResult(current) != nil {
		return errNativeRestoreEvidence
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.progress.Source, veleroPath+"/podvolumerestores/"+transferID.Name, current); err != nil {
		return err
	}
	if err := r.waitAbsent(ctx, "podvolumerestores.velero.io", "velero", transferID.Name); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	return removeScratch(ctx, r.c, r.progress.Source, "configmap", "velero",
		"/api/v1/namespaces/velero/configmaps/"+state.Resources["ConfigMap"].Name, modifier)
}

func (r nativeRestoreReclaim) waitAbsent(ctx context.Context, kind, ns, name string) error {
	return waitReady(ctx, func(ctx context.Context) error {
		current, err := r.c.get(ctx, kind, ns, name, true)
		if err != nil || current != nil {
			return errNativeRestoreEvidence
		}
		return nil
	})
}

func (r nativeRestoreReclaim) removeFence(ctx context.Context, desired object) error {
	name := text(desired, "metadata", "name")
	policy, err := r.c.get(ctx, reclaimPolicyKind, "", name, false)
	if err != nil {
		return err
	}
	binding, err := r.c.get(ctx, reclaimBindingKind, "", name, false)
	if err != nil || !matchesOwnedBenchmarkFence(policy, binding, desired, r.progress.Fences[name]) {
		return errNativeRestoreEvidence
	}
	const admissionPath = "/apis/admissionregistration.k8s.io/v1"
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.progress.Source, admissionPath+"/validatingadmissionpolicybindings/"+name, binding); err != nil {
		return err
	}
	if err := r.waitAbsent(ctx, reclaimBindingKind, "", name); err != nil {
		return err
	}
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.c.deleteObserved(ctx, r.progress.Source, admissionPath+"/validatingadmissionpolicies/"+name, policy); err != nil {
		return err
	}
	return r.waitAbsent(ctx, reclaimPolicyKind, "", name)
}
