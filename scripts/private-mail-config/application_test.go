package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPlanSelectsLocalAdministrationAssetsAndBoundedRoutes(t *testing.T) {
	plan, err := render(fixture())
	if err != nil {
		t.Fatal(err)
	}
	ops, err := planOperations(plan)
	if err != nil {
		t.Fatal(err)
	}
	var applications []operation
	for _, op := range ops {
		if op.Object == "Application" {
			applications = append(applications, op)
		}
	}
	if len(applications) != 1 {
		t.Fatalf("expected one configured local administration application, got %d", len(applications))
	}
	op := applications[0]
	if op.Type != "upsert" || len(op.Value) != 1 {
		t.Fatal("administration configuration must preserve its existing application")
	}
	app, ok := op.Value["webadmin"].(map[string]any)
	if !ok || app["description"] != "Stalwart Web Interface" || app["enabled"] != true {
		t.Fatal("the administration application is not configured")
	}
	if app["resourceUrl"] != "file:///usr/local/share/stalwart/webui.zip" {
		t.Fatal("administration still depends on an external download or another local resource")
	}
	if !reflect.DeepEqual(app["urlPrefix"], map[string]any{"/admin": true, "/account": true}) {
		t.Fatal("administration routes are incomplete or broader than the supported interfaces")
	}
}

func applicationAssetFixture(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "webui.zip")
	if err := os.WriteFile(path, []byte("verified admin asset fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return path, "c8ab5014a65262af4c57ecf7602ffb31a6f6c67b5c6cb9f85f8a043e03e2f3bc"
}

func TestBootstrapChecksAdministrationAssetsBeforeStartingRecovery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(string) error
		started bool
		digest  string
	}{
		{"verified asset", func(string) error { return nil }, true, ""},
		{"missing asset", os.Remove, false, ""},
		{"altered asset", func(path string) error { return os.WriteFile(path, []byte("altered fixture"), 0644) }, false, ""},
		{"oversized matching asset", func(path string) error { return os.WriteFile(path, make([]byte, 1048577), 0644) }, false, "2cb74edba754a81d121c9db6833704a8e7d417e5b13d1a19f4a52f007d644264"},
		{"symlink asset", func(path string) error {
			if err := os.Rename(path, path+".target"); err != nil {
				return err
			}
			return os.Symlink(path+".target", path)
		}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asset, digest := applicationAssetFixture(t)
			if err := tc.change(asset); err != nil {
				t.Fatal(err)
			}
			if tc.digest != "" {
				digest = tc.digest
			}
			data := t.TempDir()
			pidFile := filepath.Join(data, "owned-child")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			runtime := bootstrapRuntime{server: executable, serverArgs: []string{"-test.run=TestOwnedBootstrapChild", "--", pidFile},
				cli: "/must-not-run", url: "http://127.0.0.1:1", dataDir: data, startup: 700 * time.Millisecond, shutdown: time.Second,
				applicationFile: asset, applicationDigest: digest}
			if bootstrap(context.Background(), bootstrapInputs(t), runtime) == nil {
				t.Fatal("the test recovery child never opens management; bootstrap must not succeed")
			}
			_, statErr := os.Stat(pidFile)
			if statErr != nil && !os.IsNotExist(statErr) {
				t.Fatal(statErr)
			}
			if (statErr == nil) != tc.started {
				t.Fatal("recovery process start did not honor the administration asset verification")
			}
		})
	}
}

func TestAdministrationReadbackRequiresTheLocalResourceAndExactRoutes(t *testing.T) {
	plan := []byte(`{"@type":"upsert","object":"Application","matchOn":["description"],"value":{"webadmin":{"description":"Stalwart Web Interface","enabled":true,"resourceUrl":"file:///usr/local/share/stalwart/webui.zip","urlPrefix":{"/admin":true,"/account":true}}}}` + "\n")
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{"verified local application", func(map[string]any) {}, true},
		{"external download", func(app map[string]any) { app["resourceUrl"] = "https://example.test/unreviewed.zip" }, false},
		{"additional public route", func(app map[string]any) { app["urlPrefix"].(map[string]bool)["/"] = true }, false},
		{"another application", func(app map[string]any) { app["description"] = "Another application" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := map[string]any{"id": "ui", "description": "Stalwart Web Interface", "enabled": true,
				"resourceUrl": "file:///usr/local/share/stalwart/webui.zip", "urlPrefix": map[string]bool{"/admin": true, "/account": true}}
			tc.change(app)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					Methods [][]json.RawMessage `json:"methodCalls"`
				}
				if r.URL.Path != "/jmap" || json.NewDecoder(r.Body).Decode(&call) != nil || len(call.Methods) != 1 || len(call.Methods[0]) != 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var method, tag string
				if json.Unmarshal(call.Methods[0][0], &method) != nil || json.Unmarshal(call.Methods[0][2], &tag) != nil || tag != "verify" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var result any
				switch method {
				case "x:Application/query":
					result = map[string]any{"ids": []string{"ui"}, "total": 1, "position": 0}
				case "x:Application/get":
					result = map[string]any{"list": []any{app}, "notFound": []string{}}
				default:
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": []any{[]any{method, result, tag}}})
			}))
			defer server.Close()
			m := management{client: server.Client(), url: server.URL, password: "synthetic-test-only"}
			if (m.verify(context.Background(), plan) == nil) != tc.valid {
				t.Fatal("administration readback did not enforce the configured resource and routes")
			}
		})
	}
}
