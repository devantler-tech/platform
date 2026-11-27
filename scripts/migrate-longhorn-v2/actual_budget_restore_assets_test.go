package main

import (
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
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
	// Velero v1.18 decodes the resource modifier strictly and translates
	// operation to op only when constructing the RFC 6902 patch.
	type modifierPatch struct {
		Operation string `json:"operation"`
		Path      string `json:"path"`
		Value     string `json:"value"`
	}
	var document struct {
		Version string `json:"version"`
		Rules   []struct {
			Conditions object          `json:"conditions"`
			Patches    []modifierPatch `json:"patches"`
		} `json:"resourceModifierRules"`
	}
	if err := yaml.UnmarshalStrict([]byte(text(modifiers, "data", "modifiers.json")), &document); err != nil {
		t.Fatalf("Velero would reject the claim modifier: %v", err)
	}
	rules := document.Rules
	if document.Version != "v1" || len(rules) != 1 || text(rules[0].Conditions, "groupResource") != "persistentvolumeclaims" ||
		text(rules[0].Conditions, "resourceNameRegex") != "^"+actualBudgetClaim+"$" {
		t.Fatal("the modifier must select only the backed up claim")
	}
	wantPatches := []modifierPatch{
		{Operation: "replace", Path: "/spec/storageClassName", Value: "longhorn-v2"},
		{Operation: "add", Path: "/metadata/annotations/volume.kubernetes.io~1selected-node", Value: "restore-worker"},
	}
	if !reflect.DeepEqual(rules[0].Patches, wantPatches) {
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
