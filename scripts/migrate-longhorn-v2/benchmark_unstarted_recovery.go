package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"time"
)

const recoverUnstartedV1Operation = "recover-unstarted-v1-benchmark"
const recoverUnstartedV1Confirmation = "recover-unstarted-longhorn-v1-benchmark"

func authorizeUnstartedV1Recovery(getenv func(string) string) error {
	if getenv("LONGHORN_V2_OPERATION") != recoverUnstartedV1Operation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" {
		return errors.New("only the unstarted V1 benchmark recovery is authorized")
	}
	return authorizeWorkflow(getenv, recoverUnstartedV1Confirmation)
}

func verifyUnstartedBenchmark(record benchmarkRecord) error {
	if record.Version != 3 || record.Phase != "v1" || record.Cleaned || record.PendingCreate != "" ||
		len(record.Measurements) != 0 || !reflect.DeepEqual(record.CPU, benchmarkCPU{}) ||
		len(record.Fences) != 0 || record.FencePending != "" || record.FenceCheckpoint != "" ||
		len(record.TagNodes) != 3 ||
		len(record.Resources) != 5 {
		return errors.New("recovery requires an exact benchmark that stopped before provisioning storage")
	}
	for _, kind := range []string{"Namespace", "Pod", "PersistentVolumeClaim", "NetworkPolicy", "StorageClass"} {
		if record.Resources[kind].UID == "" {
			return errors.New("unstarted benchmark original asset identity is incomplete")
		}
	}
	return nil
}

func verifyUnstartedBenchmarkAsset(kind string, asset object, record benchmarkRecord) error {
	if benchmarkIdentity(asset) != record.Resources[kind] ||
		text(asset, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != record.Run ||
		value(asset, "metadata", "deletionTimestamp") != nil {
		return errors.New("scratch asset identity changed")
	}
	switch kind {
	case "Pod":
		if text(asset, "status", "phase") != "Pending" || text(asset, "spec", "nodeName") != "" ||
			text(asset, "status", "startTime") != "" {
			return errors.New("scratch Pod began scheduling or running")
		}
		conditions, _ := value(asset, "status", "conditions").([]any)
		for _, raw := range conditions {
			if condition, ok := raw.(object); ok &&
				text(condition, "type") == "PodScheduled" && text(condition, "status") != "False" {
				return errors.New("scratch Pod scheduling state is unverified")
			}
		}
	case "PersistentVolumeClaim":
		if text(asset, "status", "phase") != "Pending" || text(asset, "spec", "volumeName") != "" ||
			text(asset, "spec", "storageClassName") != record.Resources["StorageClass"].Name ||
			value(asset, "metadata", "annotations", "volume.kubernetes.io/selected-node") != nil {
			return errors.New("scratch claim has a binding or a selected node")
		}
	case "StorageClass":
		if text(asset, "provisioner") != "driver.longhorn.io" ||
			text(asset, "volumeBindingMode") != "WaitForFirstConsumer" ||
			text(asset, "reclaimPolicy") != "Retain" || text(asset, "parameters", "dataEngine") != "v1" {
			return errors.New("scratch StorageClass provisioning boundary changed")
		}
	}
	return nil
}

func verifyUnstartedBenchmarkStorage(ctx context.Context, c client, record benchmarkRecord) error {
	claim := record.Resources["PersistentVolumeClaim"]
	class := record.Resources["StorageClass"]
	currentClass, err := c.get(ctx, "storageclass", "", class.Name, true)
	if err != nil {
		return err
	}
	if currentClass == nil {
		if record.Recovery == nil || record.Recovery.Deletes["StorageClass"] != class {
			return errors.New("original scratch StorageClass disappeared")
		}
	} else if err := verifyUnstartedBenchmarkAsset("StorageClass", currentClass, record); err != nil {
		return err
	}
	pods, err := c.list(ctx, "pods", record.Namespace)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err := verifyUnstartedBenchmarkAsset("Pod", pod, record); err != nil {
			return err
		}
	}
	claims, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return err
	}
	for _, current := range claims {
		if text(current, "spec", "storageClassName") == class.Name {
			if err := verifyUnstartedBenchmarkAsset("PersistentVolumeClaim", current, record); err != nil {
				return err
			}
		}
	}
	volumes, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if text(volume, "spec", "storageClassName") == class.Name ||
			text(volume, "spec", "claimRef", "uid") == claim.UID ||
			(text(volume, "spec", "claimRef", "namespace") == claim.Namespace &&
				text(volume, "spec", "claimRef", "name") == claim.Name) {
			return errors.New("scratch claim has a persistent volume reference")
		}
	}
	handle := "pvc-" + claim.UID
	backends, err := c.list(ctx, "volumes.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, volume := range backends {
		if text(volume, "metadata", "name") == handle ||
			(text(volume, "status", "kubernetesStatus", "namespace") == claim.Namespace &&
				text(volume, "status", "kubernetesStatus", "pvcName") == claim.Name) {
			return errors.New("scratch claim has a Longhorn backing volume")
		}
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	for _, a := range attachments {
		if text(a, "spec", "source", "persistentVolumeName") == handle {
			return errors.New("scratch claim has a Kubernetes attachment")
		}
	}
	attachments, err = c.list(ctx, "volumeattachments.longhorn.io", "longhorn-system")
	if err != nil {
		return err
	}
	for _, a := range attachments {
		if text(a, "spec", "volume") == handle {
			return errors.New("scratch claim has a Longhorn attachment")
		}
	}
	return nil
}

