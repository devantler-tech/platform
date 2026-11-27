package main

import (
	"context"
	"errors"
	"os"
	"time"
)

const umamiStandbyReplacementOperation = "replace-umami-v2-standby"

func executeUmamiStandbyReplacement(ctx context.Context, c client, operation string) error {
	executor := umamiReplacementOwner{Source: os.Getenv("GITHUB_SHA"), Run: os.Getenv("GITHUB_RUN_ID")}
	if operation == recoverUmamiStandbyNameOperation {
		input, err := authorizeUmamiStandbyNameRecovery(os.Getenv)
		if err != nil {
			return err
		}
		return runUmamiNameRecovery(ctx, c, input, executor)
	}
	if operation == resumeUmamiStandbyOperation {
		prior, err := authorizeUmamiStandbyResume(os.Getenv)
		if err != nil {
			return err
		}
		return resumeUmamiStandbyReplacement(ctx, c, executor, prior)
	}
	target, spare, err := authorizeUmamiStandbyTarget(os.Getenv, operation)
	if err != nil {
		return err
	}
	return runUmamiStandbyReplacement(ctx, c, executor.Source, executor.Run, target, spare)
}

func runUmamiStandbyReplacement(ctx context.Context, c client, source, run, target, spare string) error {
	witness, err := observeUmamiStandbyCutover(ctx, c, source, target, spare)
	if err != nil {
		return err
	}
	if err = verifyUmamiStandbyEvictionAdmission(ctx, c, source, witness); err != nil {
		return err
	}
	current, err := c.readCNPGObservation(ctx, cnpgSource{Namespace: "umami", Name: "umami-db"})
	if err != nil || benchmarkIdentity(current.Cluster) != witness.Database || len(current.Pods) != 4 {
		return errors.New("the witnessed Umami members changed before replacement")
	}
	peers := map[string]benchmarkResource{}
	for _, pod := range current.Pods {
		identity := benchmarkIdentity(pod)
		if identity.Name == target && identity == witness.Consumer {
			continue
		}
		peers[identity.Name] = identity
	}
	primary := peers[text(current.Cluster, "status", "currentPrimary")]
	journal, err := beginUmamiStandbyReplacement(ctx, c, umamiStandbyReplacementRecord{
		Version: 1, Source: source, Run: run, Phase: umamiReplacementPlanned,
		Primary: primary, Original: witness, OtherPods: peers,
	})
	if err != nil {
		return err
	}
	if err = retainUmamiOriginalStandby(ctx, &journal); err != nil {
		return err
	}
	if err = evictUmamiRetainedStandby(ctx, &journal); err != nil {
		return err
	}
	return observeUmamiStandbyRebuild(ctx, &journal)
}

func observeUmamiStandbyRebuild(ctx context.Context, journal *umamiStandbyReplacementJournal) error {
	if journal.record.Phase != umamiReplacementEvictionPending {
		return errors.New("the recorded Umami eviction cannot be observed at this phase")
	}
	if err := waitReadyWithin(ctx, 8*time.Minute, func(ctx context.Context) error {
		return observeUmamiOriginalDetached(ctx, journal)
	}); err != nil {
		return err
	}
	next := journal.record
	next.Phase = umamiReplacementOriginalDetached
	if err := journal.transition(ctx, next); err != nil {
		return err
	}
	return observeUmamiReplacementReady(ctx, journal)
}

func observeUmamiReplacementReady(ctx context.Context, journal *umamiStandbyReplacementJournal) error {
	if journal.record.Phase != umamiReplacementOriginalDetached {
		return errors.New("the original Umami storage is not verified as detached")
	}
	var replacement cnpgRecoveredStorage
	if err := waitReadyWithin(ctx, 12*time.Minute, func(ctx context.Context) error {
		original := observeUmamiOriginalDetached
		if journal.record.NameRelease != nil {
			original = func(ctx context.Context, j *umamiStandbyReplacementJournal) error {
				return observeUmamiReleasedOriginal(ctx, j, false)
			}
		}
		if err := original(ctx, journal); err != nil {
			return err
		}
		candidate, err := readUmamiReplacementStorage(ctx, journal)
		if err == nil {
			replacement = candidate
		}
		return err
	}); err != nil {
		return err
	}
	next := journal.record
	next.Phase, next.Replacement = umamiReplacementStandbyRebuilt, &replacement
	return journal.transition(ctx, next)
}

