package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCorootClickHouseStorageEvidenceJoinsBothReplicas(t *testing.T) {
	const tables = `{"Database":"app","Name":"traces","Engine":"ReplicatedMergeTree"}
{"Database":"app","Name":"local_history","Engine":"MergeTree"}
{"Database":"app","Name":"report","Engine":"View"}`
	tableRows, err := readClickHouseStorageRows[corootClickHouseTable]([]byte(tables))
	if err != nil {
		t.Fatal(err)
	}
	replica := corootClickHouseReplica{
		Database: "app", Name: "traces", Engine: "ReplicatedMergeTree", Replica: "coroot-clickhouse-shard-0-0",
		KeeperPath: "/test/shard/traces", ReadOnly: "0", SessionExpired: "0", PartsToCheck: "0",
		ActiveReplicas: 2, TotalReplicas: 2, DelaySeconds: "0", QueueErrorAbsent: 1, KeeperErrorAbsent: 1,
	}
	other := replica
	other.Replica = "coroot-clickhouse-shard-0-1"
	other.DelaySeconds = "62"
	members := [2]string{replica.Replica, other.Replica}
	before := [2][]corootClickHouseTable{tableRows, tableRows}
	reports := [2][]corootClickHouseReplica{{replica}, {other}}
	got, err := classifyCorootClickHouseStorage(members, before, reports)
	if err != nil || got != (corootClickHouseStorageSummary{Tables: 3, Replicated: 1, Local: 1, Views: 1, MaxReplicaDelaySeconds: 62}) {
		t.Fatalf("matched data: got %+v, %v", got, err)
	}
	for _, test := range []struct {
		reason string
		change func(*corootClickHouseReplica)
	}{
		{"Keeper paths", func(r *corootClickHouseReplica) { r.KeeperPath = "/different/table" }},
		{"replica identity", func(r *corootClickHouseReplica) { r.Replica = replica.Replica }},
		{"active replicas", func(r *corootClickHouseReplica) { r.ActiveReplicas = 1 }},
		{"total replicas", func(r *corootClickHouseReplica) { r.TotalReplicas = 3 }},
		{"read-only replica", func(r *corootClickHouseReplica) { r.ReadOnly = "1" }},
		{"Keeper session", func(r *corootClickHouseReplica) { r.SessionExpired = "1" }},
		{"parts pending verification", func(r *corootClickHouseReplica) { r.PartsToCheck = "" }},
		{"replica delay observation", func(r *corootClickHouseReplica) { r.DelaySeconds = "unknown" }},
		{"replication queue exception", func(r *corootClickHouseReplica) { r.QueueErrorAbsent = 0 }},
		{"Keeper exception", func(r *corootClickHouseReplica) { r.KeeperErrorAbsent = 0 }},
	} {
		candidate := other
		test.change(&candidate)
		reports[1] = []corootClickHouseReplica{candidate}
		_, err := classifyCorootClickHouseStorage(members, before, reports)
		if !errors.Is(err, errCorootClickHouseStorage) || !strings.Contains(err.Error(), test.reason) ||
			strings.Contains(err.Error(), candidate.Name) || strings.Contains(err.Error(), candidate.KeeperPath) {
			t.Fatalf("expected the sanitized %q signal: %v", test.reason, err)
		}
	}
	reports[1] = []corootClickHouseReplica{other}
	before[1] = tableRows[:2]
	if _, err := classifyCorootClickHouseStorage(members, before, reports); !errors.Is(err, errCorootClickHouseStorage) ||
		!strings.Contains(err.Error(), "table catalogs differ") || strings.Contains(err.Error(), "local_history") {
		t.Fatalf("the rejection should identify only the catalog mismatch: %v", err)
	}
}

func TestCorootClickHouseStorageRejectsUnreadableRows(t *testing.T) {
	const absent = `{"Database":"app","Name":"traces","Engine":"ReplicatedMergeTree"}`
	for _, raw := range []string{"", absent + "{}", absent + "\nnull", absent + "\n{", absent[:len(absent)-1] + `,"extra":11}`} {
		if _, err := readClickHouseStorageRows[corootClickHouseTable]([]byte(raw)); err == nil {
			t.Errorf("unreadable rows were accepted: %q", raw)
		}
	}
	encoded, err := json.Marshal(corootClickHouseTable{Database: "app", Name: "traces", Engine: "ReplicatedMergeTree"})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := readClickHouseStorageRows[corootClickHouseTable](encoded); err != nil || len(rows) != 1 {
		t.Fatalf("single row failed: %v", err)
	}
}

func TestCorootClickHouseCatalogUsesOnlyFixedReadOnlyQueries(t *testing.T) {
	pod := benchmarkResource{
		Name: corootClickHouseController + "-0", Namespace: corootClickHouseNamespace,
		UID: "e9d399c5-9614-41c0-bc1a-e07fb34bce02",
	}
	member := corootClickHouseSource{Pod: pod}
	calls := 0
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		calls++
		if input != nil || program != "kubectl" || !slices.Contains(args, "--readonly=1") ||
			!slices.Contains(args, "--host=127.0.0.1") || !slices.Contains(args, "--container="+corootClickHouseContainer) ||
			!slices.Contains(args, "--namespace="+corootClickHouseNamespace) ||
			!slices.Contains(args, member.Pod.Name) ||
			!slices.Contains(args, "/usr/bin/clickhouse") || slices.Contains(args, "--password") {
			t.Fatalf("an unrecognized or unbound catalog command ran: %s %v", program, args)
		}
		return []byte("fixture"), nil
	}}
	for _, query := range []string{corootClickHouseTablesQuery, corootClickHouseReplicasQuery} {
		if raw, err := c.readCorootClickHouseCatalog(t.Context(), member, query); err != nil || string(raw) != "fixture" {
			t.Fatalf("fixed catalog query refused: %v", err)
		}
	}
	if _, err := c.readCorootClickHouseCatalog(t.Context(), member, "SELECT version()"); err == nil {
		t.Fatal("an undeclared query was accepted")
	}
	member.Pod.Name = "other-app-0"
	if _, err := c.readCorootClickHouseCatalog(t.Context(), member, corootClickHouseTablesQuery); err == nil {
		t.Fatal("an unrelated pod was accepted")
	}
	if calls != 2 {
		t.Fatalf("the command runner observed %d calls, want two fixed queries", calls)
	}
}

