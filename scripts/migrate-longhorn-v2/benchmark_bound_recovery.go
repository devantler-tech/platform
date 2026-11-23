package main

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
)

const recoverBoundV1Operation = "recover-bound-v1-benchmark"
const recoverBoundV1Confirmation = "recover-bound-longhorn-v1-benchmark:"

func authorizeBoundV1Recovery(getenv func(string) string) error {
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	original, prefixed := strings.CutPrefix(confirmation, recoverBoundV1Confirmation)
	if getenv("LONGHORN_V2_OPERATION") != recoverBoundV1Operation ||
		getenv("LONGHORN_V2_NODE") != "" || getenv("LONGHORN_V2_DATABASE") != "" ||
		!prefixed || !validBenchmarkOwner(original, getenv("GITHUB_SHA")) || original == getenv("GITHUB_RUN_ID") {
		return errors.New("the stopped bound V1 run must be explicitly confirmed")
	}
	return authorizeWorkflow(getenv, confirmation)
}

func verifyBoundRecoveryRecord(record benchmarkRecord, original string) error {
	claim := record.Resources["PersistentVolumeClaim"]
	handle := "pvc-" + claim.UID
	if record.Version != 3 || record.Phase != "v1" || record.Run != original ||
		record.CompletedAt != "" || len(record.Measurements) != 0 || len(record.TagNodes) != 3 ||
		record.CPU.ClientRequest != "1" || record.CPU.ClientLimit != "2" ||
		verifyRetirementReceipts(record) != nil || validateBenchmarkRuntimeReceipt(record) != nil ||
		!benchmarkDynamicVolume.MatchString(handle) || record.Resources["PersistentVolume"].Name != handle ||
		record.Resources["LonghornVolume"].Name != handle {
		return errors.New("exact bound V1 scratch receipts from the unmeasured run are required")
	}
	if record.Recovery == nil && (len(record.Fences) != 0 || record.FencePending != "" || record.FenceCheckpoint != "") {
		return errors.New("the original workflow started retirement; preserve ambiguous fence ownership")
	}
	return nil
}

type boundBenchmarkAsset struct {
	kind, resource, path string
}

func boundBenchmarkAssets(record benchmarkRecord) []boundBenchmarkAsset {
	ns := record.Namespace
	return []boundBenchmarkAsset{
		{"Pod", "pod", "/api/v1/namespaces/" + ns + "/pods/fio"},
		{"PersistentVolumeClaim", "persistentvolumeclaim", "/api/v1/namespaces/" + ns + "/persistentvolumeclaims/fio-data"},
		{"PersistentVolume", "persistentvolume", "/api/v1/persistentvolumes/" + record.Resources["PersistentVolume"].Name},
		{"LonghornVolume", "volumes.longhorn.io", "/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes/" + record.Resources["LonghornVolume"].Name},
		{"StorageClass", "storageclass", "/apis/storage.k8s.io/v1/storageclasses/" + record.Resources["StorageClass"].Name},
		{"NetworkPolicy", "networkpolicy", "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies/fio-default-deny"},
	}
}

