package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"
)

func bindCNPGPromotionRecord(database cnpgSource, journal object, previous, target string) (umamiV2PromotionRecord, error) {
	var record umamiV2PromotionRecord
	refused := errors.New("the previous Umami promotion record is unverified")
	raw := text(journal, "data", "record")
	if !recoveredNativeObject(journal, "v1", "ConfigMap", umamiV2SwitchoverNamespace) ||
		text(journal, "metadata", "name") != cnpgSwitchoverJournalName(database) ||
		text(journal, "metadata", "labels", "app.kubernetes.io/managed-by") != "github-actions" ||
		len(raw) > 65536 || json.Unmarshal([]byte(raw), &record) != nil || !validBenchmarkOwner(record.Run, record.Source) ||
		text(journal, "metadata", "labels", "storage.platform.devantler.tech/recovery-run") != record.Run {
		return record, refused
	}
	encoded, err := json.Marshal(record)
	witness := record.Witness
	_, stampErr := time.Parse(time.RFC3339Nano, witness.PreviousTargetSince)
	executor, source := record.executor()
	if err != nil || string(encoded) != raw || record.Target != target || witness.Previous.Name != previous ||
		cnpgPromotionSource(witness.Database) != database ||
		witness.Previous == witness.Standbys[target].Pod || witness.Standbys[target].Pod.Name != target || len(witness.Standbys) != 3 ||
		witness.Generation == 0 || stampErr != nil || !validBenchmarkOwner(executor, source) ||
		(record.RetryRun == "") != (record.RetrySource == "") ||
		witness.PreviousState.InRecovery || validPostgresState(witness.PreviousState) != nil {
		return record, refused
	}
	return record, nil
}

func resumeCNPGV2Promotion(ctx context.Context, c client, database cnpgSource, source, run, previous, target string, observation cnpgObservation, journal object, disks []cnpgRecoveryDiskBinding) error {
	record, err := bindCNPGPromotionRecord(database, journal, previous, target)
	if err != nil {
		return err
	}
	prior, priorSource := record.executor()
	if run == record.Run || run == prior || benchmarkIdentity(observation.Cluster) != record.Witness.Database {
		return errors.New("the previous Umami switchover run or cluster is unverified")
	}
	finished, err := readFinishedMigrationRun(ctx, c, prior, priorSource)
	if err != nil || finished.Attempt != 1 {
		return errors.New("the prior Umami switchover attempt is not verified terminal")
	}
	cluster := observation.Cluster
	if text(cluster, "status", "currentPrimary") == target && text(cluster, "status", "targetPrimary") == target {
		return verifyAndRecordUmamiV2Promotion(ctx, c, source, target, journal, record.Witness, disks)
	}
	if !umamiOriginalWriterCurrent(cluster, record.Witness) || value(journal, "data", "verified") != nil ||
		(finished.Conclusion != "failure" && finished.Conclusion != "cancelled" && finished.Conclusion != "timed_out") {
		return errors.New("the prior switchover could have changed the original writer")
	}
	witness, err := observeUmamiV2Promotion(ctx, c, observation, previous, target, disks)
	if err != nil || !sameUmamiPromotionWitness(record.Witness, witness) {
		return errors.New("the original writer or V2 standby proof changed before retry")
	}
	record.Witness, record.RetryRun, record.RetrySource = witness, run, source
	// The retry owns no promotion until it records its current witness and
	// repeats the target and compare-and-swap checks.
	return promoteRecordedUmamiWriter(ctx, c, source, journal, record, disks)
}

func writeUmamiPromotionIntent(ctx context.Context, c client, source string, journal object, record umamiV2PromotionRecord) (object, error) {
	next, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return nil, err
	}
	if err := c.patch(ctx, "configmap", umamiV2SwitchoverNamespace, journal,
		object{"op": "test", "path": "/data/record", "value": text(journal, "data", "record")},
		object{"op": "replace", "path": "/data/record", "value": string(next)}); err != nil {
		return nil, err
	}
	confirmed, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, cnpgSwitchoverJournalName(cnpgPromotionSource(record.Witness.Database)), false)
	if err != nil || benchmarkIdentity(confirmed) != benchmarkIdentity(journal) || text(confirmed, "data", "record") != string(next) {
		return nil, errors.New("the exact Umami promotion intent was not confirmed")
	}
	return confirmed, nil
}

