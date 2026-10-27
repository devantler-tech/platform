package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// TestDiagnosticReadbackUsesTaggedIdentity exercises the authenticated query/get path.
func TestDiagnosticReadbackUsesTaggedIdentity(t *testing.T) {
	for _, test := range []string{"valid", "different-type", "duplicate-console", "extra-event"} {
		t.Run(test, func(t *testing.T) {
			want := map[string]any{"@type": "Stdout", "enable": true, "level": "info", "eventsPolicy": "include", "events": map[string]any{"server.startup": true}}
			encoded, err := json.Marshal(map[string]any{"@type": "upsert", "object": "Tracer", "matchOn": []string{"@type"}, "value": map[string]any{"console": want}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "bootstrap" || password != "synthetic-only" || r.Method != http.MethodPost || r.URL.Path != "/jmap" {
					w.WriteHeader(403)
					return
				}
				var request struct {
					Calls [][]json.RawMessage `json:"methodCalls"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Calls) != 1 || len(request.Calls[0]) != 3 {
					w.WriteHeader(400)
					return
				}
				var method, tag string
				json.Unmarshal(request.Calls[0][0], &method)
				json.Unmarshal(request.Calls[0][2], &tag)
				var result any
				if method == "x:Tracer/query" {
					ids := []string{"console-id"}
					if test == "duplicate-console" {
						ids = append(ids, "other-id")
					}
					result = map[string]any{"ids": ids, "total": len(ids), "position": 0}
				} else if method == "x:Tracer/get" {
					actual := map[string]any{"id": "console-id", "@type": "Stdout", "enable": true, "level": "info", "eventsPolicy": "include", "events": map[string]any{"server.startup": true}}
					if test == "different-type" {
						actual["@type"] = "Log"
					}
					if test == "extra-event" {
						actual["events"].(map[string]any)["auth.success"] = true
					}
					list := []any{actual}
					if test == "duplicate-console" {
						duplicate := map[string]any{}
						for k, v := range actual {
							duplicate[k] = v
						}
						duplicate["id"] = "other-id"
						list = append(list, duplicate)
					}
					result = map[string]any{"list": list, "notFound": []string{}}
				} else {
					w.WriteHeader(400)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			m := management{url: server.URL, password: "synthetic-only", client: server.Client()}
			err = m.verify(context.Background(), encoded)
			if (err == nil) != (test == "valid") {
				t.Fatal("incorrect tracer identity or privacy readback verdict")
			}
		})
	}
}