func observeBoundBenchmarkAssets(ctx context.Context, c client, record benchmarkRecord) (map[string]object, error) {
	if _, err := observeBenchmarkWorkspace(ctx, c, record.NamespaceUID); err != nil {
		return nil, err
	}
	observed := map[string]object{}
	for _, asset := range boundBenchmarkAssets(record) {
		identity := record.Resources[asset.kind]
		current, err := c.get(ctx, asset.resource, identity.Namespace, identity.Name, true)
		if err != nil {
			return nil, err
		}
		intended := record.Recovery != nil && record.Recovery.Deletes[asset.kind] == identity
		if current == nil && !intended || current != nil && benchmarkIdentity(current) != identity ||
			value(current, "metadata", "deletionTimestamp") != nil && !intended {
			return nil, errors.New("bound scratch assets disappeared or changed without their original deletion intent")
		}
		switch asset.kind {
		case "Pod", "PersistentVolumeClaim", "StorageClass", "NetworkPolicy":
			if current != nil && text(current, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != record.Run {
				return nil, errors.New("bound scratch resource no longer has its original run label")
			}
		}
		observed[asset.kind] = current
	}
	pod, claim, class := observed["Pod"], observed["PersistentVolumeClaim"], observed["StorageClass"]
	pv, backend := observed["PersistentVolume"], observed["LonghornVolume"]
	if pod != nil && (text(pod, "status", "phase") != "Failed" || verifyBenchmarkPod(pod, record.Resources["Pod"].UID, record.clientNode(), record.Image) != nil) {
		return nil, errors.New("the recorded fio Pod is not the failed original consumer")
	}
	if claim != nil && (text(claim, "status", "phase") != "Bound" || text(claim, "spec", "volumeName") != record.Resources["PersistentVolume"].Name ||
		text(claim, "spec", "storageClassName") != record.Resources["StorageClass"].Name) {
		return nil, errors.New("the original scratch claim binding changed")
	}
	if backend != nil {
		if pv != nil && value(pv, "metadata", "deletionTimestamp") == nil &&
			verifyRecoveredBenchmarkStorage(record, boundBenchmarkClaim(record, claim), class, pv, backend) != nil {
			return nil, errors.New("the original retained scratch binding changed")
		}
		if text(backend, "spec", "dataEngine") != "v1" || text(backend, "spec", "accessMode") != "rwo" ||
			!absentOrEmptyString(value(backend, "spec", "fromBackup")) || !absentOrEmptyString(value(backend, "spec", "dataSource")) {
			return nil, errors.New("the original scratch backend is outside the bound V1 recovery scope")
		}
	}
	if err := verifyBoundBenchmarkConsumers(ctx, c, record); err != nil {
		return nil, err
	}
	return observed, nil
}

// Once the original claim has been deleted with an exact intent, the retained
// PV still carries its claim reference. Recheck that reference against the journal.
func boundBenchmarkClaim(record benchmarkRecord, claim object) object {
	if claim != nil {
		return claim
	}
	identity := record.Resources["PersistentVolumeClaim"]
	return object{"metadata": object{"name": identity.Name, "namespace": identity.Namespace, "uid": identity.UID},
		"spec": object{"volumeName": record.Resources["PersistentVolume"].Name}}
}

func verifyBoundBenchmarkConsumers(ctx context.Context, c client, record benchmarkRecord) error {
	pods, err := c.list(ctx, "pods", record.Namespace)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if benchmarkIdentity(pod) != record.Resources["Pod"] || text(pod, "status", "phase") != "Failed" {
			return errors.New("another consumer or a nonterminal Pod exists in the scratch namespace")
		}
	}
	claim := record.Resources["PersistentVolumeClaim"]
	class := record.Resources["StorageClass"].Name
	claims, err := c.list(ctx, "persistentvolumeclaims", "*")
	if err != nil {
		return err
	}
	for _, current := range claims {
		if (text(current, "metadata", "namespace") == record.Namespace || text(current, "spec", "storageClassName") == class ||
			text(current, "spec", "volumeName") == record.Resources["PersistentVolume"].Name) && benchmarkIdentity(current) != claim {
			return errors.New("another claim references the scratch volume, class or namespace")
		}
	}
	pvs, err := c.list(ctx, "persistentvolumes", "")
	if err != nil {
		return err
	}
	for _, pv := range pvs {
		if (text(pv, "spec", "storageClassName") == class || text(pv, "spec", "csi", "volumeHandle") == record.Resources["LonghornVolume"].Name ||
			text(pv, "spec", "claimRef", "uid") == claim.UID ||
			(text(pv, "spec", "claimRef", "namespace") == claim.Namespace && text(pv, "spec", "claimRef", "name") == claim.Name)) &&
			benchmarkIdentity(pv) != record.Resources["PersistentVolume"] {
			return errors.New("another persistent volume references the scratch backend, class or claim")
		}
	}
	return nil
}

func recoverBoundV1Benchmark(ctx context.Context, c client, source, run, original string) error {
	_, record, err := readBenchmark(ctx, c, "v1")
	if err != nil {
		return err
	}
	if err := verifyBoundRecoveryRecord(record, original); err != nil {
		return err
	}
	if _, err := observeBoundBenchmarkAssets(ctx, c, record); err != nil {
		return err
	}
	record, err = claimBenchmarkRecovery(ctx, c, "v1", source, run)
	if err != nil {
		return err
	}
	if err := verifyBoundRecoveryRecord(record, original); err != nil {
		return err
	}
	if !activeBenchmarkV1FenceOwner(c, record) {
		return errors.New("bound scratch recovery does not own the confirmed workflow")
	}
	if err := verifyOriginalBenchmarkV1Runtime(ctx, c, record); err != nil {
		return err
	}
	if err := archiveBenchmarkRecovery(ctx, c, record, "bound"); err != nil {
		return err
	}
	for _, asset := range boundBenchmarkAssets(record) {
		observed, err := observeBoundBenchmarkAssets(ctx, c, record)
		if err != nil {
			return err
		}
		current := observed[asset.kind]
		if current == nil {
			continue
		}
		if err := retireBoundBenchmarkAsset(ctx, c, &record, asset, observed); err != nil {
			return err
		}
	}
	remaining, err := observeBoundBenchmarkAssets(ctx, c, record)
	if err != nil {
		return err
	}
	for _, asset := range remaining {
		if asset != nil {
			return errors.New("an original scratch asset still exists")
		}
	}
	if err := removeCompletedBenchmarkFences(ctx, c, record); err != nil {
		return err
	}
	if err := removeRecoveryBenchmarkTags(ctx, c, record); err != nil {
		return err
	}
	if err := archiveBenchmarkRecovery(ctx, c, record, "bound"); err != nil {
		return err
	}
	return removeArchivedBenchmarkJournal(ctx, c, record)
}

