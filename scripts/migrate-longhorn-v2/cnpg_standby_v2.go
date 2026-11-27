package main

import (
	"context"
	"errors"
)

func readUmamiV2Standby(ctx context.Context, c client, observation cnpgObservation, name string, disks []cnpgRecoveryDiskBinding) (cnpgRecoveredStorage, error) {
	refused := errors.New("the live V2 standby storage inventory is incomplete")
	claim, err := c.get(ctx, "persistentvolumeclaim", "umami", name, false)
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
	return bindUmamiV2Standby(observation, claim, pv, volume, engine, replicas, disks)
}

func bindUmamiV2Standby(observation cnpgObservation, claim, pv, volume, engine object, replicas []object, disks []cnpgRecoveryDiskBinding) (cnpgRecoveredStorage, error) {
	const size, bytes = "5Gi", "5368709120"
	cluster := observation.Cluster
	name := text(claim, "metadata", "name")
	primary := text(cluster, "status", "currentPrimary")
	refused := errors.New("the live V2 standby or physical replay is unverified")
	if !umamiV2Transition(cluster) || name == primary || len(disks) != 3 ||
		verifyPostgresStandby(observation.States[primary], observation.States[name]) != nil {
		return cnpgRecoveredStorage{}, refused
	}
	owned, err := healthyCNPGPods(cluster, observation.Pods)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	pods, err := indexed(owned)
	if err != nil {
		return cnpgRecoveredStorage{}, refused
	}
	pod := pods[name]
	if !recoveredNativeObject(pod, "v1", "Pod", "umami") || verifyPostgresPod(pod, pod, cluster) != nil || !recoveredPostgresMount(pod, claim) {
		return cnpgRecoveredStorage{}, refused
	}
	if !recoveredNativeObject(claim, "v1", "PersistentVolumeClaim", "umami") || !cnpgOwned(claim, cluster) ||
		text(claim, "status", "phase") != "Bound" || text(claim, "spec", "storageClassName") != "longhorn-v2" ||
		text(claim, "spec", "resources", "requests", "storage") != size || text(claim, "status", "capacity", "storage") != size ||
		!providerRWOFilesystem(claim) {
		return cnpgRecoveredStorage{}, refused
	}
	handle := "pvc-" + text(claim, "metadata", "uid")
	hostname := text(pod, "spec", "nodeName")
	if hostname == "" || !recoveredPVBackend(claim, pv, volume, engine, "umami", handle, size, bytes, hostname) {
		return cnpgRecoveredStorage{}, refused
	}
	copies, err := recoveredNativeCopies(disks, volume, engine, replicas, bytes)
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
