package main

import (
	"slices"
	"testing"
	"time"
)

func TestWeddingWriterGrantCannotTargetAnotherDatabase(t *testing.T) {
	const previous, target = "wedding-db-2", "wedding-db-4"
	workflow := migrationDispatchWorkflow(t)
	choices, _ := value(workflow, "on", "workflow_dispatch", "inputs", "operation", "options").([]any)
	if !slices.Contains(choices, any(weddingV2SwitchoverOperation)) {
		t.Fatal("the production workflow does not expose the reviewed Wedding switchover operation")
	}
	for _, fault := range []struct{ key, value string }{
		{}, {"GITHUB_RUN_ATTEMPT", "2"}, {"GITHUB_REF", "refs/heads/unsafe"}, {"LONGHORN_V2_DATABASE", "umami-db"},
		{"LONGHORN_V2_CONFIRM", weddingV2SwitchoverOperation + ":umami-db-1:" + target},
		{"LONGHORN_V2_CONFIRM", weddingV2SwitchoverOperation + ":" + target + ":" + target},
		{"LONGHORN_V2_OPERATION", replaceWeddingStandbyOperation},
	} {
		env := dispatchFixture()
		env["LONGHORN_V2_OPERATION"], env["LONGHORN_V2_NODE"] = weddingV2SwitchoverOperation, ""
		env["LONGHORN_V2_DATABASE"], env["LONGHORN_V2_CONFIRM"] = weddingStandbySource.Name, weddingV2SwitchoverOperation+":"+previous+":"+target
		if fault.key != "" {
			env[fault.key] = fault.value
		}
		old, newWriter, err := authorizeCNPGStandbyTarget(func(key string) string { return env[key] }, weddingV2SwitchoverOperation, weddingStandbySource.Name)
		if fault.key == "" && (err != nil || old != previous || newWriter != target) || fault.key != "" && err == nil {
			t.Fatalf("unexpected Wedding switchover authorization for %s: %v", fault.key, err)
		}
	}
}

func TestWeddingWriterUpdateRequiresItsExactOwnedCluster(t *testing.T) {
	const previous, target = "wedding-db-2", "wedding-db-4"
	fresh := object{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": object{"name": "wedding-db", "namespace": "wedding-app", "uid": freshTestUID, "resourceVersion": "41"},
		"status": object{"currentPrimary": previous, "targetPrimary": previous, "phase": "Cluster in healthy state",
			"conditions": []any{object{"type": "Ready", "status": "True"}, object{"type": "ContinuousArchiving", "status": "True"}}}}
	patch, err := cnpgV2SwitchoverPatch(weddingStandbySource, fresh, previous, target, time.Unix(1795789980, 0))
	if err != nil || len(patch) != 7 || text(patch[0], "value") != freshTestUID || text(patch[1], "value") != "41" || text(patch[2], "value") != target {
		t.Fatalf("the status mutation did not protect the exact Wedding writer: %v", err)
	}
	valueObject(fresh, "metadata")["namespace"] = "umami"
	if _, err := cnpgV2SwitchoverPatch(weddingStandbySource, fresh, previous, target, time.Unix(1795789980, 0)); err == nil {
		t.Fatal("a cluster with the right name in another namespace accepted the transition")
	}
}
