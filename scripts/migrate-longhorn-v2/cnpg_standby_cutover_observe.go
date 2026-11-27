package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
)

const umamiStandbyObservationOperation = "observe-umami-v2-standby"

func authorizeUmamiStandbyObservation(getenv func(string) string) (string, string, error) {
	const database = "umami-db"
	if getenv("LONGHORN_V2_OPERATION") != umamiStandbyObservationOperation || getenv("LONGHORN_V2_DATABASE") != database ||
		getenv("LONGHORN_V2_NODE") != "" {
		return "", "", errors.New("the exact Umami standby observation is required")
	}
	confirmation := getenv("LONGHORN_V2_CONFIRM")
	items := strings.Split(confirmation, ":")
	if len(items) != 3 || items[0] != umamiStandbyObservationOperation || items[1] == items[2] {
		return "", "", errors.New("the original and spare standby must be specified and distinct")
	}
	for _, name := range items[1:] {
		if _, err := cnpgInstanceID(name, database); err != nil {
			return "", "", err
		}
	}
	if err := authorizeWorkflow(getenv, confirmation); err != nil {
		return "", "", err
	}
	return items[1], items[2], nil
}

func observeUmamiStandbyCutover(ctx context.Context, c client, source, target, spare string) (cnpgStandbyCutover, error) {
	const name, namespace = "umami-db", "umami"
	if _, err := cnpgInstanceID(target, name); err != nil {
		return cnpgStandbyCutover{}, err
	}
	if _, err := cnpgInstanceID(spare, name); err != nil || spare == target {
		return cnpgStandbyCutover{}, errors.New("the selected standby and spare must be distinct Umami instances")
	}
	before, err := observeCNPGMigration(ctx, c, name, source)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	archive, err := rebindUmamiArchiveProof(ctx, c, before)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	activation := temporaryV2Activation{c: c, source: source, owner: os.Getenv("GITHUB_RUN_ID"), pools: c.inventoryPools}
	qualified, err := activation.recoveryStorage(ctx, archive.Plan.Source)
	if err != nil || !recoveryJSONEqual(qualified, *archive.Storage) {
		return cnpgStandbyCutover{}, errors.New("the raw storage qualified during backup recovery is unverified")
	}
	spareBefore, err := readUmamiV2Standby(ctx, c, before, spare, qualified.Disks)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", namespace, target, false)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	pvName := "pvc-" + text(claim, "metadata", "uid")
	if !nativeRestoreUID(text(claim, "metadata", "uid")) || text(claim, "spec", "volumeName") != pvName {
		return cnpgStandbyCutover{}, errors.New("the selected standby has no original bound storage identity")
	}
	pv, err := c.get(ctx, "persistentvolume", "", pvName, false)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", pvName, false)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	budget, err := c.get(ctx, "poddisruptionbudget", namespace, name, false)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	pods, err := c.list(ctx, "pods", namespace)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	after, err := observeCNPGMigration(ctx, c, name, source)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	if err := validateCNPGObservation(before, after); err != nil {
		return cnpgStandbyCutover{}, err
	}
	spareAfter, err := readUmamiV2Standby(ctx, c, after, spare, qualified.Disks)
	if err != nil || !reflect.DeepEqual(spareBefore, spareAfter) {
		return cnpgStandbyCutover{}, errors.New("the qualified V2 spare changed during observation")
	}
	result, err := bindUmamiStandbyCutover(after, pods, target, claim, pv, volume, budget)
	if err != nil {
		return cnpgStandbyCutover{}, err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return cnpgStandbyCutover{}, err
	}
	result.Spare = spareAfter
	return result, nil
}

func rebindUmamiArchiveProof(ctx context.Context, c client, observation cnpgObservation) (cnpgProofRecord, error) {
	refused := errors.New("the completed V2 backup recovery for this Umami database is unverified")
	_, proof, err := readCNPGProof(ctx, c, "umami-db")
	primary := observation.States[text(observation.Cluster, "status", "currentPrimary")]
	if err != nil || proof.Phase != cnpgArchiveCredentialRetired || !proof.RestoreVerified || validateCNPGRestoreReceipt(proof) != nil ||
		proof.Plan.Source.ClusterUID != text(observation.Cluster, "metadata", "uid") ||
		proof.Plan.Barrier.SystemID != primary.SystemID {
		return cnpgProofRecord{}, refused
	}
	run, source := proof.Run, proof.Source
	if proof.Executor != nil {
		run, source = proof.Executor.Run, proof.Executor.Source
	}
	result, err := readFinishedMigrationRun(ctx, c, run, source)
	if err != nil || result.Attempt != 1 || result.Conclusion != "success" {
		return cnpgProofRecord{}, refused
	}
	// Independently verify that the same restored V2 copy still exists. A
	// completed run alone cannot prove that backup retention still applies.
	if _, err := readRecoveredStorageSnapshot(ctx, c, proof); err != nil {
		return cnpgProofRecord{}, errors.New("the verified retained Umami archive copy is no longer proven")
	}
	return proof, nil
}
