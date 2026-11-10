package main

import (
	"context"
	"encoding/json"
	"errors"
)

// Native kubectl raw DELETE accepts DeleteOptions. Unlike name-only deletion,
// the API atomically refuses changed or replaced objects through preconditions.
func (c client) deleteObserved(ctx context.Context, source, path string, o object) error {
	uid, version := text(o, "metadata", "uid"), text(o, "metadata", "resourceVersion")
	if uid == "" || version == "" {
		return errors.New("deletion requires the observed resource identity")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	body, err := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Foreground",
		"preconditions": object{"uid": uid, "resourceVersion": version}})
	if err != nil {
		return err
	}
	_, err = c.kubectl(ctx, body, "delete", "--raw="+path, "--filename=/dev/stdin")
	return err
}

// A controller can update a scratch resource between the read and the delete.
// Only that conflict is retried, each time from a fresh read of the same object,
// and only for kinds that identity alone protects. Every other kind was cleared by
// checks made before removeScratch, which a retry would not repeat, so it stops.
const scratchDeleteAttempts = 3

var scratchDeleteRetryKinds = map[string]bool{"pod": true, "persistentvolumeclaim": true}

func removeScratch(ctx context.Context, c client, source, kind, namespace, path string, original object) error {
	for attempt := 1; ; attempt++ {
		current, err := c.get(ctx, kind, namespace, text(original, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if text(current, "metadata", "uid") != text(original, "metadata", "uid") {
			return errors.New("scratch resource was replaced; refusing deletion")
		}
		err = c.deleteObserved(ctx, source, path, current)
		if err == nil {
			break
		}
		if !errors.Is(err, errConflict) || !scratchDeleteRetryKinds[kind] || attempt == scratchDeleteAttempts {
			return err
		}
	}
	return waitReady(ctx, func(ctx context.Context) error {
		o, err := c.get(ctx, kind, namespace, text(original, "metadata", "name"), true)
		if err != nil {
			return err
		}
		if o != nil {
			return errors.New("scratch resource deletion has not completed")
		}
		return nil
	})
}

func verifyScratchBinding(pv, volume, claim, class object) error {
	if text(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" ||
		text(pv, "spec", "claimRef", "uid") != text(claim, "metadata", "uid") || text(claim, "metadata", "uid") == "" ||
		text(pv, "spec", "claimRef", "namespace") != text(claim, "metadata", "namespace") ||
		text(pv, "spec", "claimRef", "name") != "fio-data" || text(pv, "spec", "storageClassName") != text(class, "metadata", "name") ||
		text(pv, "spec", "csi", "driver") != "driver.longhorn.io" || text(pv, "spec", "csi", "volumeHandle") != text(volume, "metadata", "name") ||
		text(volume, "metadata", "uid") == "" || text(volume, "status", "kubernetesStatus", "pvName") != text(pv, "metadata", "name") {
		return errors.New("scratch volume ownership is incomplete; no volume may be retired")
	}
	return nil
}

func observeBenchmarkWorkspace(ctx context.Context, c client, expectedUID string) (object, error) {
	namespace, err := c.get(ctx, "namespace", "", benchmarkNamespace, false)
	uid := text(namespace, "metadata", "uid")
	if err != nil || text(namespace, "apiVersion") != "v1" || text(namespace, "kind") != "Namespace" ||
		text(namespace, "metadata", "name") != benchmarkNamespace || uid == "" || expectedUID != "" && uid != expectedUID ||
		text(namespace, "status", "phase") != "Active" || value(namespace, "metadata", "deletionTimestamp") != nil ||
		text(namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" ||
		text(namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce-version") != "latest" ||
		text(namespace, "metadata", "labels", "velero.io/exclude-from-backup") != "true" ||
		text(namespace, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/prune") != "disabled" {
		return nil, errors.New("the reviewed GitOps benchmark namespace is missing, replaced or unverified")
	}
	return namespace, nil
}

func cleanupBenchmark(ctx context.Context, c client, record *benchmarkRecord, v1Runtime benchmarkV1Runtime, created map[string]object, claim, pv, volume object) error {
	source, namespace := record.Source, record.Namespace
	if record.Version != 3 || namespace != benchmarkNamespace || record.NamespaceUID == "" {
		return errors.New("scratch cleanup requires the dedicated benchmark namespace receipt")
	}
	if _, err := observeBenchmarkWorkspace(ctx, c, record.NamespaceUID); err != nil {
		return err
	}
	class := created["StorageClass"]
	if benchmarkIdentity(claim) != benchmarkIdentity(created["PersistentVolumeClaim"]) ||
		text(claim, "status", "phase") != "Bound" || text(claim, "spec", "volumeName") != text(pv, "metadata", "name") {
		return errors.New("the bound benchmark claim differs from its original creation receipt")
	}
	if err := verifyScratchBinding(pv, volume, claim, class); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "pod", namespace, "/api/v1/namespaces/"+namespace+"/pods/fio", created["Pod"]); err != nil {
		return err
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		v, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", text(volume, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if text(v, "metadata", "uid") != text(volume, "metadata", "uid") || text(v, "status", "state") != "detached" {
			return errors.New("scratch backend has not detached")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := removeScratch(ctx, c, source, "persistentvolumeclaim", namespace, "/api/v1/namespaces/"+namespace+"/persistentvolumeclaims/fio-data", claim); err != nil {
		return err
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		current, err := c.get(ctx, "persistentvolume", "", text(pv, "metadata", "name"), false)
		if err != nil {
			return err
		}
		if text(current, "metadata", "uid") != text(pv, "metadata", "uid") || text(current, "status", "phase") != "Released" {
			return errors.New("scratch retained PV has not released")
		}
		if err = verifyScratchBinding(current, volume, claim, class); err != nil {
			return err
		}
		attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
		if err != nil {
			return err
		}
		for _, a := range attachments {
			if text(a, "spec", "source", "persistentVolumeName") == text(pv, "metadata", "name") {
				return errors.New("scratch PV still has an attachment or pending consumer")
			}
		}
		engines, err := c.list(ctx, "engines.longhorn.io", "longhorn-system")
		if err != nil {
			return err
		}
		for _, e := range engines {
			if text(e, "spec", "volumeName") == text(volume, "metadata", "name") && text(e, "status", "currentState") != "stopped" {
				return errors.New("scratch volume engine is still active")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if record.Phase == "v1" {
		if err := retireBenchmarkV1PV(ctx, c, record, volume, claim, class); err != nil {
			return err
		}
		if err := retireBenchmarkV1Backend(ctx, c, record, v1Runtime); err != nil {
			return err
		}
	} else {
		if err := removeScratch(ctx, c, source, "persistentvolume", "", "/api/v1/persistentvolumes/"+text(pv, "metadata", "name"), pv); err != nil {
			return err
		}
		if err := removeScratch(ctx, c, source, "volumes.longhorn.io", "longhorn-system", "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/"+text(volume, "metadata", "name"), volume); err != nil {
			return err
		}
	}
	if err := waitReady(ctx, func(ctx context.Context) error {
		replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
		if err != nil {
			return err
		}
		for _, r := range replicas {
			if text(r, "spec", "volumeName") == text(volume, "metadata", "name") {
				return errors.New("scratch backend replicas remain")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	claims, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return err
	}
	for _, remaining := range claims {
		if text(remaining, "spec", "storageClassName") == text(class, "metadata", "name") || text(remaining, "metadata", "namespace") == namespace {
			return errors.New("scratch class or namespace has another claim")
		}
	}
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return err
	}
	for _, remaining := range pvs {
		if text(remaining, "spec", "storageClassName") == text(class, "metadata", "name") {
			return errors.New("scratch class still has a persistent volume")
		}
	}
	if err = removeScratch(ctx, c, source, "storageclass", "", "/apis/storage.k8s.io/v1/storageclasses/"+text(class, "metadata", "name"), class); err != nil {
		return err
	}
	if err = removeScratch(ctx, c, source, "networkpolicy", namespace, "/apis/networking.k8s.io/v1/namespaces/"+namespace+"/networkpolicies/fio-default-deny", created["NetworkPolicy"]); err != nil {
		return err
	}
	if record.Phase == "v1" {
		if err := removeCompletedBenchmarkFences(ctx, c, *record); err != nil {
			return err
		}
	}
	_, err = observeBenchmarkWorkspace(ctx, c, record.NamespaceUID)
	return err
}

func retireBenchmarkV1PV(ctx context.Context, c client, record *benchmarkRecord, volume, claim, class object) error {
	policies, err := benchmarkRetirementFences(*record)
	if err != nil {
		return err
	}
	identity := record.Resources["PersistentVolume"]
	pv, err := c.get(ctx, "persistentvolume", "", identity.Name, false)
	if err != nil || benchmarkIdentity(pv) != identity || text(pv, "status", "phase") != "Released" ||
		value(pv, "metadata", "deletionTimestamp") != nil || verifyRecoveredBenchmarkStorage(*record, claim, class, pv, volume) != nil {
		return errors.New("original retained benchmark PV is not released and verified")
	}
	if err := installBenchmarkReferenceFence(ctx, c, record, policies[0]); err != nil {
		return err
	}
	if err := proveBenchmarkReferenceFenceOnServingAPIs(ctx, c, *record, pv); err != nil {
		return err
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == identity.Name {
			return errors.New("benchmark PV still has a Kubernetes attachment request")
		}
	}
	// Keep the resource version that passed both native dry-runs. A newer
	// object must pass those same gates before it can be deleted.
	if err := c.deleteObserved(ctx, record.Source, "/api/v1/persistentvolumes/"+identity.Name, pv); err != nil {
		return err
	}
	return waitReady(ctx, func(ctx context.Context) error {
		current, err := c.get(ctx, "persistentvolume", "", identity.Name, true)
		if err != nil {
			return err
		}
		if current != nil {
			return errors.New("original retained benchmark PV is still present")
		}
		return nil
	})
}
