package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConsoleDiagnosticsExcludeMailAndAuthenticationEvents requires a bounded diagnostic tracer.
func TestConsoleDiagnosticsExcludeMailAndAuthenticationEvents(t *testing.T) {
	plan, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(plan)), "\n") {
		var op struct {
			Type    string `json:"@type"`
			Object  string
			MatchOn []string
			Value   map[string]map[string]any
		}
		var family struct{ Object string }
		if json.Unmarshal([]byte(line), &family) != nil {
			t.Fatal("invalid plan")
		}
		if family.Object != "Tracer" {
			continue
		}
		if json.Unmarshal([]byte(line), &op) != nil || op.Type != "upsert" || len(op.MatchOn) != 1 || op.MatchOn[0] != "@type" || len(op.Value) != 1 {
			t.Fatal("diagnostics are not repeatable by tracer type")
		}
		for _, tracer := range op.Value {
			count++
			if tracer["@type"] != "Stdout" || tracer["enable"] != true || tracer["level"] != "info" || tracer["buffered"] != false || tracer["ansi"] != false || tracer["multiline"] != false || tracer["lossy"] != false || tracer["eventsPolicy"] != "include" {
				t.Fatal("unsafe console diagnostic configuration")
			}
			events, ok := tracer["events"].(map[string]any)
			if !ok || len(events) == 0 || events["server.startup"] != true || events["server.startup-error"] != true || events["store.rocksdb-error"] != true {
				t.Fatal("startup diagnostics are incomplete")
			}
			allowed := map[string]bool{"server.startup": true, "server.shutdown": true, "server.startup-error": true, "server.thread-error": true, "network.listen-start": true, "network.listen-stop": true, "network.listen-error": true, "network.bind-error": true, "resource.error": true, "resource.not-found": true, "resource.bad-parameters": true, "resource.application-unpacked": true, "store.rocksdb-error": true}
			for event, enabled := range events {
				if !allowed[event] || enabled != true {
					t.Fatal("mail or authentication event reached console diagnostics")
				}
			}
		}
	}
	if count != 1 {
		t.Fatal("exactly one console diagnostic tracer is required")
	}
}

// TestDiagnosticReadbackRejectsAdditionalEvents prevents silent expansion of the console trace.
func TestDiagnosticReadbackRejectsAdditionalEvents(t *testing.T) {
	want := map[string]any{"server.startup": true}
	actual := map[string]any{"server.startup": true, "auth.success": true}
	if policyMatches(want, actual, "events") {
		t.Fatal("diagnostic readback accepted additional private events")
	}
}
