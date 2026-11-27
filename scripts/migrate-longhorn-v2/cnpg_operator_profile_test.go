package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// cnpgSchemaFixture uses an explicitly synthetic pin, never the deployment pin.
func cnpgSchemaFixture() (object, string) {
	o := object{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": object{"name": "clusters.postgresql.cnpg.io", "uid": "schema-uid", "resourceVersion": "10", "generation": 1}, "spec": object{"profile": "synthetic"}, "status": object{"storedVersions": []any{"v1"}, "conditions": []any{object{"type": "Established", "status": "True"}, object{"type": "NamesAccepted", "status": "True"}}}}
	return o, fmt.Sprintf("%x", sha256.Sum256([]byte(`{"profile":"synthetic"}`)))
}

// TestCNPGSchemaBindingRequiresThePinnedDefinitionAndEstablishedNativeVersion checks its full input.
func TestCNPGSchemaBindingRequiresThePinnedDefinitionAndEstablishedNativeVersion(t *testing.T) {
	o, pin := cnpgSchemaFixture()
	raw, _ := json.Marshal(o)
	binding, err := bindCNPGCRD(raw, pin)
	if err != nil || binding.UID != "schema-uid" || binding.Generation != 1 || binding.SpecSHA != pin {
		t.Fatalf("complete synthetic schema binding failed: %+v %v", binding, err)
	}
	if _, err = bindCNPGCRD(raw, cnpgReviewedCRDSHA); err == nil {
		t.Fatal("synthetic schema satisfied the real released pin")
	}
}

// TestCNPGSchemaBindingRejectsIncompleteOrChangedNativeDefinitions prevents schema drift.
func TestCNPGSchemaBindingRejectsIncompleteOrChangedNativeDefinitions(t *testing.T) {
	for name, mutate := range map[string]func(object){
		"kind":          func(o object) { o["kind"] = "ConfigMap" },
		"identity":      func(o object) { o["metadata"].(object)["uid"] = "" },
		"revision":      func(o object) { delete(o["metadata"].(object), "resourceVersion") },
		"generation":    func(o object) { o["metadata"].(object)["generation"] = 0 },
		"deleting":      func(o object) { o["metadata"].(object)["deletionTimestamp"] = "2026-11-03T10:00:00Z" },
		"schema":        func(o object) { o["spec"].(object)["profile"] = "changed" },
		"unestablished": func(o object) { value(o, "status", "conditions").([]any)[0].(object)["status"] = "False" },
		"ambiguous conditions": func(o object) {
			value(o, "status").(object)["conditions"] = append(value(o, "status", "conditions").([]any), object{"type": "Established", "status": "True"})
		},
		"stored version": func(o object) { value(o, "status").(object)["storedVersions"] = []any{"v1beta1"} },
	} {
		t.Run(name, func(t *testing.T) {
			o, pin := cnpgSchemaFixture()
			mutate(o)
			raw, _ := json.Marshal(o)
			if binding, err := bindCNPGCRD(raw, pin); err == nil || binding != (cnpgCRDBinding{}) {
				t.Fatal("incomplete schema returned a binding")
			}
		})
	}
}

// cnpgStartupFixture models the three current-process startup records.
func cnpgStartupFixture() ([]object, string) {
	return []object{
		{"msg": "Starting CloudNativePG Operator", "ts": "2026-11-03T10:14:19.62Z", "version": "1.30.2", "build": object{"Version": "1.30.2", "Commit": "30cca36d3"}},
		{"msg": "Loading configuration from ConfigMap", "ts": "2026-11-03T10:14:19.64Z", "namespace": "cnpg-system", "name": "cnpg-controller-manager-config"},
		{"msg": "Operator configuration loaded", "ts": "2026-11-03T10:14:19.68Z", "configuration": object{"operatorImageName": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.2", "operatorNamespace": "cnpg-system", "monitoringQueriesConfigmap": "cnpg-default-monitoring", "monitoringQueriesSecret": "", "includePlugins": "", "operatorPullSecretName": "cnpg-pull-secret", "createAnyService": false}},
	}, "2026-11-03T10:14:19Z"
}

func TestCNPGStartupRejectsUnboundGeneratedResourceSettings(t *testing.T) {
	for _, field := range []string{"operatorPullSecretName", "createAnyService"} {
		for _, invalid := range []any{nil, "private-canary", float64(0)} {
			records, started := cnpgStartupFixture()
			records[2]["configuration"].(object)[field] = invalid
			if binding, err := bindCNPGOperatorStartup(encodeStartupFixture(t, records), started); err == nil || binding != (cnpgStartupBinding{}) {
				t.Fatalf("unqualified generated-resource setting %s accepted", field)
			}
		}
		records, started := cnpgStartupFixture()
		delete(records[2]["configuration"].(object), field)
		if binding, err := bindCNPGOperatorStartup(encodeStartupFixture(t, records), started); err == nil || binding != (cnpgStartupBinding{}) {
			t.Fatalf("missing generated-resource setting %s accepted", field)
		}
	}
}

// encodeStartupFixture preserves native JSON-line framing.
func encodeStartupFixture(t *testing.T, records []object) []byte {
	t.Helper()
	var lines []string
	for _, record := range records {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestCNPGStartupDefaultsComeFromTheCurrentProcessRatherThanAdmission checks positive provenance.
func TestCNPGStartupDefaultsComeFromTheCurrentProcessRatherThanAdmission(t *testing.T) {
	records, started := cnpgStartupFixture()
	binding, err := bindCNPGOperatorStartup(encodeStartupFixture(t, records), started)
	if err != nil || binding.Defaults != (recoveryAdmissionDefaults{Version: "1.30.2", ConfigMapQueries: true}) || binding.ConfigSHA == "" || binding.OperatorPullSecretName != "cnpg-pull-secret" || binding.CreateAnyService {
		t.Fatalf("complete startup profile failed: %+v %v", binding, err)
	}
}

func TestCNPGStartupRetainsAnExplicitTrueServiceSetting(t *testing.T) {
	records, started := cnpgStartupFixture()
	records[2]["configuration"].(object)["createAnyService"] = true
	binding, err := bindCNPGOperatorStartup(encodeStartupFixture(t, records), started)
	if err != nil || !binding.CreateAnyService || binding.OperatorPullSecretName != "cnpg-pull-secret" {
		t.Fatalf("explicit native generated-service setting was not retained: %+v %v", binding, err)
	}
}

// TestCNPGStartupRefusesWrongReleaseMissingFlagsAndStaleProcessLogs rejects inferred defaults.
func TestCNPGStartupRefusesWrongReleaseMissingFlagsAndStaleProcessLogs(t *testing.T) {
	for name, mutate := range map[string]func([]object){
		"release":             func(o []object) { o[0]["version"] = "1.29.0" },
		"build":               func(o []object) { o[0]["build"].(object)["Commit"] = "unreviewed" },
		"source map":          func(o []object) { o[1]["name"] = "foreign-config" },
		"stale log":           func(o []object) { o[0]["ts"] = "2026-11-03T09:14:19Z" },
		"late configuration":  func(o []object) { o[2]["ts"] = "2026-11-03T10:20:19Z" },
		"missing flag":        func(o []object) { delete(o[2]["configuration"].(object), "monitoringQueriesSecret") },
		"added plugin":        func(o []object) { o[2]["configuration"].(object)["includePlugins"] = "foreign" },
		"monitoring override": func(o []object) { o[2]["configuration"].(object)["monitoringQueriesConfigmap"] = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			records, started := cnpgStartupFixture()
			mutate(records)
			if binding, err := bindCNPGOperatorStartup(encodeStartupFixture(t, records), started); err == nil || binding != (cnpgStartupBinding{}) {
				t.Fatal("unknown startup defaults returned a binding")
			}
		})
	}
}
