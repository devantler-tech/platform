package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
)

const (
	umamiReplacementPlanned          = "planned"
	umamiReplacementRetentionPending = "retention-requested"
	umamiReplacementClaimRetained    = "claim-retained"
	umamiReplacementEvictionPending  = "eviction-requested"
	umamiReplacementOriginalDetached = "original-detached"
	umamiReplacementStandbyRebuilt   = "replacement-ready"
)

type umamiStandbyReplacementRecord struct {
	Version            int
	Source, Run, Phase string
	Primary            benchmarkResource
	Original           cnpgStandbyCutover
	OtherPods          map[string]benchmarkResource
	Replacement        *cnpgRecoveredStorage    `json:",omitempty"`
	Continuation       *umamiReplacementOwner   `json:",omitempty"`
	NameRelease        *umamiStandbyNameRelease `json:",omitempty"`
}

const umamiStandbyJournalNamespace = "longhorn-system"

type umamiReplacementOwner struct {
	Source, Run string
}

type umamiStandbyReplacementJournal struct {
	c      client
	uid    string
	record umamiStandbyReplacementRecord
	resume *umamiReplacementOwner
}

func (j umamiStandbyReplacementJournal) executor() umamiReplacementOwner {
	if j.resume != nil {
		return *j.resume
	}
	return umamiReplacementOwner{Source: j.record.Source, Run: j.record.Run}
}

func beginUmamiStandbyReplacement(ctx context.Context, c client, record umamiStandbyReplacementRecord) (umamiStandbyReplacementJournal, error) {
	var journal umamiStandbyReplacementJournal
	if c.context != "admin@prod" || record.Phase != umamiReplacementPlanned || record.Replacement != nil ||
		record.Continuation != nil || record.NameRelease != nil || !validUmamiOriginalReplacement(record) {
		return journal, errors.New("the original Umami standby replacement witness is incomplete")
	}
	name := umamiStandbyJournalName(record)
	prior, err := c.get(ctx, "configmap", umamiStandbyJournalNamespace, name, true)
	if err != nil {
		return journal, err
	}
	if prior != nil {
		return journal, errors.New("an existing Umami replacement journal requires explicit continuation")
	}
	body, err := json.Marshal(record)
	if err != nil {
		return journal, err
	}
	journalConfig := object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": object{"name": name, "namespace": umamiStandbyJournalNamespace,
			"labels": object{"storage.platform.devantler.tech/recovery-run": record.Run, "app.kubernetes.io/managed-by": "github-actions"}},
		"data": object{"record": string(body)},
	}
	created, err := c.create(ctx, record.Source, journalConfig)
	if err != nil {
		return journal, err
	}
	journal = umamiStandbyReplacementJournal{c: c, uid: text(created, "metadata", "uid"), record: record}
	if _, err = journal.read(ctx); err != nil {
		return umamiStandbyReplacementJournal{}, err
	}
	return journal, nil
}

