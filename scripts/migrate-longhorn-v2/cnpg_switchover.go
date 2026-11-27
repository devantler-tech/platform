package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"
)

const (
	umamiV2SwitchoverOperation = "switch-umami-primary-to-v2"
	umamiV2SwitchoverJournal   = "longhorn-v2-umami-primary-switchover"
)

type umamiV2Promotion struct {
	Database, Previous   benchmarkResource
	OriginalClaim, OldPV benchmarkResource
	OriginalVolume       benchmarkResource
	Standbys             map[string]cnpgRecoveredStorage
	PreviousState        postgresState
}

func observeUmamiV2Promotion(ctx context.Context, c client, observation cnpgObservation, previous, target string, disks []cnpgRecoveryDiskBinding) (umamiV2Promotion, error) {
	refused := errors.New("the original primary and three V2 standbys are unverified")
	cluster := observation.Cluster
	if !umamiV2Transition(cluster) || len(observation.Pods) != 4 || len(observation.States) != 4 ||
		text(cluster, "status", "currentPrimary") != previous || text(cluster, "status", "targetPrimary") != previous {
		return umamiV2Promotion{}, refused
	}
	pods, err := indexed(observation.Pods)
	if err != nil {
		return umamiV2Promotion{}, refused
	}
	primary := pods[previous]
	claim, err := c.get(ctx, "persistentvolumeclaim", "umami", previous, false)
	backend := "pvc-" + text(claim, "metadata", "uid")
	if err != nil || !nativeRestoreUID(text(claim, "metadata", "uid")) || !recoveredPostgresMount(primary, claim) ||
		text(primary, "metadata", "labels", "cnpg.io/instanceRole") != "primary" {
		return umamiV2Promotion{}, refused
	}
	pv, err := c.get(ctx, "persistentvolume", "", backend, false)
	if err != nil {
		return umamiV2Promotion{}, refused
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", backend, false)
	if err != nil || !umamiV1StorageRetained(cluster, primary, claim, pv, volume) {
		return umamiV2Promotion{}, refused
	}
	witness := umamiV2Promotion{
		Database: benchmarkIdentity(cluster), Previous: benchmarkIdentity(primary),
		OriginalClaim: benchmarkIdentity(claim), OldPV: benchmarkIdentity(pv), OriginalVolume: benchmarkIdentity(volume),
		Standbys: map[string]cnpgRecoveredStorage{}, PreviousState: observation.States[previous],
	}
	for name := range pods {
		if name == previous {
			continue
		}
		peer, err := readUmamiV2Standby(ctx, c, observation, name, disks)
		if err != nil {
			return umamiV2Promotion{}, err
		}
		witness.Standbys[name] = peer
	}
	if len(witness.Standbys) != 3 || witness.Standbys[target].Volume.UID == "" {
		return umamiV2Promotion{}, refused
	}
	return witness, nil
}

func verifyUmamiPromotionHistory(before postgresState, after cnpgObservation, target string) error {
	refused := errors.New("the new primary has not proven continuity with the old write history")
	if text(after.Cluster, "status", "currentPrimary") != target || text(after.Cluster, "status", "targetPrimary") != target ||
		len(after.States) != 4 {
		return refused
	}
	primary := after.States[target]
	if validPostgresState(before) != nil || validPostgresState(primary) != nil || before.InRecovery || primary.InRecovery ||
		primary.SystemID != before.SystemID {
		return refused
	}
	oldTimeline, _ := strconv.ParseUint(before.Timeline, 10, 32)
	newTimeline, _ := strconv.ParseUint(primary.Timeline, 10, 32)
	oldPosition, _ := walPosition(before.FlushLSN)
	newPosition, _ := walPosition(primary.FlushLSN)
	if newTimeline <= oldTimeline || newPosition < oldPosition {
		return refused
	}
	for name, standby := range after.States {
		if name != target && verifyPostgresStandby(primary, standby) != nil {
			return refused
		}
	}
	return nil
}

func requestUmamiV2Promotion(ctx context.Context, c client, source string, cluster object, previous, target string) error {
	updates, err := umamiV2SwitchoverPatch(cluster, previous, target, time.Now())
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	body, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	output, err := c.kubectl(ctx, nil, "-n", "umami", "patch", "clusters.postgresql.cnpg.io", "umami-db",
		"--subresource=status", "--type=json", "--patch="+string(body), "-o", "json")
	var confirmation object
	if err != nil || json.Unmarshal(output, &confirmation) != nil || benchmarkIdentity(confirmation) != benchmarkIdentity(cluster) ||
		text(confirmation, "status", "targetPrimary") != target {
		return errors.New("switchover acknowledgement is unverified, preserve its journal")
	}
	return nil
}

func waitForUmamiV2Primary(ctx context.Context, c client, source, target string, witness umamiV2Promotion, disks []cnpgRecoveryDiskBinding) error {
	return waitReadyWithin(ctx, 10*time.Minute, func(ctx context.Context) error {
		current, err := observeCNPGMigration(ctx, c, "umami-db", source)
		if err != nil {
			return err
		}
		if benchmarkIdentity(current.Cluster) != witness.Database {
			return errors.Join(errReadinessTerminal, errors.New("the witnessed Umami cluster was replaced"))
		}
		if err = verifyUmamiPromotionHistory(witness.PreviousState, current, target); err != nil {
			return err
		}
		pods, err := indexed(current.Pods)
		if err != nil || benchmarkIdentity(pods[witness.Previous.Name]) != witness.Previous {
			return errors.New("the original primary has not returned as the retained standby")
		}
		for name, peer := range witness.Standbys {
			if benchmarkIdentity(pods[name]) != peer.Pod {
				return errors.New("a V2 member changed during the switchover")
			}
		}
		newPrimary := witness.Standbys[target]
		claim, err := c.get(ctx, "persistentvolumeclaim", "umami", newPrimary.Claim.Name, false)
		if err != nil || benchmarkIdentity(claim) != newPrimary.Claim ||
			text(claim, "spec", "storageClassName") != "longhorn-v2" || text(claim, "spec", "volumeName") != newPrimary.PV.Name ||
			!recoveredPostgresMount(pods[target], claim) {
			return errors.New("the new primary has not rebound to the witnessed V2 claim")
		}
		volume, err := c.get(ctx, "volumes.longhorn.io", "longhorn-system", newPrimary.Volume.Name, false)
		if err != nil || benchmarkIdentity(volume) != newPrimary.Volume {
			return errors.New("the new primary V2 engine is unverified")
		}
		pv, err := c.get(ctx, "persistentvolume", "", newPrimary.PV.Name, false)
		if err != nil || benchmarkIdentity(pv) != newPrimary.PV {
			return errors.New("the new primary persistent volume changed")
		}
		engine, err := c.get(ctx, "engines.longhorn.io", "longhorn-system", newPrimary.Engine.Name, false)
		if err != nil || benchmarkIdentity(engine) != newPrimary.Engine ||
			!recoveredPVBackend(claim, pv, volume, engine, "umami", newPrimary.PV.Name, "5Gi", "5368709120", text(pods[target], "spec", "nodeName")) {
			return errors.New("the new primary V2 engine is unverified")
		}
		replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
		if err != nil || !recoveredStorageInventory(replicas, "longhorn.io/v1beta2", "Replica", "longhorn-system") {
			return errors.New("the new primary replica inventory is unverified")
		}
		_, err = recoveredNativeCopies(disks, volume, engine, replicas, "5368709120")
		return err
	})
}

func runUmamiV2Switchover(ctx context.Context, c client, source, run, previous, target string) error {
	if c.context != "admin@prod" || !validBenchmarkOwner(run, source) {
		return errors.New("the protected source must own the Umami switchover")
	}
	before, err := observeCNPGMigration(ctx, c, "umami-db", source)
	if err != nil {
		return err
	}
	archive, err := rebindUmamiArchiveProof(ctx, c, before)
	if err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	storage, err := activation.recoveryStorage(ctx, archive.Plan.Source)
	if err != nil || !recoveryJSONEqual(storage, *archive.Storage) {
		return errors.New("the recovered and qualified storage identity changed")
	}
	first, err := observeUmamiV2Promotion(ctx, c, before, previous, target, storage.Disks)
	if err != nil {
		return err
	}
	prior, err := c.get(ctx, "configmap", "longhorn-system", umamiV2SwitchoverJournal, true)
	if err != nil || prior != nil {
		return errors.New("an earlier Umami switchover must be reconciled before starting another")
	}
	latest, err := observeCNPGMigration(ctx, c, "umami-db", source)
	if err != nil {
		return err
	}
	if err = validateCNPGObservation(before, latest); err != nil {
		return err
	}
	retained, err := observeUmamiV2Promotion(ctx, c, latest, previous, target, storage.Disks)
	if err != nil || first.Database != retained.Database || first.Previous != retained.Previous ||
		first.OriginalClaim != retained.OriginalClaim || first.OldPV != retained.OldPV ||
		first.OriginalVolume != retained.OriginalVolume || !reflect.DeepEqual(first.Standbys, retained.Standbys) {
		return errors.New("the witnessed source or standby storage changed before promotion")
	}
	intent, err := json.Marshal(object{"source": source, "run": run, "target": target, "phase": "promotion-requested", "witness": retained})
	if err != nil {
		return err
	}
	journal, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": umamiV2SwitchoverJournal, "namespace": "longhorn-system",
			"labels": object{"storage.platform.devantler.tech/recovery-run": run}},
		"data": object{"record": string(intent)}})
	if err != nil || text(journal, "data", "record") != string(intent) {
		return errors.New("the Umami switchover intent is unconfirmed")
	}
	if err = requestUmamiV2Promotion(ctx, c, source, latest.Cluster, previous, target); err != nil {
		return err
	}
	if err = waitForUmamiV2Primary(ctx, c, source, target, retained, storage.Disks); err != nil {
		return err
	}
	if err = sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	observed, err := c.get(ctx, "configmap", "longhorn-system", umamiV2SwitchoverJournal, false)
	if err != nil || benchmarkIdentity(observed) != benchmarkIdentity(journal) || text(observed, "data", "record") != string(intent) {
		return errors.New("the Umami switchover journal has changed")
	}
	return c.patch(ctx, "configmap", "longhorn-system", observed, object{"op": "add", "path": "/data/verified", "value": "v2-primary"})
}
