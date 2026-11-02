package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A rebuilt datastore assigns new native role IDs. Verification must resolve
// the declared creation reference in the binding and refuse additional grants.
func TestRebuildVerifiesTheDeclaredAdministratorBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		roleIDs map[string]bool
		valid   bool
	}{
		{"exact binding", map[string]bool{"native-owned-role": true}, true},
		{"retained additional administrator", map[string]bool{"native-owned-role": true, "unmanaged-role": true}, false},
		{"wrong administrator", map[string]bool{"unmanaged-role": true}, false},
		{"missing administrator", map[string]bool{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authenticationReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				if r.URL.Path != "/jmap" || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Methods) != 1 || len(request.Methods[0]) != 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var method, tag string
				if json.Unmarshal(request.Methods[0][0], &method) != nil || json.Unmarshal(request.Methods[0][2], &tag) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any
				switch method {
				case "x:Role/query":
					result = map[string]any{"ids": []string{"native-owned-role"}, "total": 1, "position": 0, "queryState": "fixture-state", "canCalculateChanges": false}
				case "x:Role/get":
					result = map[string]any{"state": "fixture-state", "list": []any{map[string]any{"id": "native-owned-role", "description": "Declared Email Administrator", "memberTenantId": nil, "roleIds": map[string]bool{}, "enabledPermissions": map[string]bool{"authenticate": true, "sysAccountQuery": true}, "disabledPermissions": map[string]bool{}}}, "notFound": []string{}}
				case "x:Authentication/get":
					authenticationReads.Add(1)
					result = map[string]any{"state": "fixture-state", "list": []any{map[string]any{"id": "singleton", "defaultAdminRoleIds": tc.roleIDs, "defaultUserRoleIds": map[string]bool{}}}, "notFound": []string{}}
				default:
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			plan := []byte(`{"@type":"upsert","object":"Role","matchOn":["description"],"value":{"email-admin":{"description":"Declared Email Administrator","memberTenantId":null,"roleIds":{},"enabledPermissions":{"authenticate":true,"sysAccountQuery":true},"disabledPermissions":{}}}}
{"@type":"update","object":"Authentication","value":{"defaultAdminRoleIds":{"#email-admin":true},"defaultUserRoleIds":{}}}
`)
			m := management{url: server.URL, password: t.Name(), client: server.Client()}
			err := m.verify(context.Background(), plan)
			if (err == nil) != tc.valid {
				t.Fatal("administrator binding was rejected or undeclared administration was accepted")
			}
			if authenticationReads.Load() != 1 {
				t.Fatal("the binding was not read; an earlier refusal cannot satisfy this control")
			}
		})
	}
}
