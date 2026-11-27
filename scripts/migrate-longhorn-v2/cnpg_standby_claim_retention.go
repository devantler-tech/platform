package main

import (
	"errors"
	"maps"
	"reflect"
)

// umamiStandbyClaimRetentionChanges prepares the metadata change used by
// CloudNativePG's keep-PVC operation. The caller must apply it through the
// existing UID and resource-version patch fence, then verify the retained
// claim before asking the eviction API to stop the original consumer.
func umamiStandbyClaimRetentionChanges(witness cnpgStandbyCutover, cluster, pod, claim, pv, volume object) ([]object, error) {
	const (
		statusAnnotation = "cnpg.io/pvcStatus"
		statusPath       = "/metadata/annotations/cnpg.io~1pvcStatus"
		instanceLabel    = "cnpg.io/instanceName"
	)
	name := witness.Consumer.Name
	owners, _ := value(claim, "metadata", "ownerReferences").([]any)
	profile, admitted := cnpgStandbyTransition(cluster)
	if !admitted || benchmarkIdentity(cluster) != witness.Database ||
		text(cluster, "status", "currentPrimary") == name || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" ||
		benchmarkIdentity(pod) != witness.Consumer || text(pod, "metadata", "resourceVersion") != witness.ConsumerVersion ||
		benchmarkIdentity(claim) != witness.Claim || benchmarkIdentity(pv) != witness.PV ||
		benchmarkIdentity(volume) != witness.Volume || !cnpgV1StorageRetained(profile, cluster, pod, claim, pv, volume) ||
		len(owners) != 1 || text(claim, "metadata", "annotations", statusAnnotation) != "ready" ||
		text(claim, "metadata", "labels", instanceLabel) != name {
		return nil, errors.New("the original standby claim changed before retention")
	}
	return []object{
		{"op": "test", "path": "/metadata/ownerReferences", "value": owners},
		{"op": "test", "path": statusPath, "value": "ready"},
		{"op": "replace", "path": statusPath, "value": "detached"},
		{"op": "remove", "path": "/metadata/ownerReferences/0"},
	}, nil
}

func verifyUmamiStandbyClaimRetention(original, retained object) error {
	wantAnnotations := maps.Clone(valueObject(original, "metadata", "annotations"))
	if wantAnnotations["cnpg.io/pvcStatus"] != "ready" {
		return errors.New("original standby claim readiness is unverified")
	}
	wantAnnotations["cnpg.io/pvcStatus"] = "detached"
	owners := value(retained, "metadata", "ownerReferences")
	if owners != nil && !reflect.DeepEqual(owners, []any{}) {
		return errors.New("retained standby claim still has an owner")
	}
	if !recoveredNativeObject(retained, "v1", "PersistentVolumeClaim", text(original, "metadata", "namespace")) ||
		benchmarkIdentity(retained) != benchmarkIdentity(original) ||
		text(retained, "metadata", "resourceVersion") == text(original, "metadata", "resourceVersion") ||
		!reflect.DeepEqual(wantAnnotations, value(retained, "metadata", "annotations")) ||
		!reflect.DeepEqual(value(retained, "metadata", "labels"), value(original, "metadata", "labels")) ||
		!reflect.DeepEqual(value(retained, "metadata", "finalizers"), value(original, "metadata", "finalizers")) ||
		!reflect.DeepEqual(value(retained, "spec"), value(original, "spec")) ||
		text(retained, "status", "phase") != "Bound" {
		return errors.New("the retained standby claim could not be verified")
	}
	return nil
}

func umamiRetainedClaimMetadata(claim object, name string) bool {
	owners := value(claim, "metadata", "ownerReferences")
	return text(claim, "metadata", "labels", "cnpg.io/instanceName") == name &&
		text(claim, "metadata", "annotations", "cnpg.io/pvcStatus") == "detached" &&
		(owners == nil || reflect.DeepEqual(owners, []any{}))
}

// verifyUmamiStandbyDetached accepts the old backend only when its original
// claim still protects it and neither a Pod nor a CSI attachment can use it.
func verifyUmamiStandbyDetached(witness cnpgStandbyCutover, claim, pv, volume object, pods, attachments []object) error {
	refused := errors.New("the retained standby storage is not demonstrably detached")
	name := witness.Consumer.Name
	profile, admitted := cnpgStandbyProfileForSource(cnpgSource{Namespace: witness.Database.Namespace, Name: witness.Database.Name})
	robustness := text(volume, "status", "robustness")
	if !admitted || pods == nil || attachments == nil ||
		benchmarkIdentity(claim) != witness.Claim || benchmarkIdentity(pv) != witness.PV ||
		benchmarkIdentity(volume) != witness.Volume || !verifyOriginalV1ClaimBinding(profile, claim, pv, volume) ||
		!umamiRetainedClaimMetadata(claim, name) ||
		text(volume, "status", "state") != "detached" || text(volume, "status", "currentNodeID") != "" ||
		text(volume, "spec", "nodeID") != "" || (robustness != "unknown" && robustness != "healthy") {
		return refused
	}
	for _, pod := range pods {
		if text(pod, "metadata", "namespace") != witness.Consumer.Namespace {
			return refused
		}
		if text(pod, "metadata", "name") == name {
			return refused
		}
		mounts, _ := value(pod, "spec", "volumes").([]any)
		for _, raw := range mounts {
			mount, _ := raw.(object)
			if text(mount, "persistentVolumeClaim", "claimName") == witness.Claim.Name {
				return refused
			}
		}
	}
	for _, attachment := range attachments {
		if text(attachment, "spec", "source", "persistentVolumeName") == witness.PV.Name {
			return refused
		}
	}
	return nil
}