func TestCorootClickHouseSnapshotBindsTheRunningProcess(t *testing.T) {
	status := object{
		"name": corootClickHouseContainer, "ready": true, "imageID": corootClickHouseImage,
		"containerID":  "containerd://1111111111111111111111111111111111111111111111111111111111111111",
		"restartCount": float64(2),
		"state":        object{"running": object{"startedAt": "2026-11-28T01:05:00Z"}},
	}
	pod := object{"status": object{"containerStatuses": []any{status}}}
	original, ok := corootClickHouseContainerProcess(pod)
	if !ok || original.Restarts != 2 {
		t.Fatal("the current ready process was not bound")
	}
	before := corootClickHouseSourceSet{Members: [2]corootClickHouseSource{{Process: original}}}
	status["containerID"] = "containerd://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	status["restartCount"] = float64(3)
	valueObject(status, "state", "running")["startedAt"] = "2026-11-28T01:05:43Z"
	restarted, ok := corootClickHouseContainerProcess(pod)
	after := corootClickHouseSourceSet{Members: [2]corootClickHouseSource{{Process: restarted}}}
	if !ok || before == after {
		t.Fatal("a replacement process could satisfy the original source snapshot")
	}
	delete(status, "containerID")
	if _, ok := corootClickHouseContainerProcess(pod); ok {
		t.Fatal("a source without its running container identity was accepted")
	}
}

func TestCorootClickHouseSourceRequiresRetainedPV(t *testing.T) {
	const (
		podUID   = "11111111-1111-4111-8111-111111111111"
		claimUID = "22222222-2222-4222-8222-222222222222"
		pvUID    = "33333333-3333-4333-8333-333333333333"
		node     = "storage-worker"
		revision = "rendered-revision"
	)
	controller := benchmarkResource{Name: corootClickHouseController, Namespace: corootClickHouseNamespace, UID: "44444444-4444-4444-8444-444444444444"}
	member, pvName := controller.Name+"-0", "pvc-"+claimUID
	claimName := "data-" + member
	pod := object{"apiVersion": "v1", "kind": "Pod",
		"metadata": object{"name": member, "namespace": controller.Namespace, "uid": podUID, "resourceVersion": "1",
			"labels": object{"controller-revision-hash": revision}, "ownerReferences": []any{
				object{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": controller.Name, "uid": controller.UID, "controller": true},
			}},
		"spec": object{"nodeName": node, "volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": claimName}}},
			"containers": []any{object{"name": corootClickHouseContainer, "volumeMounts": []any{object{"name": "data", "mountPath": "/var/lib/clickhouse"}}}}},
		"status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}},
			"containerStatuses": []any{object{"name": corootClickHouseContainer, "ready": true, "imageID": corootClickHouseImage,
				"containerID": "containerd://1111111111111111111111111111111111111111111111111111111111111111", "restartCount": float64(0),
				"state": object{"running": object{"startedAt": "2026-11-28T01:05:00Z"}}}}},
	}
	claim := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": object{"name": claimName, "namespace": controller.Namespace, "uid": claimUID, "resourceVersion": "1"},
		"spec": object{"volumeName": pvName, "storageClassName": "hcloud", "volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"},
			"resources": object{"requests": object{"storage": "20Gi"}}},
		"status": object{"phase": "Bound", "capacity": object{"storage": "35Gi"}}}
	pv := object{"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": object{"name": pvName, "uid": pvUID, "resourceVersion": "1"},
		"spec": object{"storageClassName": "hcloud", "volumeMode": "Filesystem", "accessModes": []any{"ReadWriteOnce"},
			"capacity": object{"storage": "35Gi"}, "csi": object{"driver": hetznerCSIDriver, "volumeHandle": "1234"},
			"claimRef": object{"namespace": controller.Namespace, "name": claimName, "uid": claimUID}},
		"status": object{"phase": "Bound"}}
	attachment := object{"apiVersion": "storage.k8s.io/v1", "kind": "VolumeAttachment",
		"metadata": object{"name": "csi-attached-1234", "uid": "55555555-5555-4555-8555-555555555555", "resourceVersion": "1"},
		"spec":     object{"source": object{"persistentVolumeName": pvName}, "attacher": hetznerCSIDriver, "nodeName": node},
		"status":   object{"attached": true}}
	resources := map[string]object{"pod": pod, "persistentvolumeclaim": claim, "persistentvolume": pv}
	c := client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || program != "kubectl" || len(args) < 5 || args[2] != "get" || resources[args[3]] == nil {
			t.Fatalf("unexpected native read: %s %v", program, args)
		}
		return json.Marshal(resources[args[3]])
	}}
	for _, policy := range []string{"Retain", "Delete", ""} {
		valueObject(pv, "spec")["persistentVolumeReclaimPolicy"] = policy
		_, err := readCorootClickHouseSource(t.Context(), c, controller, revision, 0, []object{attachment})
		if (err == nil) != (policy == "Retain") {
			t.Errorf("policy %q: got %v, want success only for Retain", policy, err)
		}
	}
}
