package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNativeRestorePreparesIndependentV2StorageAndRestoresOnlyTheOriginalPod(t *testing.T) {
	r, seed := seedStateFixture(t, "source-cleaned")
	assets, err := nativeRestoreAssets(r, seed, "5678", nativeReattachFixture().Targets)
	if err != nil || len(assets) != 5 {
		t.Fatal("the verified source cannot prepare the separate restore:", err)
	}
	byKind := map[string]object{}
	for _, asset := range assets {
		kind := text(asset, "kind")
		if byKind[kind] != nil || text(asset, "metadata", "labels", "storage.platform.devantler.tech/benchmark-run") != "5678" {
			t.Fatal("restore asset identity is ambiguous")
		}
		byKind[kind] = asset
	}
	class, claim, restore, config := byKind["StorageClass"], byKind["PersistentVolumeClaim"], byKind["Restore"], byKind["ConfigMap"]
	policy := byKind["NetworkPolicy"]
	if text(policy, "apiVersion") != "networking.k8s.io/v1" || text(policy, "metadata", "namespace") != benchmarkNamespace ||
		text(policy, "metadata", "name") != "fio-default-deny" ||
		!reflect.DeepEqual(value(policy, "spec"), object{"podSelector": object{}, "policyTypes": []any{"Ingress", "Egress"}}) {
		t.Fatal("the separately restored Pod lost the source's complete network isolation")
	}
	if text(class, "parameters", "dataEngine") != "v2" || text(class, "parameters", "numberOfReplicas") != "3" ||
		text(class, "parameters", "nodeSelector") != "longhorn-native-v2-5678" || text(class, "reclaimPolicy") != "Retain" ||
		text(class, "volumeBindingMode") != "Immediate" || text(class, "parameters", "fromBackup") != "" ||
		text(claim, "metadata", "namespace") != benchmarkNamespace || text(claim, "metadata", "name") != "fio-data" ||
		text(claim, "spec", "storageClassName") != text(class, "metadata", "name") ||
		text(claim, "spec", "resources", "requests", "storage") != "4Gi" || value(claim, "spec", "volumeName") != nil ||
		value(claim, "spec", "dataSource") != nil || value(claim, "spec", "dataSourceRef") != nil {
		t.Fatal("restore storage can reuse the source or escape the qualified V2 targets")
	}
	if text(restore, "spec", "backupName") != seed.Backup.Name || !reflect.DeepEqual(value(restore, "spec", "includedResources"), []any{"pods"}) ||
		!reflect.DeepEqual(value(restore, "spec", "excludedResources"), []any{"persistentvolumeclaims", "persistentvolumes"}) ||
		!reflect.DeepEqual(value(restore, "spec", "includedNamespaces"), []any{benchmarkNamespace}) ||
		value(restore, "spec", "restorePVs") != false || value(restore, "spec", "includeClusterResources") != false ||
		text(restore, "spec", "existingResourcePolicy") != "none" ||
		text(restore, "spec", "labelSelector", "matchLabels", "storage.platform.devantler.tech/benchmark-run") != r.Run ||
		text(restore, "spec", "resourceModifier", "kind") != "ConfigMap" ||
		text(restore, "spec", "resourceModifier", "name") != text(config, "metadata", "name") ||
		text(config, "metadata", "namespace") != "velero" || text(restore, "metadata", "namespace") != "velero" {
		t.Fatal("restore can replace storage, select another backup or overwrite an existing object")
	}
	var modifiers object
	if err = json.Unmarshal([]byte(text(config, "data", "modifiers.json")), &modifiers); err != nil || text(modifiers, "version") != "v1" {
		t.Fatal("restore modifiers are not valid native input:", err)
	}
	rules, _ := value(modifiers, "resourceModifierRules").([]any)
	if len(rules) != 1 {
		t.Fatal("restore modifiers select more than the one original Pod")
	}
	rule := rules[0].(object)
	if text(rule, "conditions", "groupResource") != "pods" || text(rule, "conditions", "resourceNameRegex") != "^fio$" ||
		!reflect.DeepEqual(value(rule, "conditions", "namespaces"), []any{benchmarkNamespace}) ||
		text(rule, "conditions", "labelSelector", "matchLabels", "storage.platform.devantler.tech/benchmark-run") != r.Run {
		t.Fatal("modifiers escaped the original backed-up Pod")
	}
	patches, _ := value(rule, "mergePatches").([]any)
	var patch object
	if len(patches) != 1 || json.Unmarshal([]byte(text(patches[0].(object), "patchData")), &patch) != nil ||
		!reflect.DeepEqual(value(patch, "spec", "nodeSelector"), object{"kubernetes.io/hostname": "prod-worker-1", migrationPoolLabel: nil}) {
		t.Fatal("the first consumer is not bound to its qualified node roster")
	}
}

func TestNativeRestoreRequiresThreeDistinctQualifiedNodeIdentities(t *testing.T) {
	for _, fault := range []string{"no roster", "missing target", "extra target", "missing UID", "borrowed UID", "invalid name", "control in UID"} {
		t.Run(fault, func(t *testing.T) {
			r, seed := seedStateFixture(t, "source-cleaned")
			targets := nativeReattachFixture().Targets
			switch fault {
			case "no roster":
				targets = nil
			case "missing target":
				delete(targets, "prod-worker-longhorn-v2-1")
			case "extra target":
				targets["other"] = "node-uid-d"
			case "missing UID":
				targets["prod-worker-1"] = ""
			case "borrowed UID":
				targets["prod-worker-1"] = targets["prod-worker-2"]
			case "invalid name":
				delete(targets, "prod-worker-1")
				targets["../other"] = "node-uid-a"
			case "control in UID":
				targets["prod-worker-1"] = "node\nuid"
			}
			if assets, err := nativeRestoreAssets(r, seed, "5678", targets); err == nil || assets != nil {
				t.Fatal("restore produced assets without its qualified three-node roster")
			}
		})
	}
}

func TestNativeRestoreRefusesIncompleteOrBorrowedSeedEvidence(t *testing.T) {
	for _, fault := range []string{"unfinished source", "lost snapshot", "other workload", "claimed benchmark", "borrowed measurements", "borrowed completion", "old run", "bad run"} {
		t.Run(fault, func(t *testing.T) {
			r, seed := seedStateFixture(t, "source-cleaned")
			run := "5678"
			switch fault {
			case "unfinished source":
				r, seed = seedStateFixture(t, "backup-verified")
			case "lost snapshot":
				seed.Snapshot = ""
			case "other workload":
				r.Workload = strings.Repeat("0", 64)
			case "claimed benchmark":
				r.Cleaned = true
			case "borrowed measurements":
				r.Measurements = []fioMeasurement{{}}
			case "borrowed completion":
				r.CompletedAt = seed.CompletedAt
			case "old run":
				run = r.Run
			case "bad run":
				run = "1234/../../other"
			}
			if assets, err := nativeRestoreAssets(r, seed, run, nativeReattachFixture().Targets); err == nil || assets != nil {
				t.Fatal("an unqualified restore produced assets:", fault, err)
			}
		})
	}
}
