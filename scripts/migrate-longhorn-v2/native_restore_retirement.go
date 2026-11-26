package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

func runNativeRestoreReclaim(ctx context.Context, c client, config []byte) error {
	inputs, err := readNativeRestoreReclaimInputs(ctx, c)
	if err != nil {
		return err
	}
	state := inputs.state
	policies, err := nativeRestoreReclaimFences(inputs.record, inputs.seed, state)
	if err != nil {
		return err
	}
	current, err := c.snapshot(ctx)
	if err != nil || verifyRedundancy(current) != nil {
		return errNativeRestoreEvidence
	}
	tag := "longhorn-native-v2-" + state.Run
	if _, err := qualifyNativeRestoreTargets(state.Targets, current.nodes, current.storageNodes, tag, true, config); err != nil {
		return err
	}
	pod, err := inputs.readConsumer(ctx, c)
	if err != nil {
		return err
	}
	if value(pod, "metadata", "deletionTimestamp") != nil {
		if err := inputs.waitConsumerGone(ctx, c); err != nil {
			return err
		}
	}
	r, err := beginNativeRestoreReclaim(ctx, c, inputs)
	if err != nil {
		return err
	}
	if err := r.removeConsumer(ctx); err != nil {
		return err
	}
	for _, policy := range policies {
		if err := r.installReclaimFence(ctx, policy); err != nil {
			return err
		}
	}
	if err := r.reclaimStorage(ctx, policies); err != nil {
		return err
	}
	if err := waitReady(ctx, func(ctx context.Context) error { return r.resourcesRetired(ctx) }); err != nil {
		return err
	}
	if err := r.advance(ctx, "storage-retired", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if err := r.removeRetiredAssets(ctx, policies, tag); err != nil {
		return err
	}
	after, err := c.snapshot(ctx)
	if err != nil || verifyRedundancy(after) != nil {
		return errNativeRestoreEvidence
	}
	if _, err := qualifyNativeRestoreTargets(state.Targets, after.nodes, after.storageNodes, tag, false, config); err != nil {
		return err
	}
	if _, err := observeBenchmarkWorkspace(ctx, c, inputs.record.NamespaceUID); err != nil {
		return err
	}
	if err := inputs.retainedSeed(ctx, c); err != nil {
		return err
	}
	return r.advance(ctx, "completed", "", benchmarkFenceReceipt{})
}

func (r *nativeRestoreReclaim) removeConsumer(ctx context.Context) error {
	pod, err := r.inputs.readConsumer(ctx, r.c)
	if err != nil {
		return err
	}
	if pod == nil {
		return nil
	}
	if value(pod, "metadata", "deletionTimestamp") != nil {
		return r.inputs.waitConsumerGone(ctx, r.c)
	}
	if err := r.advance(ctx, "consumer-removal-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if err := removeScratch(ctx, r.c, r.progress.Source, "pod", benchmarkNamespace, "/api/v1/namespaces/"+benchmarkNamespace+"/pods/fio", pod); err != nil {
		return err
	}
	return r.advance(ctx, "consumer-removed", "", benchmarkFenceReceipt{})
}

func (inputs nativeRestoreReclaimInputs) readConsumer(ctx context.Context, c client) (object, error) {
	consumer := inputs.state.Resources["SecondPod"]
	pod, err := c.get(ctx, "pod", consumer.Namespace, consumer.Name, true)
	if err != nil || pod == nil {
		return pod, err
	}
	if benchmarkIdentity(pod) != consumer || text(pod, "apiVersion") != "v1" || text(pod, "kind") != "Pod" ||
		text(pod, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != inputs.state.Run ||
		text(pod, "spec", "nodeName") != inputs.state.SecondNode || value(pod, "metadata", "ownerReferences") != nil {
		return nil, errNativeRestoreEvidence
	}
	return pod, nil
}

func (inputs nativeRestoreReclaimInputs) waitConsumerGone(ctx context.Context, c client) error {
	return waitReady(ctx, func(ctx context.Context) error {
		pod, err := inputs.readConsumer(ctx, c)
		if err != nil {
			return errors.Join(errReadinessTerminal, err)
		}
		if pod != nil {
			return errNativeRestoreEvidence
		}
		return nil
	})
}

func (r nativeRestoreReclaim) resourcesRetired(ctx context.Context) error {
	if _, err := r.guard(ctx); err != nil {
		return errors.Join(errReadinessTerminal, err)
	}
	claim := r.inputs.state.Resources["PersistentVolumeClaim"]
	remaining, err := r.c.get(ctx, "persistentvolumeclaim", claim.Namespace, claim.Name, true)
	if err != nil || remaining != nil {
		return errNativeRestoreEvidence
	}
	groups, err := readNativeRestoreStorageGraph(ctx, r.c)
	if err != nil {
		return err
	}
	return verifyNativeRestoreStorageAbsent(r.inputs.state.Resources, groups)
}

func (r *nativeRestoreReclaim) removeRetiredAssets(ctx context.Context, policies []object, tag string) error {
	if err := r.advance(ctx, "auxiliary-cleanup-requested", "", benchmarkFenceReceipt{}); err != nil {
		return err
	}
	if err := r.removeVeleroArtifacts(ctx); err != nil {
		return err
	}
	state := r.inputs.state
	for _, item := range []struct{ receipt, kind, path string }{
		{"StorageClass", "storageclass", "/apis/storage.k8s.io/v1/storageclasses/" + state.Resources["StorageClass"].Name},
		{"NetworkPolicy", "networkpolicy", "/apis/networking.k8s.io/v1/namespaces/" + benchmarkNamespace + "/networkpolicies/fio-default-deny"},
	} {
		if _, err := r.guard(ctx); err != nil {
			return err
		}
		id := state.Resources[item.receipt]
		o, err := r.c.get(ctx, item.kind, id.Namespace, id.Name, false)
		if err != nil || benchmarkIdentity(o) != id {
			return errNativeRestoreEvidence
		}
		if err := removeScratch(ctx, r.c, r.progress.Source, item.kind, id.Namespace, item.path, o); err != nil {
			return err
		}
	}
	for _, target := range state.Targets {
		if err := r.clearTargetTag(ctx, target, tag); err != nil {
			return err
		}
	}
	for _, policy := range policies {
		if err := r.removeFence(ctx, policy); err != nil {
			return err
		}
	}
	return nil
}

func (r nativeRestoreReclaim) clearTargetTag(ctx context.Context, target nativeRestoreTarget, tag string) error {
	if _, err := r.guard(ctx); err != nil {
		return err
	}
	node, err := r.c.get(ctx, "nodes.longhorn.io", "longhorn-system", target.Name, false)
	registered, registrationErr := temporaryRegistrationState(node, target.Preparation.Activation.UID, target.Preparation.selection())
	if err != nil || registrationErr != nil || !registered || text(node, "metadata", "uid") != target.Preparation.LonghornUID ||
		text(node, "status", "diskStatus", "longhorn-v2", "diskUUID") != target.Preparation.DiskUUID ||
		text(node, "metadata", "annotations", benchmarkOwner) != tag || !reflect.DeepEqual(value(node, "spec", "tags"), []any{tag}) {
		return errNativeRestoreEvidence
	}
	if err := sourceCurrent(ctx, r.c.run, r.progress.Source); err != nil {
		return err
	}
	return r.c.patch(ctx, "nodes.longhorn.io", "longhorn-system", node,
		object{"op": "replace", "path": "/spec/tags", "value": []any{}},
		object{"op": "remove", "path": "/metadata/annotations/" + strings.ReplaceAll(benchmarkOwner, "/", "~1")})
}
