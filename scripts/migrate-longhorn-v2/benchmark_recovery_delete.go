package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
)

// This first recovery stage retires only the run's recorded Pod or ingress/
// egress policy. Volume, class and namespace retirement need their own complete
// binding and foreign-consumer checks before they can enter the procedure.
func removeRecoveredBenchmarkWorkload(ctx context.Context, c client, record *benchmarkRecord, kind string) error {
	if record.Recovery == nil || (kind != "Pod" && kind != "NetworkPolicy") {
		return errors.New("recovery workload scope is unverified")
	}
	identity, known := record.Resources[kind]
	if !known || identity.UID == "" || identity.Namespace != record.Namespace {
		return errors.New("recovery workload has no original UID receipt")
	}
	resource, suffix := "pod", "pods/fio"
	if kind == "NetworkPolicy" {
		resource, suffix = "networkpolicy", "networkpolicies/fio-default-deny"
	}
	if (kind == "Pod" && identity.Name != "fio") || (kind == "NetworkPolicy" && identity.Name != "fio-default-deny") || safeKubeRequest(resource, record.Namespace, identity.Name, false) != nil {
		return errors.New("recovery workload target differs from the reviewed scope")
	}
	path := "/api/v1/namespaces/" + record.Namespace + "/" + suffix
	if kind == "NetworkPolicy" {
		path = "/apis/networking.k8s.io/v1/namespaces/" + record.Namespace + "/" + suffix
	}
	current, err := c.get(ctx, resource, record.Namespace, identity.Name, true)
	if err != nil {
		return err
	}
	if current == nil && record.Recovery.Deletes[kind] != identity {
		return errors.New("workload disappeared without this recovery's durable deletion intent")
	}
	if current != nil && text(current, "metadata", "uid") != identity.UID {
		return errors.New("recovery workload was replaced")
	}
	// Reassert ownership through journal UID/version CAS before every delete,
	// including a retry whose previous API response was lost.
	next := *record
	owner := *record.Recovery
	owner.Deletes = maps.Clone(owner.Deletes)
	if owner.Deletes == nil {
		owner.Deletes = map[string]benchmarkResource{}
	}
	owner.Deletes[kind] = identity
	next.Recovery = &owner
	if err := saveBenchmarkRecoveryProgress(ctx, c, *record, next); err != nil {
		return err
	}
	*record = next
	if current == nil {
		checked, err := c.get(ctx, resource, record.Namespace, identity.Name, true)
		if err != nil {
			return err
		}
		if checked != nil {
			return errors.New("workload changed while its absence was being verified")
		}
		return nil
	}
	return removeScratch(ctx, c, record.Recovery.Source, resource, record.Namespace, path, current)
}

func saveBenchmarkRecoveryProgress(ctx context.Context, c client, previous, next benchmarkRecord) error {
	if previous.Recovery == nil || next.Recovery == nil {
		return errors.New("recovery owner is missing")
	}
	a, b := previous, next
	a.Recovery, b.Recovery = nil, nil
	if !reflect.DeepEqual(a, b) || previous.Recovery.Source != next.Recovery.Source || previous.Recovery.Run != next.Recovery.Run || previous.Recovery.JournalUID != next.Recovery.JournalUID {
		return errors.New("recovery cannot rewrite original evidence or ownership")
	}
	for kind, receipt := range previous.Recovery.Deletes {
		if next.Recovery.Deletes[kind] != receipt {
			return errors.New("recovery cannot discard a prior deletion intent")
		}
	}
	for kind, receipt := range next.Recovery.Deletes {
		if (kind != "Pod" && kind != "NetworkPolicy") || receipt.UID == "" || next.Resources[kind] != receipt {
			return errors.New("recovery deletion intent is outside its original UID receipts")
		}
	}
	cm, current, err := readBenchmark(ctx, c, previous.Phase)
	if err != nil || cm == nil || text(cm, "metadata", "uid") != previous.Recovery.JournalUID || !reflect.DeepEqual(current, previous) {
		return errors.New("recovery journal changed before progress could be saved")
	}
	if err := benchmarkOwnerFinished(ctx, c, previous.Run, previous.Source); err != nil {
		return err
	}
	if err := sourceCurrent(ctx, c.run, previous.Recovery.Source); err != nil {
		return err
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := c.patch(ctx, "configmap", "longhorn-system", cm, object{"op": "replace", "path": "/data/record", "value": string(body)}); err != nil {
		return errors.New("recovery progress was not confirmed; preserve the journal")
	}
	checkedCM, checked, err := readBenchmark(ctx, c, previous.Phase)
	if err != nil || text(checkedCM, "metadata", "uid") != previous.Recovery.JournalUID || !reflect.DeepEqual(checked, next) {
		return errors.New("recovery progress readback changed")
	}
	return nil
}
