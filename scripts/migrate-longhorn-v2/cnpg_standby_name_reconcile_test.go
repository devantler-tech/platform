package main

import "testing"

func TestUmamiNameReconcileOnlyAddsItsOwnAnnotation(t *testing.T) {
	cluster := umamiV2TransitionObservation(t).Cluster
	cluster["apiVersion"], cluster["kind"] = "postgresql.cnpg.io/v1", "Cluster"
	metadata := valueObject(cluster, "metadata")
	metadata["generation"] = float64(1)
	metadata["uid"] = freshTestUID
	metadata["resourceVersion"] = "10"
	metadata["annotations"] = object{"tested.example.com/kept": "yes"}
	status := valueObject(cluster, "status")
	status["readyInstances"] = float64(3)
	status["phase"] = "Creating a new replica"
	status["phaseReason"] = "Creating replica umami-db-1-join"
	status["conditions"] = []any{object{"type": "ContinuousArchiving", "status": "True"}}
	owner := umamiReplacementOwner{Run: "45999990321", Source: "acef1536f21e1b8cf32d62c6045fc989fa8e55d6"}
	record := umamiStandbyReplacementRecord{
		Phase: umamiReplacementOriginalDetached, NameRelease: &umamiStandbyNameRelease{Owner: owner},
		Original: cnpgStandbyCutover{
			Consumer: benchmarkResource{Name: "umami-db-1", Namespace: "umami"}, Database: benchmarkIdentity(cluster),
		},
		Primary: benchmarkResource{Name: text(cluster, "status", "currentPrimary")},
	}
	changes, err := umamiStandbyReconcileChanges(cluster, record, owner)
	if err != nil || len(changes) != 3 || text(changes[2], "path") != "/metadata/annotations/storage.platform.devantler.tech~1umami-standby-reconcile" {
		t.Fatalf("expected an identity-bound annotation change: %v", err)
	}
	stamp := owner.Run + ":" + owner.Source
	changed := cloneCNPGObservation(t, cnpgObservation{Cluster: cluster}).Cluster
	valueObject(changed, "metadata", "annotations")[umamiStandbyReconcileAnnotation] = stamp
	if !verifiedUmamiStandbyReconcilePatch(cluster, changed, stamp) {
		t.Fatal("an annotation-only readback was rejected")
	}
	if again, err := umamiStandbyReconcileChanges(changed, record, owner); err != nil || len(again) != 0 {
		t.Fatal("replaying the same annotation update was not idempotent")
	}
	valueObject(changed, "metadata", "annotations")["tested.example.com/kept"] = "changed"
	if verifiedUmamiStandbyReconcilePatch(cluster, changed, stamp) {
		t.Fatal("admission changed unrelated annotations")
	}
	valueObject(changed, "metadata", "annotations")["tested.example.com/kept"] = "yes"
	valueObject(changed, "spec")["instances"] = float64(3)
	if verifiedUmamiStandbyReconcilePatch(cluster, changed, stamp) {
		t.Fatal("changed cluster spec was accepted")
	}
	status["phaseReason"] = "Creating replica umami-db-13-join"
	if _, err := umamiStandbyReconcileChanges(cluster, record, owner); err == nil {
		t.Fatal("the reconciliation event was allowed during a different replica's creation")
	}
}
