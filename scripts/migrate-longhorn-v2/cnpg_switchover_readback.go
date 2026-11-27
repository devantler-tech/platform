package main

import (
	"context"
	"encoding/json"
	"errors"
)

func bindUmamiPromotionRecord(journal object, previous, target string) (umamiV2PromotionRecord, error) {
	var record umamiV2PromotionRecord
	refused := errors.New("the previous Umami promotion record is unverified")
	raw := text(journal, "data", "record")
	if !recoveredNativeObject(journal, "v1", "ConfigMap", umamiV2SwitchoverNamespace) ||
		text(journal, "metadata", "name") != umamiV2SwitchoverJournal ||
		text(journal, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" ||
		len(raw) > 65536 || json.Unmarshal([]byte(raw), &record) != nil || !validBenchmarkOwner(record.Run, record.Source) ||
		text(journal, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != record.Run {
		return record, refused
	}
	encoded, err := json.Marshal(record)
	witness := record.Witness
	if err != nil || string(encoded) != raw || record.Target != target || witness.Previous.Name != previous ||
		witness.Database.Name != "umami-db" || witness.Database.Namespace != "umami" ||
		witness.Previous == witness.Standbys[target].Pod || witness.Standbys[target].Pod.Name != target || len(witness.Standbys) != 3 ||
		witness.PreviousState.InRecovery || validPostgresState(witness.PreviousState) != nil {
		return record, refused
	}
	return record, nil
}

// A retry can observe a promotion already accepted by CNPG. It never sends a
// second status write, even if the original request lost its acknowledgement.
func resumeUmamiV2Promotion(ctx context.Context, c client, source, run, previous, target string, cluster, journal object, disks []cnpgRecoveryDiskBinding) error {
	record, err := bindUmamiPromotionRecord(journal, previous, target)
	if err != nil {
		return err
	}
	if run == record.Run || benchmarkIdentity(cluster) != record.Witness.Database ||
		text(cluster, "status", "currentPrimary") != target || text(cluster, "status", "targetPrimary") != target {
		return errors.New("the previous run must be terminal and its named V2 writer must already be primary")
	}
	if err = benchmarkOwnerFinished(ctx, c, record.Run, record.Source); err != nil {
		return err
	}
	return verifyAndRecordUmamiV2Promotion(ctx, c, source, target, journal, record.Witness, disks)
}

func verifyAndRecordUmamiV2Promotion(ctx context.Context, c client, source, target string, journal object, witness umamiV2Promotion, disks []cnpgRecoveryDiskBinding) error {
	if err := waitForUmamiV2Primary(ctx, c, source, target, witness, disks); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	observed, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, umamiV2SwitchoverJournal, false)
	if err != nil || benchmarkIdentity(observed) != benchmarkIdentity(journal) ||
		text(observed, "data", "record") != text(journal, "data", "record") {
		return errors.New("the Umami switchover journal has changed")
	}
	switch value(observed, "data", "verified") {
	case "v2-primary":
		return nil
	case nil:
	default:
		return errors.New("the Umami switchover journal has an unknown receipt")
	}
	if err := c.patch(ctx, "configmap", umamiV2SwitchoverNamespace, observed, object{"op": "add", "path": "/data/verified", "value": "v2-primary"}); err != nil {
		return err
	}
	receipt, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, umamiV2SwitchoverJournal, false)
	if err != nil || benchmarkIdentity(receipt) != benchmarkIdentity(journal) ||
		text(receipt, "data", "record") != text(journal, "data", "record") || text(receipt, "data", "verified") != "v2-primary" {
		return errors.New("the Umami switchover verification receipt has not been confirmed")
	}
	return nil
}

func verifyUmamiWriterService(ctx context.Context, c client, cluster, writer object) error {
	refused := errors.New("the Umami writer service has not moved to the witnessed V2 writer")
	service, err := c.get(ctx, "service", "umami", "umami-db-rw", false)
	if err != nil || !recoveredNativeObject(service, "v1", "Service", "umami") || !cnpgOwnerReference(service, cluster) {
		return refused
	}
	slices, err := c.list(ctx, "endpointslices.discovery.k8s.io", "umami")
	if err != nil {
		return err
	}
	return bindUmamiWriterEndpoints(service, writer, slices)
}

func bindUmamiWriterEndpoints(service, writer object, slices []object) error {
	refused := errors.New("the Umami writer service has an unknown or stale route")
	active := 0
	for _, slice := range slices {
		if text(slice, "metadata", "labels", "kubernetes.io/service-name") != "umami-db-rw" {
			continue
		}
		if !recoveredNativeObject(slice, "discovery.k8s.io/v1", "EndpointSlice", "umami") {
			return refused
		}
		refs, _ := value(slice, "metadata", "ownerReferences").([]any)
		if len(refs) != 1 {
			return refused
		}
		owner, _ := refs[0].(object)
		if text(owner, "apiVersion") != "v1" || text(owner, "kind") != "Service" ||
			text(owner, "name") != "umami-db-rw" || text(owner, "uid") != text(service, "metadata", "uid") || value(owner, "controller") != true {
			return refused
		}
		endpoints, ok := value(slice, "endpoints").([]any)
		if !ok {
			return refused
		}
		for _, raw := range endpoints {
			endpoint, ok := raw.(object)
			if !ok || value(endpoint, "conditions", "ready") != true || value(endpoint, "conditions", "serving") != true ||
				value(endpoint, "conditions", "terminating") != false ||
				text(endpoint, "targetRef", "kind") != "Pod" || text(endpoint, "targetRef", "namespace") != "umami" ||
				text(endpoint, "targetRef", "name") != text(writer, "metadata", "name") ||
				text(endpoint, "targetRef", "uid") != text(writer, "metadata", "uid") {
				return refused
			}
			active++
		}
	}
	if active == 0 {
		return refused
	}
	return nil
}
