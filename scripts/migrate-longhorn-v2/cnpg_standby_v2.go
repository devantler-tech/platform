package main

import (
	"context"
	"errors"
)

func readUmamiV2Standby(ctx context.Context, c client, observation cnpgObservation, name string, disks []cnpgRecoveryDiskBinding) (cnpgRecoveredStorage, error) {
	if !umamiV2Transition(observation.Cluster) {
		return cnpgRecoveredStorage{}, errors.New("the selected Umami transition is unverified")
	}
	return readCNPGV2Standby(ctx, c, observation, name, disks)
}

func readCNPGV2Standby(ctx context.Context, c client, observation cnpgObservation, name string, disks []cnpgRecoveryDiskBinding) (cnpgRecoveredStorage, error) {
	refused := errors.New("the live V2 standby storage inventory is incomplete")
	profile, ok := cnpgStandbyTransition(observation.Cluster)
	if _, err := cnpgInstanceID(name, profile.Source.Name); !ok || err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", profile.Source.Namespace, name, false)
	handle := "pvc-" + text(claim, "metadata", "uid")
	if err != nil || !nativeRestoreUID(text(claim, "metadata", "uid")) || text(claim, "spec", "volumeName") != handle ||
		text(claim, "metadata", "name") != name {
		return cnpgRecoveredStorage{}, refused
	}
	pv, err := c.get(ctx, "persistentvolume", "", handle, false)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", handle, false)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	engines, err := c.list(ctx, "engines.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(engines, "longhorn.io/v1beta2", "Engine", "longhorn-system") {
		return cnpgRecoveredStorage{}, refused
	}
	var engine object
	for _, candidate := range engines {
		if recoveredVolumeAssociated(candidate, volume) {
			if engine != nil {
				return cnpgRecoveredStorage{}, refused
			}
			engine = candidate
		}
	}
	if engine == nil {
		return cnpgRecoveredStorage{}, refused
	}
	replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
	if err != nil || !recoveredStorageInventory(replicas, "longhorn.io/v1beta2", "Replica", "longhorn-system") {
		return cnpgRecoveredStorage{}, refused
	}
	return bindCNPGV2Standby(observation, claim, pv, volume, engine, replicas, disks)
}

func bindCNPGV2Standby(observation cnpgObservation, claim, pv, volume, engine object, replicas []object, disks []cnpgRecoveryDiskBinding) (cnpgRecoveredStorage, error) {
	cluster := observation.Cluster
	profile, admitted := cnpgStandbyTransition(cluster)
	name := text(claim, "metadata", "name")
	primary := text(cluster, "status", "currentPrimary")
	refused := errors.New("the live V2 standby or physical replay is unverified")
	if !admitted || name == primary || len(disks) != 3 ||
		verifyPostgresStandby(observation.States[primary], observation.States[name]) != nil {
		return cnpgRecoveredStorage{}, refused
	}
	owned, err := verifiedUmamiStandbyRoster(observation)
	if err != nil {
		return cnpgRecoveredStorage{}, err
	}
	pods, err := indexed(owned)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	pod := pods[name]
	if !recoveredNativeObject(pod, "v1", "Pod", profile.Source.Namespace) || verifyPostgresPod(pod, pod, cluster) != nil || !recoveredPostgresMount(pod, claim) {
		return cnpgRecoveredStorage{}, refused
	}
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", profile.Source.Namespace) || !cnpgOwned(claim, cluster) ||
		text(claim, "status", "phase") != "Bound" || text(claim, "spec", "storageClassName") != "longhorn-v2" ||
		text(claim, "spec", "resources", "requests", "storage") != profile.Size || text(claim, "status", "capacity", "storage") != profile.Size ||
		!providerRWOFilesystem(claim) {
		return cnpgRecoveredStorage{}, refused
	}
	handle := "pvc-" + text(claim, "metadata", "uid")
	hostname := text(pod, "spec", "nodeName")
	if hostname == "" || !recoveredPVBackend(claim, pv, volume, engine, profile.Source.Namespace, handle, profile.Size, profile.SizeBytes, hostname) {
		return cnpgRecoveredStorage{}, refused
	}
	copies, err := recoveredNativeCopies(disks, volume, engine, replicas, profile.SizeBytes)
	if err != nil {
		return cnpgRecoveredStorage{}, err
	}
	process, err := postgresExecutable(pod)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	return cnpgRecoveredStorage{
		Cluster: benchmarkIdentity(cluster), Pod: benchmarkIdentity(pod), Claim: benchmarkIdentity(claim),
		PV: benchmarkIdentity(pv), Volume: benchmarkIdentity(volume), Engine: benchmarkIdentity(engine),
		Process: process, Producer: recoveredProgramProducer(pod), Replicas: copies,
	}, nil
}

func verifiedUmamiStandbyRoster(observation cnpgObservation) ([]object, error) {
	cluster := observation.Cluster
	if observation.umamiRecoveryPeers == nil {
		return healthyCNPGPodsWithRetention(cluster, observation.Pods, observation.retainedStandby)
	}
	const surviving = 3
	refused := errors.New("the three surviving Umami members do not match the recovery journal")
	primary := text(cluster, "status", "currentPrimary")
	if observation.retainedStandby != "" || !umamiV2Transition(cluster) || len(observation.Pods) != surviving ||
		len(observation.umamiRecoveryPeers) != surviving || value(cluster, "status", "readyInstances") != float64(surviving) ||
		primary == "" || text(cluster, "status", "targetPrimary") != primary || !cnpgCondition(cluster, "ContinuousArchiving") {
		return nil, refused
	}
	pods, err := indexed(observation.Pods)
	if err != nil || pods[primary] == nil {
		return nil, refused
	}
	for name, pod := range pods {
		if benchmarkIdentity(pod) != observation.umamiRecoveryPeers[name] || verifyPostgresPod(pod, pod, cluster) != nil {
			return nil, refused
		}
	}
	return observation.Pods, nil
}
