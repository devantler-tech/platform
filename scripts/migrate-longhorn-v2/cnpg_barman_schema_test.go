package main

import (
	"encoding/json"
	"testing"
)

// TestRecoverySchemasCannotSubstituteForEachOther binds the API identity as well as the full spec hash.
func TestRecoverySchemasCannotSubstituteForEachOther(t *testing.T) {
	o, pin := cnpgSchemaFixture()
	o["metadata"].(object)["name"] = "objectstores.barmancloud.cnpg.io"
	raw, _ := json.Marshal(o)
	bound, err := bindCNPGRecoveryCRD(raw, "objectstores.barmancloud.cnpg.io", pin)
	if err != nil || bound.SpecSHA != pin {
		t.Fatalf("complete synthetic object-store schema refused: %v", err)
	}
	if got, err := bindCNPGCRD(raw, pin); err == nil || got != (cnpgCRDBinding{}) {
		t.Fatal("object-store schema substituted for database schema")
	}
	if got, err := bindBarmanCRD(raw); err == nil || got != (cnpgCRDBinding{}) {
		t.Fatal("synthetic schema satisfied the released Barman pin")
	}
	if _, err := bindCNPGRecoveryCRD(raw, "foreign.example", pin); err == nil {
		t.Fatal("caller selected another resource family")
	}
}

// TestBarmanRecoverySchemaStillRequiresEstablishedUnchangedNativeState preserves the native status gate.
func TestBarmanRecoverySchemaStillRequiresEstablishedUnchangedNativeState(t *testing.T) {
	for name, mutate := range map[string]func(object){
		"changed schema":       func(o object) { o["spec"].(object)["profile"] = "changed" },
		"unestablished":        func(o object) { value(o, "status", "conditions").([]any)[0].(object)["status"] = "False" },
		"wrong stored version": func(o object) { o["status"].(object)["storedVersions"] = []any{"v2"} },
	} {
		t.Run(name, func(t *testing.T) {
			o, pin := cnpgSchemaFixture()
			o["metadata"].(object)["name"] = "objectstores.barmancloud.cnpg.io"
			mutate(o)
			raw, _ := json.Marshal(o)
			if got, err := bindCNPGRecoveryCRD(raw, "objectstores.barmancloud.cnpg.io", pin); err == nil || got != (cnpgCRDBinding{}) {
				t.Fatal("invalid native object-store schema accepted")
			}
		})
	}
}
