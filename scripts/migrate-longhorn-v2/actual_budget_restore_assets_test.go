package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestActualBudgetRestoreCopiesOnlyTheArchivedClaim(t *testing.T) {
	assets, err := actualBudgetRestoreAssets("91022", "90718")
	if err != nil || len(assets) != 4 {
		t.Fatalf("the isolated restore assets were not built: %v", err)
	}
	namespace := text(assets[0], "metadata", "name")
	if namespace == actualBudgetNamespace || text(assets[0], "kind") != "Namespace" ||
		text(assets[1], "kind") != "NetworkPolicy" || text(assets[1], "metadata", "namespace") != namespace ||
		!reflect.DeepEqual(value(assets[1], "spec", "policyTypes"), []any{"Ingress", "Egress"}) ||
		value(assets[1], "spec", "ingress") != nil || value(assets[1], "spec", "egress") != nil {
		t.Fatal("the restore requires an isolated namespace before it creates any storage")
	}
	modifiers, restore := assets[2], assets[3]
	if text(modifiers, "kind") != "ConfigMap" || text(restore, "kind") != "Restore" ||
		text(restore, "metadata", "namespace") != "velero" ||
		text(restore, "spec", "backupName") != "actual-budget-v1-90718" ||
		!reflect.DeepEqual(value(restore, "spec", "includedNamespaces"), []any{actualBudgetNamespace}) ||
		!reflect.DeepEqual(value(restore, "spec", "namespaceMapping"), object{actualBudgetNamespace: namespace}) ||
		!reflect.DeepEqual(value(restore, "spec", "includedResources"), []any{"persistentvolumeclaims"}) ||
		value(restore, "spec", "includeClusterResources") != false || value(restore, "spec", "restorePVs") != true ||
		text(restore, "spec", "existingResourcePolicy") != "none" ||
		text(restore, "spec", "resourceModifier", "name") != text(modifiers, "metadata", "name") {
		t.Fatal("the mover must restore only the archived claim into the exact V2 diagnostic namespace")
	}
	var document struct {
		Rules []struct {
			Conditions object   `json:"conditions"`
			Patches    []object `json:"patches"`
		} `json:"resourceModifierRules"`
	}
	if err := json.Unmarshal([]byte(text(modifiers, "data", "modifiers.json")), &document); err != nil {
		t.Fatal("invalid claim modifier", err)
	}
	rules := document.Rules
	if len(rules) != 1 || text(rules[0].Conditions, "groupResource") != "persistentvolumeclaims" ||
		text(rules[0].Conditions, "resourceNameRegex") != "^"+actualBudgetClaim+"$" {
		t.Fatal("the modifier must select only the backed up claim")
	}
	patches := rules[0].Patches
	if len(patches) != 1 || text(patches[0], "op") != "replace" ||
		text(patches[0], "path") != "/spec/storageClassName" || text(patches[0], "value") != "longhorn-v2" {
		t.Fatal("the modifier must preserve Velero's binding selector and only replace the StorageClass")
	}
}

func TestActualBudgetRestoreRefusesInvalidOrAliasedRuns(t *testing.T) {
	for _, pair := range [][2]string{{"", "21"}, {"22", "22"}, {"22", ""}, {"22", "../21"}, {"production", "21"}} {
		if result, err := actualBudgetRestoreAssets(pair[0], pair[1]); err == nil || result != nil {
			t.Fatalf("invalid source or restore run admitted: %q", pair)
		}
	}
}