func (j *umamiStandbyReplacementJournal) transition(ctx context.Context, next umamiStandbyReplacementRecord) error {
	if next.Phase == umamiReplacementEvictionPending {
		next.Continuation = j.resume
	}
	if err := verifyUmamiReplacementTransition(j.record, next); err != nil {
		return err
	}
	current, err := j.read(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err = sourceCurrent(ctx, j.c.run, j.executor().Source); err != nil {
		return err
	}
	if err = j.c.patch(ctx, "configmap", umamiStandbyJournalNamespace, current, object{
		"op": "replace", "path": "/data/record", "value": string(body),
	}); err != nil {
		return err
	}
	updated := *j
	updated.record = next
	if _, err = updated.read(ctx); err != nil {
		return err
	}
	*j = updated
	return nil
}

func (j umamiStandbyReplacementJournal) read(ctx context.Context) (object, error) {
	name := umamiStandbyJournalName(j.record)
	cm, err := j.c.get(ctx, "configmap", umamiStandbyJournalNamespace, name, false)
	want, encodeErr := json.Marshal(j.record)
	if err != nil || encodeErr != nil || !recoveredNativeObject(cm, "v1", "ConfigMap", umamiStandbyJournalNamespace) ||
		text(cm, "metadata", "name") != name || text(cm, "metadata", "uid") != j.uid ||
		text(cm, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" ||
		text(cm, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != j.record.Run ||
		text(cm, "data", "record") != string(want) {
		return nil, errors.New("the Umami replacement journal does not match the witnessed state")
	}
	return cm, nil
}

func umamiStandbyJournalName(record umamiStandbyReplacementRecord) string {
	return "longhorn-v2-standby-replacement-" + record.Original.Consumer.Name
}

func validUmamiOriginalReplacement(record umamiStandbyReplacementRecord) bool {
	witness := record.Original
	name := witness.Consumer.Name
	if record.Version != 1 || !validBenchmarkOwner(record.Run, record.Source) ||
		witness.Database.Name != "umami-db" || witness.Database.Namespace != "umami" ||
		record.Primary.Name == name || record.Primary.Name == witness.Spare.Pod.Name ||
		witness.Consumer.Namespace != "umami" || record.Primary.Namespace != "umami" ||
		witness.Claim.Name != name || witness.Claim.Namespace != "umami" ||
		witness.PV.Name != "pvc-"+witness.Claim.UID || witness.PV.Namespace != "" ||
		witness.Volume.Name != witness.PV.Name || witness.Volume.Namespace != "longhorn-system" ||
		witness.Budget.Name != "umami-db" || witness.Budget.Namespace != "umami" ||
		witness.ConsumerVersion == "" || witness.BudgetVersion == "" ||
		len(record.OtherPods) != 3 || record.OtherPods[record.Primary.Name] != record.Primary ||
		record.OtherPods[witness.Spare.Pod.Name] != witness.Spare.Pod ||
		witness.Spare.Cluster != witness.Database || witness.Spare.Pod.Namespace != "umami" || witness.Spare.Pod.Name == name {
		return false
	}
	for _, instance := range []string{name, record.Primary.Name, witness.Spare.Pod.Name} {
		if _, err := cnpgInstanceID(instance, "umami-db"); err != nil {
			return false
		}
	}
	for _, resource := range []benchmarkResource{record.Primary, witness.Database, witness.Consumer, witness.Claim, witness.PV, witness.Volume, witness.Budget, witness.Spare.Pod} {
		if !isolationUID.MatchString(resource.UID) {
			return false
		}
	}
	seen := map[string]bool{witness.Consumer.UID: true}
	for name, pod := range record.OtherPods {
		if _, err := cnpgInstanceID(name, "umami-db"); err != nil || pod.Name != name || pod.Namespace != "umami" ||
			!isolationUID.MatchString(pod.UID) || name == witness.Consumer.Name || seen[pod.UID] {
			return false
		}
		seen[pod.UID] = true
	}
	return true
}

func verifyUmamiReplacementTransition(previous, next umamiStandbyReplacementRecord) error {
	refused := errors.New("the Umami standby replacement journal transition is unverified")
	if previous.Version != 1 || next.Version != previous.Version ||
		!validBenchmarkOwner(previous.Run, previous.Source) ||
		next.Source != previous.Source || next.Run != previous.Run ||
		next.Primary != previous.Primary || !reflect.DeepEqual(next.Original, previous.Original) ||
		!reflect.DeepEqual(next.OtherPods, previous.OtherPods) ||
		previous.Replacement != nil {
		return refused
	}
	var expected string
	switch previous.Phase {
	case umamiReplacementPlanned:
		expected = umamiReplacementRetentionPending
	case umamiReplacementRetentionPending:
		expected = umamiReplacementClaimRetained
	case umamiReplacementClaimRetained:
		expected = umamiReplacementEvictionPending
	case umamiReplacementEvictionPending:
		expected = umamiReplacementOriginalDetached
	case umamiReplacementOriginalDetached:
		expected = umamiReplacementStandbyRebuilt
	}
	if !validUmamiNameReleaseProgress(previous, next) && (expected == "" || next.Phase != expected ||
		!reflect.DeepEqual(previous.NameRelease, next.NameRelease) ||
		previous.NameRelease != nil && previous.Phase != umamiReplacementOriginalDetached) {
		return refused
	}
	if next.Continuation != nil && previous.Phase == umamiReplacementClaimRetained {
		owner := next.Continuation
		if previous.Continuation != nil || !validBenchmarkOwner(owner.Run, owner.Source) ||
			owner.Run == previous.Run || owner.Source == previous.Source {
			return refused
		}
	} else if !reflect.DeepEqual(next.Continuation, previous.Continuation) {
		return refused
	}
	if next.Phase == umamiReplacementStandbyRebuilt {
		if next.Replacement == nil || !validUmamiReplacementStorage(next, *next.Replacement) {
			return refused
		}
	} else if next.Replacement != nil {
		return refused
	}
	return nil
}

func independentUmamiReplacement(original cnpgStandbyCutover, replacement cnpgRecoveredStorage) bool {
	return independentUmamiReplacementAfterRelease(original, replacement, false)
}

func validUmamiReplacementStorage(record umamiStandbyReplacementRecord, replacement cnpgRecoveredStorage) bool {
	if record.NameRelease == nil {
		return independentUmamiReplacement(record.Original, replacement)
	}
	return independentUmamiReplacementAfterRelease(record.Original, replacement, true)
}

func independentUmamiReplacementAfterRelease(original cnpgStandbyCutover, replacement cnpgRecoveredStorage, released bool) bool {
	pvName := "pvc-" + replacement.Claim.UID
	if replacement.Cluster != original.Database || replacement.Pod.Namespace != original.Consumer.Namespace ||
		replacement.Claim.Namespace != replacement.Pod.Namespace || replacement.Claim.Name != replacement.Pod.Name ||
		replacement.PV.Namespace != "" || replacement.PV.Name != pvName ||
		replacement.Volume.Namespace != "longhorn-system" || replacement.Volume.Name != pvName ||
		replacement.Engine.Namespace != "longhorn-system" || replacement.Engine.UID == original.Spare.Engine.UID ||
		replacement.Pod.UID == original.Consumer.UID || (replacement.Pod.Name == original.Consumer.Name && !released) ||
		replacement.Pod.UID == original.Spare.Pod.UID || replacement.Pod.Name == original.Spare.Pod.Name ||
		replacement.Claim.UID == original.Claim.UID || replacement.Claim.UID == original.Spare.Claim.UID ||
		replacement.PV.UID == original.PV.UID || replacement.PV.UID == original.Spare.PV.UID ||
		replacement.Volume.UID == original.Volume.UID || replacement.Volume.UID == original.Spare.Volume.UID ||
		len(replacement.Replicas) != 3 {
		return false
	}
	if _, err := cnpgInstanceID(replacement.Pod.Name, "umami-db"); err != nil {
		return false
	}
	for _, resource := range []benchmarkResource{replacement.Pod, replacement.Claim, replacement.PV, replacement.Volume, replacement.Engine} {
		if !isolationUID.MatchString(resource.UID) {
			return false
		}
	}
	seen := map[string]bool{}
	for _, replica := range replacement.Replicas {
		if replica.Namespace != "longhorn-system" || replica.Name == "" || !isolationUID.MatchString(replica.UID) || seen[replica.UID] {
			return false
		}
		seen[replica.UID] = true
	}
	return true
}
