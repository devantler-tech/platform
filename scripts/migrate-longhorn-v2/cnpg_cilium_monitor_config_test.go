package main

import (
	"encoding/json"
	"os"
	"testing"
)

func monitorOptionsFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/synthetic-endpoint-options-cilium-1.20.5.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRecoveryMonitorRequiresNativeRealizedOptions(t *testing.T) {
	var original object
	if json.Unmarshal(monitorOptionsFixture(t), &original) != nil {
		t.Fatal("native fixture JSON")
	}
	cfg, err := bindRecoveryCiliumMonitorConfig(original)
	if err != nil || !cfg.Notifications || cfg.Audit {
		t.Fatalf("native endpoint options: %+v %v", cfg, err)
	}
	for name, change := range map[string]func(object){
		"notifications disabled":            func(o object) { value(o, "realized", "options").(object)["DropNotification"] = "Disabled" },
		"audit enabled":                     func(o object) { value(o, "realized", "options").(object)["PolicyAuditMode"] = "Enabled" },
		"missing audit observation":         func(o object) { delete(value(o, "realized", "options").(object), "PolicyAuditMode") },
		"missing notification observation":  func(o object) { delete(value(o, "realized", "options").(object), "DropNotification") },
		"boolean in place of native string": func(o object) { value(o, "realized", "options").(object)["DropNotification"] = true },
		"null native option":                func(o object) { value(o, "realized", "options").(object)["DropNotification"] = nil },
		"desired options only":              func(o object) { o["spec"] = o["realized"]; delete(o, "realized") },
		"native error":                      func(o object) { o["error"] = "private-response-not-for-logs" },
	} {
		t.Run(name, func(t *testing.T) {
			var o object
			if json.Unmarshal(monitorOptionsFixture(t), &o) != nil {
				t.Fatal("native fixture JSON")
			}
			change(o)
			if _, err := bindRecoveryCiliumMonitorConfig(o); err == nil {
				t.Fatal("unobserved notification or audit mode accepted")
			}
		})
	}
}