func observeUmamiOriginalDetached(ctx context.Context, journal *umamiStandbyReplacementJournal) error {
	c, witness := journal.c, journal.record.Original
	cluster, err := c.get(ctx, "clusters.postgresql.cnpg.io", witness.Database.Namespace, witness.Database.Name, false)
	if err != nil || benchmarkIdentity(cluster) != witness.Database {
		return errors.Join(errReadinessTerminal, errors.New("the original Umami cluster could not be verified"))
	}
	if err = verifyUmamiReplacementPrimary(ctx, c, cluster, journal.record.Primary); err != nil {
		return errors.Join(errReadinessTerminal, err)
	}
	pods, err := c.list(ctx, "pods", witness.Consumer.Namespace)
	if err != nil {
		return err
	}
	for _, identity := range journal.record.OtherPods {
		pod := cnpgRecoveryPodByName(pods, identity.Name)
		if benchmarkIdentity(pod) != identity || verifyPostgresPod(pod, pod, cluster) != nil {
			return errors.Join(errReadinessTerminal, errors.New("an unaffected Umami member changed during replacement"))
		}
	}
	claim, err := c.get(ctx, "persistentvolumeclaim", witness.Claim.Namespace, witness.Claim.Name, false)
	if err != nil {
		return err
	}
	pv, err := c.get(ctx, "persistentvolume", "", witness.PV.Name, false)
	if err != nil {
		return err
	}
	volume, err := c.get(ctx, "volumes.longhorn.io", witness.Volume.Namespace, witness.Volume.Name, false)
	if err != nil {
		return err
	}
	attachments, err := c.list(ctx, "volumeattachments.storage.k8s.io", "")
	if err != nil {
		return err
	}
	return verifyUmamiStandbyDetached(witness, claim, pv, volume, pods, attachments)
}

func readUmamiReplacementStorage(ctx context.Context, journal *umamiStandbyReplacementJournal) (cnpgRecoveredStorage, error) {
	const target = "umami-db"
	c, record := journal.c, journal.record
	executor := journal.executor()
	observation, err := observeCNPGMigration(ctx, c, target, executor.Source)
	if err != nil {
		return cnpgRecoveredStorage{}, err
	}
	if benchmarkIdentity(observation.Cluster) != record.Original.Database {
		return cnpgRecoveredStorage{}, errors.Join(errReadinessTerminal, errors.New("the database changed after the original member was detached"))
	}
	if err = verifyUmamiReplacementPrimary(ctx, c, observation.Cluster, record.Primary); err != nil {
		return cnpgRecoveredStorage{}, errors.Join(errReadinessTerminal, err)
	}
	var replacement string
	for _, pod := range observation.Pods {
		identity := benchmarkIdentity(pod)
		if original, ok := record.OtherPods[identity.Name]; ok && identity == original {
			continue
		}
		if replacement != "" || identity.UID == record.Original.Consumer.UID ||
			(identity.Name == record.Original.Consumer.Name) != (record.NameRelease != nil) ||
			record.OtherPods[identity.Name].UID != "" {
			return cnpgRecoveredStorage{}, errors.Join(errReadinessTerminal, errors.New("the database did not add exactly one new standby"))
		}
		replacement = identity.Name
	}
	if replacement == "" || len(observation.Pods) != 4 {
		return cnpgRecoveredStorage{}, errors.New("the new Umami standby is not ready")
	}
	archive, err := rebindUmamiArchiveProof(ctx, c, observation)
	if err != nil {
		return cnpgRecoveredStorage{}, err
	}
	activation := temporaryV2Activation{c: c, source: executor.Source, owner: executor.Run, pools: c.inventoryPools}
	qualified, err := activation.recoveryStorage(ctx, archive.Plan.Source)
	if err != nil || !recoveryJSONEqual(qualified, *archive.Storage) {
		return cnpgRecoveredStorage{}, errors.New("the new standby raw storage is unverified")
	}
	return readUmamiV2Standby(ctx, c, observation, replacement, qualified.Disks)
}
