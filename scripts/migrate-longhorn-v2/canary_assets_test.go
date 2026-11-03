package main

import (
	"encoding/json"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"sigs.k8s.io/yaml"
)

func TestCanaryRestoresOnlyItsOwnFilesystemDataOntoV2(t *testing.T) {
	assets, err := canaryResources("1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 11 {
		t.Fatalf("incomplete isolated canary assets: %d", len(assets))
	}
	var backup, restore, modifier object
	classes := map[string]object{}
	for _, o := range assets {
		switch text(o, "kind") {
		case "Backup":
			backup = o
		case "Restore":
			restore = o
		case "ConfigMap":
			modifier = o
		case "StorageClass":
			classes[text(o, "parameters", "dataEngine")] = o
		case "Pod":
			if text(o, "spec", "nodeSelector", "kubernetes.io/hostname") != "prod-worker-3" || value(o, "spec", "automountServiceAccountToken") != false || text(o, "metadata", "annotations", "backup.velero.io/backup-volumes") != "data" {
				t.Fatal("probe must exercise the remote V2 client and back up only persistent data")
			}
		}
	}
	if len(classes) != 2 || text(classes["v2"], "parameters", "numberOfReplicas") != "3" || text(classes["v2"], "reclaimPolicy") != "Retain" {
		t.Fatal("canary storage is not independent retained three-copy storage")
	}
	if text(classes["v2"], "parameters", "unmapMarkSnapChainRemoved") != "disabled" {
		t.Fatal("V2 probe must not inherit unsupported trim behavior")
	}
	if value(backup, "spec", "snapshotVolumes") != false || value(backup, "spec", "defaultVolumesToFsBackup") != false || value(backup, "spec", "includeClusterResources") != false || value(restore, "spec", "includeClusterResources") != false {
		t.Fatal("probe might clone a V1 snapshot or restore cluster resources")
	}
	if text(restore, "spec", "backupName") != text(backup, "metadata", "name") || text(restore, "spec", "resourceModifier", "name") != text(modifier, "metadata", "name") || text(restore, "spec", "namespaceMapping", "longhorn-canary-v1-1234") != "longhorn-canary-v2-1234" {
		t.Fatal("restore escaped the dedicated source and destination")
	}
	rules := text(modifier, "data", "rules.yaml")
	var native struct {
		ResourceModifierRules []struct {
			Patches []struct{ Operation, Path, Value string }
		}
	}
	if err := yaml.Unmarshal([]byte(rules), &native); err != nil {
		t.Fatal("modifier cannot decode Velero's native string-valued JSON patches:", err)
	}
	// The backup excludes PVs, so the modifier receives a claim still bound to
	// its V1 source PV. Apply the emitted RFC 6902 operations to that real shape.
	var operations []object
	for _, patch := range native.ResourceModifierRules[0].Patches {
		op := object{"op": patch.Operation, "path": patch.Path}
		if patch.Value != "" {
			op["value"] = patch.Value
		}
		operations = append(operations, op)
	}
	encoded, err := json.Marshal(operations)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := jsonpatch.DecodePatch(encoded)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := patch.Apply([]byte(`{"spec":{"storageClassName":"lh-canary-v1-1234","volumeName":"source-v1-pv"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var claim object
	if err := json.Unmarshal(restored, &claim); err != nil {
		t.Fatal(err)
	}
	if value(claim, "spec", "volumeName") != nil || text(claim, "spec", "storageClassName") != "lh-canary-v2-1234" {
		t.Fatal("restored claim retains its source PV binding instead of provisioning V2 storage")
	}
	for _, required := range []string{"longhorn-canary-v1-1234", "^fio-data$", "^fio$", "/spec/storageClassName", "lh-canary-v2-1234", "operation: test"} {
		if !strings.Contains(rules, required) {
			t.Fatal("restore modifier lacks exact source or destination binding")
		}
	}
	if strings.Contains(canaryScript, "curl ") || !strings.Contains(canaryScript, "while [ ! -f /tmp/canary-start ]") || !strings.Contains(canaryScript, "--verify=sha256") {
		t.Fatal("probe does not wait for verified provisioning or verify persisted bytes")
	}
	if _, err = canaryResources("../other"); err == nil {
		t.Fatal("unbounded canary identity accepted")
	}
}

func TestCompletedBackupCannotStandInForRestoredData(t *testing.T) {
	b := resource("canary")
	b["status"] = object{"phase": "Completed", "completionTimestamp": "2026-10-29T19:00:00Z", "errors": float64(0), "warnings": float64(0)}
	if cleanVeleroResult(b) != nil {
		t.Fatal("clean native completion rejected")
	}
	for _, field := range []string{"errors", "warnings"} {
		b["status"].(object)[field] = float64(1)
		if cleanVeleroResult(b) == nil {
			t.Fatal("partial or warned backup accepted")
		}
		b["status"].(object)[field] = float64(0)
	}
	b["status"].(object)["phase"] = "PartiallyFailed"
	if cleanVeleroResult(b) == nil {
		t.Fatal("partial native completion accepted")
	}
	if cleanVeleroResult(object{}) == nil {
		t.Fatal("missing evidence accepted")
	}
	b["status"].(object)["phase"] = "Completed"
	delete(b["status"].(object), "warnings")
	delete(b["status"].(object), "errors")
	if cleanVeleroResult(b) != nil {
		t.Fatal("native omitempty zero counters rejected")
	}
}
