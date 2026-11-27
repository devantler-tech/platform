package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
)

const umamiStandbyReconcileAnnotation = "storage.platform.devantler.tech/umami-standby-reconcile"

var errUmamiStandbyReconcile = errors.New("the released Umami standby cannot receive a verified reconciliation event")

func umamiStandbyReconcileChanges(cluster object, record umamiStandbyReplacementRecord, owner umamiReplacementOwner) ([]object, error) {
	name := record.Original.Consumer.Name
	if record.Phase != umamiReplacementOriginalDetached || record.NameRelease == nil ||
		record.NameRelease.executor() != owner || !validBenchmarkOwner(owner.Run, owner.Source) ||
		benchmarkIdentity(cluster) != record.Original.Database || !recoveredNativeObject(cluster, "postgresql.cnpg.io/v1", "Cluster", "umami") ||
		!umamiV2Transition(cluster) || !cnpgCondition(cluster, "ContinuousArchiving") ||
		text(cluster, "status", "currentPrimary") != record.Primary.Name ||
		text(cluster, "status", "targetPrimary") != record.Primary.Name ||
		value(cluster, "status", "readyInstances") != float64(3) || !umamiDeadJoinPhase(cluster, name) {
		return nil, errUmamiStandbyReconcile
	}
	annotations := valueObject(cluster, "metadata", "annotations")
	if annotations == nil {
		return nil, errUmamiStandbyReconcile
	}
	generation := value(cluster, "metadata", "generation")
	if count, ok := nativeObservationUint(generation); !ok || count == 0 {
		return nil, errUmamiStandbyReconcile
	}
	stamp := owner.Run + ":" + owner.Source
	if value(cluster, "metadata", "annotations", umamiStandbyReconcileAnnotation) != nil {
		if annotations[umamiStandbyReconcileAnnotation] == stamp {
			return nil, nil
		}
		return nil, errUmamiStandbyReconcile
	}
	return []object{
		{"op": "test", "path": "/metadata/generation", "value": generation},
		{"op": "test", "path": "/metadata/annotations", "value": annotations},
		{"op": "add", "path": "/metadata/annotations/storage.platform.devantler.tech~1umami-standby-reconcile", "value": stamp},
	}, nil
}

func umamiDeadJoinPhase(cluster object, member string) bool {
	return text(cluster, "status", "phase") == "Creating a new replica" &&
		text(cluster, "status", "phaseReason") == "Creating replica "+member+"-join"
}

func requestUmamiReleasedStandbyReconcile(ctx context.Context, j *umamiStandbyReplacementJournal) error {
	r := j.record
	if r.NameRelease == nil || r.Phase != umamiReplacementOriginalDetached {
		return nil
	}
	if err := observeUmamiReleasedOriginal(ctx, j, false); err != nil {
		return err
	}
	observation, pods, err := observeUmamiUnchangedPeers(ctx, j)
	if err != nil {
		return err
	}
	cluster := observation.Cluster
	if !umamiDeadJoinPhase(cluster, r.Original.Consumer.Name) || value(cluster, "status", "readyInstances") != float64(3) {
		return nil
	}
	for _, pod := range pods {
		if cnpgOwned(pod, cluster) && r.OtherPods[text(pod, "metadata", "name")] != benchmarkIdentity(pod) {
			return nil
		}
	}
	jobs, err := j.c.list(ctx, "jobs.batch", "umami")
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if cnpgOwned(job, cluster) {
			return nil
		}
	}
	claim, err := j.c.get(ctx, "persistentvolumeclaim", "umami", r.Original.Claim.Name, true)
	if err != nil {
		return err
	}
	if claim != nil {
		return nil
	}
	fence := text(umamiNameFencePolicy(r), "metadata", "name")
	for _, kind := range []string{umamiNamePolicyKind, umamiNameBindingKind} {
		current, err := j.c.get(ctx, kind, "", fence, true)
		if err != nil || current != nil {
			return errUmamiStandbyReconcile
		}
	}
	executor := j.executor()
	changes, err := umamiStandbyReconcileChanges(cluster, r, executor)
	if err != nil || len(changes) == 0 {
		return err
	}
	patch, err := json.Marshal(append(identityTests(cluster), changes...))
	if err != nil {
		return err
	}
	raw, err := j.c.kubectl(ctx, patch, "patch", "clusters.postgresql.cnpg.io", r.Original.Database.Name,
		"--namespace=umami", "--type=json", "--patch-file=/dev/stdin", "--dry-run=server", "--output=json")
	var dryRun object
	expected := executor.Run + ":" + executor.Source
	if err != nil || json.Unmarshal(raw, &dryRun) != nil || !verifiedUmamiStandbyReconcilePatch(cluster, dryRun, expected) {
		return errUmamiStandbyReconcile
	}
	if _, err := j.read(ctx); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, j.c.run, executor.Source); err != nil {
		return err
	}
	if err := j.c.patch(ctx, "clusters.postgresql.cnpg.io", "umami", cluster, changes...); err != nil {
		// A concurrent controller write can be the very event that gives the
		// standby another attempt. Let normal replacement verification decide.
		if errors.Is(err, errConflict) {
			return nil
		}
		return err
	}
	readback, err := j.c.get(ctx, "clusters.postgresql.cnpg.io", "umami", r.Original.Database.Name, false)
	if err != nil || !verifiedUmamiStandbyReconcilePatch(cluster, readback, expected) {
		return errUmamiStandbyReconcile
	}
	return nil
}

func verifiedUmamiStandbyReconcilePatch(before, after object, expected string) bool {
	annotations := maps.Clone(valueObject(before, "metadata", "annotations"))
	if annotations == nil || expected == "" {
		return false
	}
	annotations[umamiStandbyReconcileAnnotation] = expected
	return recoveredNativeObject(after, "postgresql.cnpg.io/v1", "Cluster", "umami") &&
		benchmarkIdentity(before) == benchmarkIdentity(after) &&
		value(before, "metadata", "generation") == value(after, "metadata", "generation") &&
		reflect.DeepEqual(value(before, "spec"), value(after, "spec")) &&
		reflect.DeepEqual(value(after, "metadata", "labels"), value(before, "metadata", "labels")) &&
		reflect.DeepEqual(value(after, "metadata", "ownerReferences"), value(before, "metadata", "ownerReferences")) &&
		reflect.DeepEqual(value(after, "metadata", "annotations"), annotations)
}
