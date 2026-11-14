package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Exercise the rendered policy through native JMAP readback, so a successful
// replay cannot silently retain a future external directory or a suspended
// recovery account. The protocol fixture contains no real credentials.
func TestRebuildVerifiesTheLocalAuthenticationBaseline(t *testing.T) {
	plan, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	var administrator, authentication operation
	for _, op := range ops {
		if op.Object == "Account" && op.Type == "create" {
			administrator = operation{Type: "create", Object: "Account", Value: map[string]any{"administrator": op.Value["administrator"]}}
			administrator.Value["administrator"].(map[string]any)["domainId"] = "fixture-domain"
		}
		if op.Object == "Authentication" && op.Type == "update" {
			authentication = op
		}
	}
	for _, tc := range []struct {
		name   string
		op     operation
		actual map[string]any
		valid  bool
	}{
		{"administrator recovered", administrator, map[string]any{"id": "fixture-admin", "@type": "User", "name": "operator", "domainId": "fixture-domain", "roles": map[string]any{"@type": "Admin"}, "permissions": map[string]any{"@type": "Inherit"}}, true},
		{"administrator still suspended", administrator, map[string]any{"id": "fixture-admin", "@type": "User", "name": "operator", "domainId": "fixture-domain", "roles": map[string]any{"@type": "Admin"}, "permissions": map[string]any{"@type": "Replace", "enabledPermissions": map[string]any{}, "disabledPermissions": map[string]any{}}}, false},
		{"local directory recovered", authentication, map[string]any{"id": "singleton", "directoryId": nil}, true},
		{"external directory retained", authentication, map[string]any{"id": "singleton", "directoryId": "future-provider"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.op.Object == "" {
				t.Fatal("the rebuild plan does not declare the authentication baseline")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				if r.URL.Path != "/jmap" || json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 || len(call.Methods[0]) != 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var method, tag string
				if json.Unmarshal(call.Methods[0][0], &method) != nil || json.Unmarshal(call.Methods[0][2], &tag) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any
				switch method {
				case "x:Account/query":
					result = map[string]any{"ids": []string{"fixture-admin"}, "total": 1, "position": 0}
				case "x:Account/get", "x:Authentication/get":
					result = map[string]any{"list": []any{tc.actual}, "notFound": []string{}}
				default:
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			raw, err := json.Marshal(tc.op)
			if err != nil {
				t.Fatal(err)
			}
			m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
			if (m.verify(context.Background(), append(raw, '\n')) == nil) != tc.valid {
				t.Fatal("readback accepted stale authentication state or rejected the recovered baseline")
			}
		})
	}
}