func retireBoundBenchmarkAsset(ctx context.Context, c client, record *benchmarkRecord, asset boundBenchmarkAsset, observed map[string]object) error {
	current := observed[asset.kind]
	identity := record.Resources[asset.kind]
	if value(current, "metadata", "deletionTimestamp") != nil {
		return waitRecoveryAssetRemoved(ctx, c, asset, identity)
	}
	switch asset.kind {
	case "PersistentVolumeClaim":
		if err := waitReady(ctx, func(ctx context.Context) error {
			if err := verifyBoundBenchmarkConsumers(ctx, c, *record); err != nil {
				return err
			}
			volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", record.Resources["LonghornVolume"].Name, false)
			if err != nil || benchmarkIdentity(volume) != record.Resources["LonghornVolume"] {
				return errors.New("bound backend identity is unverified")
			}
			attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
			if err != nil {
				return err
			}
			for _, a := range attachments {
				if text(a, "spec", "source", "persistentVolumeName") == record.Resources["PersistentVolume"].Name {
					return errors.New("benchmark scratch still has a Kubernetes attachment")
				}
			}
			return verifyBenchmarkDetachedBackendState(volume)
		}); err != nil {
			return err
		}
	case "PersistentVolume":
		// The claim DELETE is foreground. The PV release controller can still
		// be catching up after its claim is gone.
		if err := waitReady(ctx, func(ctx context.Context) error {
			pv, err := c.get(ctx, "persistentvolume", "", identity.Name, false)
			if err != nil || benchmarkIdentity(pv) != identity || text(pv, "status", "phase") != "Released" {
				return errors.New("original retained benchmark PV is not released")
			}
			return nil
		}); err != nil {
			return err
		}
		return retireBenchmarkV1PV(ctx, c, record, observed["LonghornVolume"], boundBenchmarkClaim(*record, nil), observed["StorageClass"])
	case "LonghornVolume":
		return retireBenchmarkV1Backend(ctx, c, record)
	}
	if err := recordBenchmarkRecoveryDeletion(ctx, c, record, asset.kind); err != nil {
		return err
	}
	if err := c.deleteObserved(ctx, record.Recovery.Source, asset.path, current); err != nil {
		return err
	}
	return waitRecoveryAssetRemoved(ctx, c, asset, identity)
}

func waitRecoveryAssetRemoved(ctx context.Context, c client, asset boundBenchmarkAsset, identity benchmarkResource) error {
	return waitReady(ctx, func(ctx context.Context) error {
		current, err := c.get(ctx, asset.resource, identity.Namespace, identity.Name, true)
		if err != nil || current == nil {
			return err
		}
		if benchmarkIdentity(current) != identity {
			return errReadinessTerminal
		}
		return errors.New("original scratch asset removal is pending")
	})
}

func removeRecoveryBenchmarkTags(ctx context.Context, c client, record benchmarkRecord) error {
	tag := benchmarkTag(record.Phase, record.Run)
	for _, name := range slices.Sorted(maps.Keys(record.TagNodes)) {
		node, err := c.get(ctx, "nodes.longhorn.io", "longhorn-system", name, false)
		if err != nil || text(node, "metadata", "uid") != record.TagNodes[name] {
			return errors.New("scratch scheduling tag owner changed")
		}
		tags, _ := value(node, "spec", "tags").([]any)
		if text(node, "metadata", "annotations", benchmarkOwner) == "" && !slices.Contains(tags, any(tag)) {
			continue
		}
		if err := c.setBenchmarkTag(ctx, record.Recovery.Source, tag, name, record.TagNodes[name], false); err != nil {
			return err
		}
	}
	return nil
}
