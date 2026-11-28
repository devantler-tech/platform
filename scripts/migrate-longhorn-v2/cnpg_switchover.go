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
	umamiV2SwitchoverOperation   = "switch-umami-primary-to-v2"
	weddingV2SwitchoverOperation = "switch-wedding-primary-to-v2"
	umamiV2SwitchoverJournal     = "longhorn-v2-umami-primary-switchover"
	umamiV2SwitchoverNamespace   = "longhorn-system"
)

type umamiV2Promotion struct {
	Database, Previous   benchmarkResource
	OriginalClaim, OldPV benchmarkResource
	OriginalVolume       benchmarkResource
	Standbys             map[string]cnpgRecoveredStorage
	PreviousState        postgresState
	Generation           uint64
	PreviousTargetSince  string
}

type umamiV2PromotionRecord struct {
	Source, Run, Target string
	Witness             umamiV2Promotion
	RetrySource         string `json:",omitempty"`
	RetryRun            string `json:",omitempty"`
}

func cnpgPromotionSource(identity benchmarkResource) cnpgSource {
	return cnpgSource{Namespace: identity.Namespace, Name: identity.Name}
}

func cnpgSwitchoverJournalName(database cnpgSource) string {
	switch database {
	case cnpgSource{Namespace: "umami", Name: "umami-db"}:
		return umamiV2SwitchoverJournal
	case weddingStandbySource:
		return "longhorn-v2-wedding-primary-switchover"
	default:
		return ""
	}
}

func (r umamiV2PromotionRecord) executor() (run, source string) {
	if r.RetryRun != "" {
		return r.RetryRun, r.RetrySource
	}
	return r.Run, r.Source
}

