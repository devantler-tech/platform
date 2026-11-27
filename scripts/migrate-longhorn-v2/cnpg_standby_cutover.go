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
	cluster := observation.Cluster
	ns := text(cluster, "metadata", "namespace")
	primary := text(cluster, "status", "currentPrimary")
	unqualified := errors.New("the selected database standby or retained storage binding is unverified")
	if !umamiV2Transition(cluster) || target == primary || len(observation.Pods) != 4 || len(observation.States) != 4 ||
		verifyPostgresStandby(observation.States[primary], observation.States[target]) != nil {
		return cnpgStandbyCutover{}, unqualified
	}
	byName, err := indexed(observation.Pods)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	pod := byName[target]
	if verifyPostgresPod(pod, pod, cluster) != nil || text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "replica" ||
		!recoveredPostgresMount(pod, claim) || !onlyCNPGStandbyUsesClaim(pods, pod, target, ns) ||
		!umamiStandbyDisruptionAllowed(budget, cluster) || !umamiV1StorageRetained(cluster, pod, claim, pv, volume) {
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

func umamiStandbyDisruptionAllowed(budget, cluster object) bool {
	labels := object{"cnpg.io/cluster": "umami-db", "cnpg.io/instanceRole": "replica"}
	generation, ok := nativeObservationUint(value(budget, "metadata", "generation"))
	return recoveredNativeObject(budget, "policy/v1", "PodDisruptionBudget", "umami") &&
		text(budget, "metadata", "name") == "umami-db" && cnpgOwnerReference(budget, cluster) && ok && generation > 0 &&
		reflect.DeepEqual(valueObject(budget, "spec", "selector"), object{"matchLabels": labels}) &&
		value(budget, "spec", "minAvailable") == float64(2) && value(budget, "spec", "maxUnavailable") == nil &&
		value(budget, "metadata", "generation") == value(budget, "status", "observedGeneration") &&
		value(budget, "status", "currentHealthy") == float64(3) && value(budget, "status", "expectedPods") == float64(3) &&
		value(budget, "status", "desiredHealthy") == float64(2) && value(budget, "status", "disruptionsAllowed") == float64(1) &&
		len(valueObject(budget, "status", "disruptedPods")) == 0
}

func umamiV1StorageRetained(cluster, pod, claim, pv, volume object) bool {
	node := text(pod, "spec", "nodeName")
	return cnpgOwned(claim, cluster) && text(claim, "metadata", "name") == text(pod, "metadata", "name") &&
		verifyUmamiOriginalClaimBinding(claim, pv, volume) &&
		node != "" && text(volume, "status", "currentNodeID") == node && text(volume, "status", "state") == "attached" &&
		text(volume, "status", "robustness") == "healthy"
}

func verifyUmamiOriginalClaimBinding(claim, pv, volume object) bool {
	const namespace, class, size = "umami", "longhorn-wffc", "5Gi"
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
		text(volume, "spec", "size") == "5368709120" && value(volume, "spec", "numberOfReplicas") == float64(3) &&
		text(volume, "status", "kubernetesStatus", "pvName") == backend &&
		text(volume, "status", "kubernetesStatus", "pvcName") == name && text(volume, "status", "kubernetesStatus", "namespace") == namespace
}
