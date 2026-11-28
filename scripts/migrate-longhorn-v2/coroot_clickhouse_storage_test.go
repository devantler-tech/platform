package main

import (
	"context"
	"encoding/json"
	"slices"
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
	for _, change := range []func(*corootClickHouseReplica){
		func(r *corootClickHouseReplica) { r.KeeperPath = "/different/table" },
		func(r *corootClickHouseReplica) { r.Replica = replica.Replica },
		func(r *corootClickHouseReplica) { r.ActiveReplicas = 1 },
		func(r *corootClickHouseReplica) { r.SessionExpired = "1" },
		func(r *corootClickHouseReplica) { r.PartsToCheck = "" },
		func(r *corootClickHouseReplica) { r.QueueErrorAbsent = 0 },
	} {
		candidate := other
		change(&candidate)
		reports[1] = []corootClickHouseReplica{candidate}
		if _, err := classifyCorootClickHouseStorage(members, before, reports); err == nil {
			t.Fatal("unverified replica evidence was accepted")
		}
	}
	reports[1] = []corootClickHouseReplica{other}
	before[1] = tableRows[:2]
	if _, err := classifyCorootClickHouseStorage(members, before, reports); err == nil {
		t.Fatal("changed table inventory was accepted")
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