func readUmamiRetainedV1Storage(ctx context.Context, c client, cluster, pod object, name string) (benchmarkResource, benchmarkResource, benchmarkResource, error) {
	var empty benchmarkResource
	refused := errors.New("the retained database V1 storage is unverified")
	profile, admitted := cnpgStandbyTransition(cluster)
	if !admitted {
		return empty, empty, empty, refused
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", profile.Source.Namespace, name, false)
	backend := "pvc-" + text(claim, "metadata", "uid")
	if err != nil || !nativeRestoreUID(text(claim, "metadata", "uid")) || !recoveredPostgresMount(pod, claim) {
		return empty, empty, empty, refused
	}
	pv, err := c.get(ctx, "persistentvolume", "", backend, false)
	if err != nil {
		return empty, empty, empty, refused
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", umamiV2SwitchoverNamespace, backend, false)
	if err != nil || !cnpgV1StorageRetained(profile, cluster, pod, claim, pv, volume) {
		return empty, empty, empty, refused
	}
	return benchmarkIdentity(claim), benchmarkIdentity(pv), benchmarkIdentity(volume), nil
}

func observeUmamiV2Promotion(ctx context.Context, c client, observation cnpgObservation, previous, target string, disks []cnpgRecoveryDiskBinding) (umamiV2Promotion, error) {
	refused := errors.New("the original primary and its V2 standbys are unverified")
	cluster := observation.Cluster
	profile, admitted := cnpgStandbyTransition(cluster)
	generation, ok := nativeObservationUint(value(cluster, "metadata", "generation"))
	targetSince := text(cluster, "status", "targetPrimaryTimestamp")
	_, stampErr := time.Parse(time.RFC3339Nano, targetSince)
	if !admitted || len(observation.Pods) != profile.Instances || len(observation.States) != profile.Instances ||
		text(cluster, "status", "currentPrimary") != previous || text(cluster, "status", "targetPrimary") != previous ||
		!ok || generation == 0 || stampErr != nil {
		return umamiV2Promotion{}, refused
	}
	pods, err := indexed(observation.Pods)
	if err != nil {
		return umamiV2Promotion{}, refused
	}
	primary := pods[previous]
	claim, pv, volume, err := readUmamiRetainedV1Storage(ctx, c, cluster, primary, previous)
	if err != nil || text(primary, "metadata", "labels", "cnpg.io/instanceRole") != "primary" {
		return umamiV2Promotion{}, refused
	}
	witness := umamiV2Promotion{
		Database: benchmarkIdentity(cluster), Previous: benchmarkIdentity(primary),
		OriginalClaim: claim, OldPV: pv, OriginalVolume: volume,
		Standbys: map[string]cnpgRecoveredStorage{}, PreviousState: observation.States[previous],
		Generation: generation, PreviousTargetSince: targetSince,
	}
	for name := range pods {
		if name == previous {
			continue
		}
		peer, err := readCNPGV2Standby(ctx, c, observation, name, disks)
		if err != nil {
			return umamiV2Promotion{}, err
		}
		witness.Standbys[name] = peer
	}
	if len(witness.Standbys) != profile.Instances-1 || witness.Standbys[target].Volume.UID == "" {
		return umamiV2Promotion{}, refused
	}
	return witness, nil
}

func sameUmamiPromotionWitness(first, latest umamiV2Promotion) bool {
	oldFlush, oldErr := walPosition(first.PreviousState.FlushLSN)
	newFlush, newErr := walPosition(latest.PreviousState.FlushLSN)
	return oldErr == nil && newErr == nil && newFlush >= oldFlush &&
		first.PreviousState.SystemID == latest.PreviousState.SystemID && first.PreviousState.Timeline == latest.PreviousState.Timeline &&
		first.PreviousState.PostmasterStart == latest.PreviousState.PostmasterStart && first.PreviousState.PostmasterStart != "" &&
		first.Database == latest.Database && first.Previous == latest.Previous &&
		first.OriginalClaim == latest.OriginalClaim && first.OldPV == latest.OldPV && first.OriginalVolume == latest.OriginalVolume &&
		first.Generation == latest.Generation && first.PreviousTargetSince == latest.PreviousTargetSince &&
		reflect.DeepEqual(first.Standbys, latest.Standbys)
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

func promoteRecordedUmamiWriter(ctx context.Context, c client, source string, journal object, record umamiV2PromotionRecord, disks []cnpgRecoveryDiskBinding) error {
	witness, target := record.Witness, record.Target
	database := cnpgPromotionSource(witness.Database)
	current, err := observeCNPGMigration(ctx, c, database.Name, source)
	if err != nil || !umamiOriginalWriterCurrent(current.Cluster, witness) {
		return errors.New("the witnessed Umami cluster changed before the promotion request")
	}
	observed := witness
	observed.Previous = benchmarkIdentity(cnpgRecoveryPodByName(current.Pods, witness.Previous.Name))
	observed.PreviousState = current.States[witness.Previous.Name]
	if observed.PreviousState.InRecovery || !sameUmamiPromotionWitness(witness, observed) {
		return errors.New("the original Umami writer changed before promotion")
	}
	record.Witness = observed
	journal, err = writeUmamiPromotionIntent(ctx, c, source, journal, record)
	if err != nil {
		return err
	}
	fresh, err := c.readCNPGObservation(ctx, database)
	if err != nil || !umamiOriginalWriterCurrent(fresh.Cluster, witness) {
		return errors.New("the witnessed Umami members changed before promotion")
	}
	fresh.States = current.States
	targetStorage, err := readCNPGV2Standby(ctx, c, fresh, target, disks)
	if err != nil || !reflect.DeepEqual(targetStorage, witness.Standbys[target]) {
		return errors.New("the selected Umami writer's V2 storage changed before promotion")
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	cluster, err := c.get(ctx, "clusters.postgresql.cnpg.io", database.Namespace, database.Name, false)
	if err != nil || !umamiOriginalWriterCurrent(cluster, witness) {
		return errors.New("the witnessed Umami cluster changed before the promotion request")
	}
	updates, err := cnpgV2SwitchoverPatch(database, cluster, witness.Previous.Name, target, time.Now())
	if err != nil {
		return err
	}
	body, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	output, err := c.kubectl(ctx, nil, "-n", database.Namespace, "patch", "clusters.postgresql.cnpg.io", database.Name,
		"--subresource=status", "--type=json", "--patch="+string(body), "-o", "json")
	var confirmation object
	if err != nil || json.Unmarshal(output, &confirmation) != nil || benchmarkIdentity(confirmation) != benchmarkIdentity(cluster) ||
		text(confirmation, "status", "targetPrimary") != target {
		return errors.New("switchover acknowledgement is unverified, preserve its journal")
	}
	return verifyAndRecordUmamiV2Promotion(ctx, c, source, target, journal, observed, disks)
}

func umamiOriginalWriterCurrent(cluster object, witness umamiV2Promotion) bool {
	generation, ok := nativeObservationUint(value(cluster, "metadata", "generation"))
	_, admitted := cnpgStandbyTransition(cluster)
	return ok && admitted && generation == witness.Generation && benchmarkIdentity(cluster) == witness.Database &&
		text(cluster, "status", "currentPrimary") == witness.Previous.Name && text(cluster, "status", "targetPrimary") == witness.Previous.Name &&
		text(cluster, "status", "targetPrimaryTimestamp") == witness.PreviousTargetSince
}

func waitForUmamiV2Primary(ctx context.Context, c client, source, target string, witness umamiV2Promotion, disks []cnpgRecoveryDiskBinding) error {
	database := cnpgPromotionSource(witness.Database)
	profile, admitted := cnpgStandbyProfileForSource(database)
	if !admitted {
		return errors.New("the selected V2 writer profile is unverified")
	}
	return waitReadyWithin(ctx, 10*time.Minute, func(ctx context.Context) error {
		current, err := observeCNPGMigration(ctx, c, database.Name, source)
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
		oldPod := pods[witness.Previous.Name]
		oldClaim, oldPV, oldVolume, err := readUmamiRetainedV1Storage(ctx, c, current.Cluster, oldPod, witness.Previous.Name)
		if err != nil || oldClaim != witness.OriginalClaim || oldPV != witness.OldPV || oldVolume != witness.OriginalVolume {
			return errors.New("the original primary storage was not retained by the demoted standby")
		}
		for name, peer := range witness.Standbys {
			if benchmarkIdentity(pods[name]) != peer.Pod {
				return errors.New("a V2 member changed during the switchover")
			}
			if name != target {
				currentPeer, err := readCNPGV2Standby(ctx, c, current, name, disks)
				if err != nil || !reflect.DeepEqual(currentPeer, peer) {
					return errors.New("a V2 standby storage binding changed during the switchover")
				}
			}
		}
		newPrimary := witness.Standbys[target]
		claim, err := c.get(ctx, "persistentvolumeclaim", database.Namespace, newPrimary.Claim.Name, false)
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
			!recoveredPVBackend(claim, pv, volume, engine, database.Namespace, newPrimary.PV.Name, profile.Size, profile.SizeBytes, text(pods[target], "spec", "nodeName")) {
			return errors.New("the new primary V2 engine is unverified")
		}
		replicas, err := c.list(ctx, "replicas.longhorn.io", "longhorn-system")
		if err != nil || !recoveredStorageInventory(replicas, "longhorn.io/v1beta2", "Replica", "longhorn-system") {
			return errors.New("the new primary replica inventory is unverified")
		}
		copies, err := recoveredNativeCopies(disks, volume, engine, replicas, profile.SizeBytes)
		if err != nil || !reflect.DeepEqual(copies, newPrimary.Replicas) {
			return errors.New("the new primary has not retained the three witnessed V2 replicas")
		}
		if err := verifyUmamiTimeline(ctx, c, current.Cluster, pods[target], witness.PreviousState, current.States[target]); err != nil {
			return err
		}
		return verifyUmamiWriterService(ctx, c, current.Cluster, pods[target])
	})
}

func runCNPGV2Switchover(ctx context.Context, c client, database cnpgSource, source, run, previous, target string) error {
	journalName := cnpgSwitchoverJournalName(database)
	if c.context != "admin@prod" || !validBenchmarkOwner(run, source) {
		return errors.New("the protected source must own the database switchover")
	}
	if journalName == "" {
		return errors.New("the selected database has no reviewed switchover path")
	}
	before, err := observeCNPGMigration(ctx, c, database.Name, source)
	if err != nil {
		return err
	}
	activation := temporaryV2Activation{c: c, source: source, owner: run, pools: c.inventoryPools}
	storage, err := activation.rebindCNPGStandbyArchiveStorage(ctx, database, before, true)
	if err != nil {
		return err
	}
	prior, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, journalName, true)
	if err != nil {
		return err
	}
	if prior != nil {
		return resumeCNPGV2Promotion(ctx, c, database, source, run, previous, target, before, prior, storage.Disks)
	}
	first, err := observeUmamiV2Promotion(ctx, c, before, previous, target, storage.Disks)
	if err != nil {
		return err
	}
	latest, err := observeCNPGMigration(ctx, c, database.Name, source)
	if err != nil {
		return err
	}
	if err = validateCNPGObservation(before, latest); err != nil {
		return err
	}
	retained, err := observeUmamiV2Promotion(ctx, c, latest, previous, target, storage.Disks)
	if err != nil || !sameUmamiPromotionWitness(first, retained) {
		return errors.New("the witnessed source or standby storage changed before promotion")
	}
	record := umamiV2PromotionRecord{Source: source, Run: run, Target: target, Witness: retained}
	intent, err := json.Marshal(record)
	if err != nil {
		return err
	}
	journal, err := c.create(ctx, source, object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": journalName, "namespace": umamiV2SwitchoverNamespace,
			"labels": object{"app.kubernetes.io/managed-by": "github-actions", "storage.platform.devantler.tech/recovery-run": run}},
		"data": object{"record": string(intent)}})
	if err != nil || text(journal, "data", "record") != string(intent) || text(journal, "data", "verified") != "" {
		return errors.New("the Umami switchover intent is unconfirmed")
	}
	return promoteRecordedUmamiWriter(ctx, c, source, journal, record, storage.Disks)
}