func recoverUnstartedV1Benchmark(ctx context.Context, c client, source, run string) error {
	const phase = "v1"
	_, record, err := readBenchmark(ctx, c, phase)
	if err != nil {
		return err
	}
	if err := verifyUnstartedBenchmark(record); err != nil {
		return err
	}
	if _, err := observeBenchmarkWorkspace(ctx, c, record.NamespaceUID); err != nil {
		return err
	}
	if err := verifyUnstartedBenchmarkStorage(ctx, c, record); err != nil {
		return err
	}
	record, err = claimBenchmarkRecovery(ctx, c, phase, source, run)
	if err != nil {
		return err
	}
	if err := verifyUnstartedBenchmark(record); err != nil {
		return err
	}
	ns := record.Namespace
	for _, asset := range []struct{ kind, resource, namespace, path string }{
		{"Pod", "pod", ns, "/api/v1/namespaces/" + ns + "/pods/fio"},
		{"PersistentVolumeClaim", "persistentvolumeclaim", ns, "/api/v1/namespaces/" + ns + "/persistentvolumeclaims/fio-data"},
		{"NetworkPolicy", "networkpolicy", ns, "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies/fio-default-deny"},
		{"StorageClass", "storageclass", "", "/apis/storage.k8s.io/v1/storageclasses/" + record.Resources["StorageClass"].Name},
	} {
		if err := verifyUnstartedBenchmarkStorage(ctx, c, record); err != nil {
			return err
		}
		identity := record.Resources[asset.kind]
		current, err := c.get(ctx, asset.resource, asset.namespace, identity.Name, true)
		if err != nil {
			return err
		}
		if current == nil {
			if record.Recovery.Deletes[asset.kind] != identity {
				return errors.New("scratch asset disappeared without a recorded recovery deletion")
			}
			continue
		}
		if err := verifyUnstartedBenchmarkAsset(asset.kind, current, record); err != nil {
			return err
		}
		next := record
		recovery := *record.Recovery
		recovery.Deletes = maps.Clone(recovery.Deletes)
		if recovery.Deletes == nil {
			recovery.Deletes = map[string]benchmarkResource{}
		}
		recovery.Deletes[asset.kind] = identity
		next.Recovery = &recovery
		if err := saveBenchmarkRecoveryProgress(ctx, c, record, next); err != nil {
			return err
		}
		record = next
		// In particular, a scheduler's new Pod assignment or PVC volume
		// binding must invalidate the exact resource version we inspected.
		if err := c.deleteObserved(ctx, source, asset.path, current); err != nil {
			return err
		}
		if err := waitReady(ctx, func(ctx context.Context) error {
			o, err := c.get(ctx, asset.resource, asset.namespace, identity.Name, true)
			if err != nil {
				return err
			}
			if o != nil {
				if text(o, "metadata", "uid") != identity.UID {
					return errReadinessTerminal
				}
				return errors.New("scratch asset deletion is still pending")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := verifyUnstartedBenchmarkStorage(ctx, c, record); err != nil {
		return err
	}
	tag := benchmarkTag(phase, record.Run)
	for _, name := range slices.Sorted(maps.Keys(record.TagNodes)) {
		node, err := c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false)
		if err != nil || text(node, "metadata", "uid") != record.TagNodes[name] {
			return errors.New("scratch scheduling tag owner changed")
		}
		tags, _ := value(node, "spec", "tags").([]any)
		if text(node, "metadata", "annotations", benchmarkOwner) == "" && !slices.Contains(tags, any(tag)) {
			continue
		}
		if err := c.setBenchmarkTag(ctx, source, tag, name, record.TagNodes[name], false); err != nil {
			return err
		}
	}
	return sealUnstartedBenchmarkRecovery(ctx, c, record)
}

func sealUnstartedBenchmarkRecovery(ctx context.Context, c client, record benchmarkRecord) error {
	cm, current, err := readBenchmark(ctx, c, "v1")
	if err != nil || cm == nil || text(cm, "metadata", "uid") != record.Recovery.JournalUID ||
		!reflect.DeepEqual(current, record) {
		return errors.New("scratch cleanup journal changed")
	}
	archive := "longhorn-v2-benchmark-v1-unstarted-" + record.Run
	source := record.Recovery.Source
	// Store original evidence with a name bound to the stopped run. Exclude
	// the current recovery owner so an interrupted recovery can verify it.
	original := record
	original.Recovery = nil
	body, err := json.Marshal(original)
	if err != nil {
		return err
	}
	saved, err := c.get(ctx, "configmap", "longhorn-system", archive, true)
	if err != nil {
		return err
	}
	if saved == nil {
		saved, err = c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true,
			"metadata": object{"name": archive, "namespace": "longhorn-system"},
			"data":     object{"record": string(body)}})
	}
	if err != nil || value(saved, "immutable") != true || text(saved, "data", "record") != string(body) {
		return errors.New("original unstarted benchmark archive is unverified")
	}
	if err := benchmarkOwnerFinished(ctx, c, record.Run, record.Source); err != nil {
		return err
	}
	if err := c.deleteObserved(ctx, source, "/api/v1/namespaces/longhorn-system/configmaps/longhorn-v2-benchmark-v1", cm); err != nil {
		return err
	}
	return waitReadyWithin(ctx, 2*time.Minute, func(ctx context.Context) error {
		remaining, _, err := readBenchmark(ctx, c, "v1")
		if err != nil {
			return err
		}
		if remaining == nil {
			return nil
		}
		if text(remaining, "metadata", "uid") != record.Recovery.JournalUID {
			return errReadinessTerminal
		}
		return errors.New("scratch journal removal is still pending")
	})
}