func verifyAndRecordUmamiV2Promotion(ctx context.Context, c client, source, target string, journal object, witness umamiV2Promotion, disks []cnpgRecoveryDiskBinding) error {
	if err := waitForUmamiV2Primary(ctx, c, source, target, witness, disks); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, c.run, source); err != nil {
		return err
	}
	journalName := cnpgSwitchoverJournalName(cnpgPromotionSource(witness.Database))
	observed, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, journalName, false)
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
	receipt, err := c.get(ctx, "configmap", umamiV2SwitchoverNamespace, journalName, false)
	if err != nil || benchmarkIdentity(receipt) != benchmarkIdentity(journal) ||
		text(receipt, "data", "record") != text(journal, "data", "record") || text(receipt, "data", "verified") != "v2-primary" {
		return errors.New("the Umami switchover verification receipt has not been confirmed")
	}
	return nil
}

func verifyUmamiWriterService(ctx context.Context, c client, cluster, writer object) error {
	database := cnpgSource{Namespace: text(cluster, "metadata", "namespace"), Name: text(cluster, "metadata", "name")}
	refused := errors.New("the database writer service has not moved to the witnessed V2 writer")
	if _, ok := cnpgStandbyProfileForSource(database); !ok {
		return refused
	}
	service, err := c.get(ctx, "service", database.Namespace, database.Name+"-rw", false)
	if err != nil || !recoveredNativeObject(service, "v1", "Service", database.Namespace) || !cnpgOwnerReference(service, cluster) {
		return refused
	}
	slices, err := c.list(ctx, "endpointslices.discovery.k8s.io", database.Namespace)
	if err != nil {
		return err
	}
	return bindUmamiWriterEndpoints(service, writer, slices)
}

func bindUmamiWriterEndpoints(service, writer object, slices []object) error {
	refused := errors.New("the database writer service has an unknown or stale route")
	name, namespace := text(service, "metadata", "name"), text(service, "metadata", "namespace")
	if name == "" || namespace == "" {
		return refused
	}
	ports, _ := value(service, "spec", "ports").([]any)
	if len(ports) != 1 {
		return refused
	}
	port, _ := ports[0].(object)
	if !umamiPostgresPort(port) || value(port, "targetPort") != float64(5432) {
		return refused
	}
	families, ok := value(service, "spec", "ipFamilies").([]any)
	if !ok || len(families) < 1 || len(families) > 2 {
		return refused
	}
	expected := map[string]netip.Addr{}
	addresses, _ := value(writer, "status", "podIPs").([]any)
	for _, raw := range addresses {
		entry, _ := raw.(object)
		address, err := netip.ParseAddr(text(entry, "ip"))
		if err != nil || address.Zone() != "" || address.Is4In6() {
			return refused
		}
		family := "IPv6"
		if address.Is4() {
			family = "IPv4"
		}
		if expected[family].IsValid() {
			return refused
		}
		expected[family] = address
	}
	routed := map[string]bool{}
	for _, slice := range slices {
		if text(slice, "metadata", "labels", "kubernetes.io/service-name") != name {
			continue
		}
		if !recoveredNativeObject(slice, "discovery.k8s.io/v1", "EndpointSlice", namespace) {
			return refused
		}
		family := text(slice, "addressType")
		slicePorts, _ := value(slice, "ports").([]any)
		if len(slicePorts) != 1 || !expected[family].IsValid() {
			return refused
		}
		slicePort, _ := slicePorts[0].(object)
		if !umamiPostgresPort(slicePort) {
			return refused
		}
		refs, _ := value(slice, "metadata", "ownerReferences").([]any)
		if len(refs) != 1 {
			return refused
		}
		owner, _ := refs[0].(object)
		if text(owner, "apiVersion") != "v1" || text(owner, "kind") != "Service" ||
			text(owner, "name") != name || text(owner, "uid") != text(service, "metadata", "uid") || value(owner, "controller") != true {
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
				text(endpoint, "targetRef", "kind") != "Pod" || text(endpoint, "targetRef", "namespace") != namespace ||
				text(endpoint, "targetRef", "name") != text(writer, "metadata", "name") ||
				text(endpoint, "targetRef", "uid") != text(writer, "metadata", "uid") {
				return refused
			}
			ip, ok := value(endpoint, "addresses").([]any)
			if !ok || len(ip) != 1 {
				return refused
			}
			host, _ := ip[0].(string)
			address, err := netip.ParseAddr(host)
			if err != nil || address != expected[family] || routed[family] {
				return refused
			}
			routed[family] = true
		}
	}
	if len(routed) != len(families) {
		return refused
	}
	for _, raw := range families {
		family, _ := raw.(string)
		if !routed[family] {
			return refused
		}
		delete(routed, family)
	}
	return nil
}

func umamiPostgresPort(port object) bool {
	return text(port, "name") == "postgres" && text(port, "protocol") == "TCP" && value(port, "port") == float64(5432)
}
