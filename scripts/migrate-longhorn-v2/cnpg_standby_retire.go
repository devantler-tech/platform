package main

import (
	"context"
	"errors"
	"reflect"
)

// retainUmamiOriginalStandby leaves the current Pod running while it removes
// the operator's ownership of the original PVC. The journal must be established
// by the protected caller after observing the archive, spare and eviction
// admission.
func retainUmamiOriginalStandby(ctx context.Context, journal *umamiStandbyReplacementJournal) (original object, err error) {
	if journal.record.Phase != umamiReplacementPlanned {
		return nil, errors.New("the existing Umami retention attempt needs explicit continuation")
	}
	next := journal.record
	next.Phase = umamiReplacementRetentionPending
	if err = journal.transition(ctx, next); err != nil {
		return nil, err
	}
	witness := journal.record.Original
	actual, err := readUmamiStandbyAdmission(ctx, journal.c, witness)
	if err != nil {
		return nil, err
	}
	if err = verifyUmamiReplacementPrimary(ctx, journal.c, actual.cluster, journal.record.Primary); err != nil {
		return nil, err
	}
	changes, err := umamiStandbyClaimRetentionChanges(witness, actual.cluster, actual.pod, actual.claim, actual.pv, actual.volume)
	if err != nil {
		return nil, err
	}
	if err = sourceCurrent(ctx, journal.c.run, journal.record.Source); err != nil {
		return nil, err
	}
	if _, err = journal.read(ctx); err != nil {
		return nil, err
	}
	if err = journal.c.patch(ctx, "persistentvolumeclaim", witness.Claim.Namespace, actual.claim, changes...); err != nil {
		return nil, err
	}
	retained, err := readUmamiStandbyAdmission(ctx, journal.c, witness)
	if err != nil {
		return nil, err
	}
	if err = verifyUmamiStandbyClaimRetention(actual.claim, retained.claim); err != nil {
		return nil, err
	}
	if !verifyUmamiOriginalClaimBinding(retained.claim, retained.pv, retained.volume) ||
		!umamiOriginalVolumeAttached(retained.pod, retained.volume) {
		return nil, errors.New("the retained Umami PVC or its attached backend changed")
	}
	if err = verifyUmamiReplacementPrimary(ctx, journal.c, retained.cluster, journal.record.Primary); err != nil {
		return nil, err
	}
	next = journal.record
	next.Phase = umamiReplacementClaimRetained
	if err = journal.transition(ctx, next); err != nil {
		return nil, err
	}
	return actual.claim, nil
}

// evictUmamiRetainedStandby uses the same PDB and UID-fenced API as the dry-run.
// A recorded eviction request cannot be repeated when its outcome is uncertain.
func evictUmamiRetainedStandby(ctx context.Context, journal *umamiStandbyReplacementJournal, original object) error {
	if journal.record.Phase != umamiReplacementClaimRetained {
		return errors.New("the existing Umami eviction attempt needs explicit continuation")
	}
	witness := journal.record.Original
	if err := verifyUmamiSpareBeforeRetirement(ctx, journal); err != nil {
		return err
	}
	if err := recheckUmamiRetainedAdmission(ctx, journal, original); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, journal.c.run, journal.record.Source); err != nil {
		return err
	}
	if err := requestUmamiStandbyEviction(ctx, journal.c, witness, true); err != nil {
		return err
	}
	next := journal.record
	next.Phase = umamiReplacementEvictionPending
	if err := journal.transition(ctx, next); err != nil {
		return err
	}
	if err := recheckUmamiRetainedAdmission(ctx, journal, original); err != nil {
		return err
	}
	if _, err := journal.read(ctx); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, journal.c.run, journal.record.Source); err != nil {
		return err
	}
	return requestUmamiStandbyEviction(ctx, journal.c, witness, false)
}

func verifyUmamiSpareBeforeRetirement(ctx context.Context, journal *umamiStandbyReplacementJournal) error {
	const target = "umami-db"
	observation, err := observeCNPGMigration(ctx, journal.c, target, journal.record.Source)
	if err != nil {
		return err
	}
	if benchmarkIdentity(observation.Cluster) != journal.record.Original.Database {
		return errors.New("the original Umami cluster changed before eviction")
	}
	if err = verifyUmamiReplacementPrimary(ctx, journal.c, observation.Cluster, journal.record.Primary); err != nil {
		return err
	}
	archive, err := rebindUmamiArchiveProof(ctx, journal.c, observation)
	if err != nil {
		return err
	}
	activation := temporaryV2Activation{c: journal.c, source: journal.record.Source, owner: journal.record.Run, pools: journal.c.inventoryPools}
	qualified, err := activation.recoveryStorage(ctx, archive.Plan.Source)
	if err != nil || !recoveryJSONEqual(qualified, *archive.Storage) {
		return errors.New("the Umami recovery storage changed before eviction")
	}
	spare := journal.record.Original.Spare
	current, err := readUmamiV2Standby(ctx, journal.c, observation, spare.Pod.Name, qualified.Disks)
	if err != nil || !reflect.DeepEqual(current, spare) {
		return errors.New("the witnessed Umami V2 spare changed before eviction")
	}
	return nil
}

func recheckUmamiRetainedAdmission(ctx context.Context, journal *umamiStandbyReplacementJournal, original object) error {
	actual, err := readUmamiStandbyAdmission(ctx, journal.c, journal.record.Original)
	if err != nil {
		return err
	}
	if err = verifyUmamiStandbyClaimRetention(original, actual.claim); err != nil {
		return err
	}
	if !verifyUmamiOriginalClaimBinding(actual.claim, actual.pv, actual.volume) ||
		!umamiOriginalVolumeAttached(actual.pod, actual.volume) {
		return errors.New("the retained Umami backend was not stable at eviction")
	}
	return verifyUmamiReplacementPrimary(ctx, journal.c, actual.cluster, journal.record.Primary)
}

func verifyUmamiReplacementPrimary(ctx context.Context, c client, cluster object, expected benchmarkResource) error {
	refused := errors.New("the original Umami write leader changed during replacement")
	if expected.Namespace != "umami" || text(cluster, "status", "currentPrimary") != expected.Name ||
		text(cluster, "status", "targetPrimary") != expected.Name {
		return refused
	}
	pod, err := c.get(ctx, "pod", expected.Namespace, expected.Name, false)
	if err != nil || benchmarkIdentity(pod) != expected ||
		text(pod, "metadata", "labels", "cnpg.io/instanceRole") != "primary" || verifyPostgresPod(pod, pod, cluster) != nil {
		return refused
	}
	return nil
}
