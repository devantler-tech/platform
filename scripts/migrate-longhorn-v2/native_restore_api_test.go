package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

// Real kubectl and the native JSON Patch interpreter, against only a local
// two-ConfigMap HTTP fixture. No cluster, real kubeconfig or credential is used.
func TestNativeRestoreJournalThroughKubectl(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatal("native kubectl is required:", err)
	}
	for _, fault := range []string{"", "revision changed", "UID changed", "lost response", "seed replaced", "unchanged readback"} {
		t.Run(fault, func(t *testing.T) {
			record, seed, before := nativeRestoreStateFixture(t, "write-requested")
			_, _, after := nativeRestoreStateFixture(t, "write-verified")
			for key, val := range nativeRestoreDispatch(before) {
				t.Setenv(key, val)
			}
			marshal := func(o any) string {
				raw, err := json.Marshal(o)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			source := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeSeedJournal, "namespace": "longhorn-system", "uid": before.SeedJournal.UID, "resourceVersion": "11"},
				"data": object{"record": marshal(record), "seed": marshal(seed)}}
			journal := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeRestoreJournal, "namespace": "longhorn-system", "uid": "restore-journal-uid", "resourceVersion": "17"},
				"data": object{"restore": marshal(before)}}
			var mu sync.Mutex
			patches, applied := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				write := func(status int, body object) {
					w.WriteHeader(status)
					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Error(err)
					}
				}
				if r.Method == http.MethodGet {
					switch r.URL.Path {
					case "/api":
						write(200, object{"apiVersion": "v1", "kind": "APIVersions", "versions": []any{"v1"}})
						return
					case "/apis":
						write(200, object{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{}})
						return
					case "/api/v1":
						write(200, object{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "v1", "resources": []any{object{"name": "configmaps", "singularName": "configmap", "namespaced": true, "kind": "ConfigMap", "verbs": []any{"get", "patch"}}}})
						return
					case "/api/v1/namespaces/longhorn-system/configmaps/" + nativeSeedJournal:
						write(200, source)
						return
					case "/api/v1/namespaces/longhorn-system/configmaps/" + nativeRestoreJournal:
						write(200, journal)
						return
					}
				}
				if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/namespaces/longhorn-system/configmaps/"+nativeRestoreJournal {
					t.Error("restore progress escaped its exact two-journal API surface:", r.Method, r.URL.Path)
					write(404, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404})
					return
				}
				patches++
				if r.Header.Get("Content-Type") != "application/json-patch+json" {
					t.Error("restore progress did not use conditional JSON Patch")
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
				if err != nil {
					t.Error(err)
					write(500, object{"kind": "Status", "status": "Failure", "code": 500})
					return
				}
				if fault == "revision changed" {
					journal["metadata"].(object)["resourceVersion"] = "18"
				}
				if fault == "UID changed" {
					journal["metadata"].(object)["uid"] = "replacement-journal"
				}
				patch, err := jsonpatch.DecodePatch(body)
				if err != nil {
					t.Error(err)
					write(400, object{"kind": "Status", "status": "Failure", "code": 400})
					return
				}
				current, err := json.Marshal(journal)
				if err != nil {
					t.Error(err)
					return
				}
				changed, err := patch.Apply(current)
				if err != nil {
					write(422, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Invalid", "code": 422, "message": "JSON Patch test failed"})
					return
				}
				applied++
				if fault != "unchanged readback" {
					if err = json.Unmarshal(changed, &journal); err != nil {
						t.Error(err)
					}
				}
				journal["metadata"].(object)["resourceVersion"] = "19"
				if fault == "seed replaced" {
					source["metadata"].(object)["uid"] = "replacement-seed"
				}
				if fault == "lost response" {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				write(200, journal)
			}))
			defer server.Close()
			config := object{"apiVersion": "v1", "kind": "Config", "current-context": "admin@prod",
				"clusters": []any{object{"name": "fixture", "cluster": object{"server": server.URL}}},
				"contexts": []any{object{"name": "admin@prod", "context": object{"cluster": "fixture", "user": "fixture"}}},
				"users":    []any{object{"name": "fixture", "user": object{}}}}
			configPath := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(configPath, []byte(marshal(config)), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", configPath)
			c := client{context: "admin@prod", run: func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" && input == nil && strings.Join(args, " ") == "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main" {
					return fmt.Appendf(nil, "%s\trefs/heads/main\n", before.Source), nil
				}
				if program != "kubectl" {
					t.Fatal("native fixture escaped to an unexpected command", program)
				}
				return execute(ctx, input, program, args...)
			}}
			err := saveNativeRestoreState(t.Context(), c, "restore-journal-uid", before, after)
			mu.Lock()
			defer mu.Unlock()
			if (err == nil) != (fault == "") {
				t.Fatal("native transport accepted unconfirmed restore progress", fault, err)
			}
			wantApplied := 1
			if fault == "revision changed" || fault == "UID changed" {
				wantApplied = 0
			}
			if patches != 1 || applied != wantApplied {
				t.Fatal("a conditional write was retried or ignored its original UID/revision", patches, applied)
			}
			var saved nativeRestoreState
			if err = json.Unmarshal([]byte(text(journal, "data", "restore")), &saved); err != nil {
				t.Fatal(err)
			}
			if fault == "" && !reflect.DeepEqual(saved, after) || text(source, "data", "record") != marshal(record) || text(source, "data", "seed") != marshal(seed) {
				t.Fatal("restore progress lost its expected state or changed the source evidence")
			}
		})
	}
}
