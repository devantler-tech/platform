package main

import (
	"errors"
	"reflect"
	"slices"
)

type cnpgStandbyCutover struct {
	Database, Consumer, Claim, PV, Volume, Budget benchmarkResource
	ConsumerVersion, BudgetVersion                string
	Spare                                         cnpgRecoveredStorage
}

// Bind the current database and its sole volume consumer before recording a
// replacement. The original PV and its V1 backend remain reserved to this
// claim while CloudNativePG provisions a replacement from its V2 template.
func bindUmamiStandbyCutover(observation cnpgObservation, pods []object, target string, claim, pv, volume, budget object) (cnpgStandbyCutover, error) {
	if !umamiV2Transition(observation.Cluster) {
		return cnpgStandbyCutover{}, errors.New("the Umami storage transition is unverified")
	}
	return bindCNPGStandbyCutover(observation, pods, target, claim, pv, volume, budget)
}

func bindCNPGStandbyCutover(observation cnpgObservation, pods []object, target string, claim, pv, volume, budget object) (cnpgStandbyCutover, error) {
	cluster := observation.Cluster
	profile, admitted := cnpgStandbyTransition(cluster)
	primary := text(cluster, "status", "currentPrimary")
	unqualified := errors.New("the selected database standby or retained storage binding is unverified")
	if !admitted || target == primary || len(observation.Pods) != profile.Instances || len(observation.States) != profile.Instances ||
		verifyPostgresStandby(observation.States[primary], observation.States[target]) != nil {
		return cnpgStandbyCutover{}, unqualified
	}
	byName, err := indexed(observation.Pods)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	pod := byName[target]
	if verifyPostgresPod(pod, pod, cluster) != nil || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" ||
		!recoveredPostgresMount(pod, claim) || !onlyCNPGStandbyUsesClaim(pods, pod, target, profile.Source.Namespace) ||
		!cnpgTransitionDisruptionAllowed(budget, cluster, profile) || !cnpgV1StorageRetained(profile, cluster, pod, claim, pv, volume) {
		return cnpgStandbyCutover{}, unqualified
	}
	return cnpgStandbyCutover{Database: benchmarkIdentity(cluster), Consumer: benchmarkIdentity(pod),
		ConsumerVersion: text(pod, "metadata", "resourceVersion"), Claim: benchmarkIdentity(claim),
		PV: benchmarkIdentity(pv), Volume: benchmarkIdentity(volume), Budget: benchmarkIdentity(budget),
		BudgetVersion: text(budget, "metadata", "resourceVersion")}, nil
}

func onlyCNPGStandbyUsesClaim(pods []object, pod object, claim, namespace string) bool {
	seen := 0
	for _, other := range pods {
		if text(other, "metadata", "namespace") != namespace {
			continue
		}
		specVolumes, _ := value(other, "spec", "volumes").([]any)
		for _, raw := range specVolumes {
			volume, _ := raw.(object)
			if text(volume, "persistentVolumeClaim", "claimName") == claim {
				if benchmarkIdentity(other) != benchmarkIdentity(pod) || value(other, "metadata", "deletionTimestamp") != nil {
					return false
				}
				seen++
			}
		}
	}
	return seen == 1
}

func cnpgTransitionDisruptionAllowed(budget, cluster object, profile cnpgStandbyProfile) bool {
	labels := object{"cnpg.io/cluster": profile.Source.Name, "cnpg.io/instanceRole": "replica"}
	generation, ok := nativeObservationUint(value(budget, "metadata", "generation"))
	minimum, followers := float64(profile.Instances-2), float64(profile.Instances-1)
	return recoveredNativeObject(budget, "policy/v1", "PodDisruptionBudget", profile.Source.Namespace) &&
		text(budget, "metadata", "name") == profile.Source.Name && cnpgOwnerReference(budget, cluster) && ok && generation > 0 &&
		reflect.DeepEqual(valueObject(budget, "spec", "selector"), object{"matchLabels": labels}) &&
		value(budget, "spec", "minAvailable") == minimum && value(budget, "spec", "maxUnavailable") == nil &&
		value(budget, "metadata", "generation") == value(budget, "status", "observedGeneration") &&
		value(budget, "status", "currentHealthy") == followers && value(budget, "status", "expectedPods") == followers &&
		value(budget, "status", "desiredHealthy") == minimum && value(budget, "status", "disruptionsAllowed") == float64(1) &&
		len(valueObject(budget, "status", "disruptedPods")) == 0
}

func umamiV1StorageRetained(cluster, pod, claim, pv, volume object) bool {
	profile, ok := cnpgStandbyTransition(cluster)
	return ok && profile.Source == (cnpgSource{Namespace: "umami", Name: "umami-db"}) && cnpgV1StorageRetained(profile, cluster, pod, claim, pv, volume)
}

func cnpgV1StorageRetained(profile cnpgStandbyProfile, cluster, pod, claim, pv, volume object) bool {
	return cnpgOwned(claim, cluster) && text(claim, "metadata", "name") == text(pod, "metadata", "name") &&
		verifyOriginalV1ClaimBinding(profile, claim, pv, volume) && umamiOriginalVolumeAttached(pod, volume)
}

func umamiOriginalVolumeAttached(pod, volume object) bool {
	node := text(pod, "spec", "nodeName")
	return node != "" && text(volume, "status", "currentNodeID") == node &&
		text(volume, "status", "state") == "attached" && text(volume, "status", "robustness") == "healthy"
}

func verifyOriginalV1ClaimBinding(profile cnpgStandbyProfile, claim, pv, volume object) bool {
	const class = "longhorn-wffc"
	namespace, size := profile.Source.Namespace, profile.Size
	name := text(claim, "metadata", "name")
	uid := text(claim, "metadata", "uid")
	backend := "pvc-" + uid
	finalizers, _ := value(claim, "metadata", "finalizers").([]any)
	return recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", namespace) &&
		slices.Contains(finalizers, "kubernetes.io/pvc-protection") &&
		text(claim, "status", "phase") == "Bound" &&
		text(claim, "spec", "storageClassName") == class && text(claim, "spec", "volumeName") == backend &&
		text(claim, "spec", "resources", "requests", "storage") == size && text(claim, "status", "capacity", "storage") == size &&
		providerRWOFilesystem(claim) &&
		recoveredNativeObject(pv, "v1", "PersistentVolume", "") && providerRWOFilesystem(pv) &&
		text(pv, "metadata", "name") == backend && text(pv, "status", "phase") == "Bound" &&
		text(pv, "spec", "persistentVolumeReclaimPolicy") == "Retain" && text(pv, "spec", "storageClassName") == class &&
		text(pv, "spec", "capacity", "storage") == size && text(pv, "spec", "claimRef", "uid") == uid &&
		text(pv, "spec", "claimRef", "name") == name && text(pv, "spec", "claimRef", "namespace") == namespace &&
		text(pv, "spec", "csi", "driver") == "driver.longhorn.io" && text(pv, "spec", "csi", "volumeHandle") == backend &&
		recoveredNativeObject(volume, "longhorn.io/v1beta2", "Volume", "longhorn-system") &&
		text(volume, "metadata", "name") == backend && text(volume, "spec", "dataEngine") == "v1" &&
		text(volume, "spec", "size") == profile.SizeBytes && value(volume, "spec", "numberOfReplicas") == float64(3) &&
		text(volume, "status", "kubernetesStatus", "pvName") == backend &&
		text(volume, "status", "kubernetesStatus", "pvcName") == name && text(volume, "status", "kubernetesStatus", "namespace") == namespace
}
