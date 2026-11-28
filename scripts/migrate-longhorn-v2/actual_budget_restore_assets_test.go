package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestActualBudgetRestoreCopiesOnlyTheArchivedClaim(t *testing.T) {
	assets, err := actualBudgetRestoreAssets("91022", "90718", "restore-worker")
	if err != nil || len(assets) != 4 {
		t.Fatalf("the isolated restore assets were not built: %v", err)
	}
	namespace := text(assets[0], "metadata", "name")
	policy := assets[1]
	if namespace == actualBudgetNamespace || text(assets[0], "kind") != "Namespace" ||
		text(policy, "kind") != "CiliumNetworkPolicy" || text(policy, "metadata", "namespace") != namespace ||
		!reflect.DeepEqual(value(policy, "spec", "endpointSelector", "matchLabels"), object{"storage.platform.devantler.tech/recovery-run": "91022"}) ||
		!reflect.DeepEqual(value(policy, "spec", "ingressDeny"), []any{object{"fromEntities": []any{"all"}}}) ||
		!reflect.DeepEqual(value(policy, "spec", "egressDeny"), []any{object{"toEntities": []any{"all"}}}) ||
		value(policy, "spec", "ingress") != nil || value(policy, "spec", "egress") != nil {
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
			Conditions object `json:"conditions"`
			Patches    []struct {
				Operation, Path, Value string
			} `json:"patches"`
		} `json:"resourceModifierRules"`
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(strings.NewReader(text(modifiers, "data", "modifiers.json")))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		t.Fatal("invalid claim modifier", err)
	}
	rules := document.Rules
	if len(rules) != 1 || text(rules[0].Conditions, "groupResource") != "persistentvolumeclaims" ||
		text(rules[0].Conditions, "resourceNameRegex") != "^"+actualBudgetClaim+"$" {
		t.Fatal("the modifier must select only the backed up claim")
	}
	patches := rules[0].Patches
	if len(patches) != 2 || patches[0].Operation != "replace" ||
		patches[0].Path != "/spec/storageClassName" || patches[0].Value != "longhorn-v2" ||
		patches[1].Operation != "add" || patches[1].Path != "/metadata/annotations/volume.kubernetes.io~1selected-node" ||
		patches[1].Value != "restore-worker" {
		t.Fatal("the modifier must keep Velero's binding selector and select the verified WFFC restore node")
	}
}

func TestActualBudgetRestoreRefusesInvalidOrAliasedRuns(t *testing.T) {
	for _, pair := range [][2]string{{"", "21"}, {"22", "22"}, {"22", ""}, {"22", "../21"}, {"production", "21"}} {
		if result, err := actualBudgetRestoreAssets(pair[0], pair[1], "restore-worker"); err == nil || result != nil {
			t.Fatalf("invalid source or restore run admitted: %q", pair)
		}
	}
}
