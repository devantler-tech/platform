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
	"strings"
	"sync"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

// Exercise the real kubectl transport and JSON Patch interpreter against an
// isolated HTTP fixture. There is no cluster or production kubeconfig here;
// only the reviewed-main lookup is synthetic. This is transport evidence, not
// a native source, backup, restore or retirement acceptance receipt.
func TestNativeSeedJournalThroughKubectl(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Fatal("native kubectl is required:", err)
	}
	for _, fault := range []string{"", "revision changed", "UID changed", "write acknowledged then lost", "readback replaced", "readback unchanged"} {
		t.Run(fault, func(t *testing.T) {
			env := nativeSeedDispatch()
			for key, val := range env {
				t.Setenv(key, val)
			}
			record, before := seedStateFixture(t, "created")
			record.Source, before.Source = env["GITHUB_SHA"], env["GITHUB_SHA"]
			after := before
			after.Phase = "source-write-requested"
			recordBody, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			seedBody, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			journal := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": object{"name": nativeSeedJournal, "namespace": "longhorn-system", "uid": "original-journal", "resourceVersion": "17"},
				"data": object{"record": string(recordBody), "seed": string(seedBody)}}
			var mu sync.Mutex
			reads, patches, applied := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				write := func(status int, response object) {
					w.WriteHeader(status)
					if encodeErr := json.NewEncoder(w).Encode(response); encodeErr != nil {
						t.Error("fixture response:", encodeErr)
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
						write(200, object{"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "v1", "resources": []any{object{
							"name": "configmaps", "singularName": "configmap", "namespaced": true, "kind": "ConfigMap", "verbs": []any{"get", "patch"}}}})
						return
					}
				}
				if r.URL.Path != "/api/v1/namespaces/longhorn-system/configmaps/"+nativeSeedJournal {
					t.Error("unexpected fixture API path:", r.Method, r.URL.Path)
					write(404, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "NotFound", "code": 404})
					return
				}
				switch r.Method {
				case http.MethodGet:
					reads++
					write(200, journal)
				case http.MethodPatch:
					patches++
					if r.Header.Get("Content-Type") != "application/json-patch+json" {
						t.Error("progress did not use JSON Patch")
					}
					body, readErr := io.ReadAll(io.LimitReader(r.Body, 64<<10))
					if readErr != nil {
						t.Error(readErr)
					}
					meta := journal["metadata"].(object)
					switch fault {
					case "revision changed":
						meta["resourceVersion"] = "18"
					case "UID changed":
						meta["uid"] = "replacement-journal"
					}
					current, marshalErr := json.Marshal(journal)
					patch, decodeErr := jsonpatch.DecodePatch(body)
					if marshalErr != nil || decodeErr != nil {
						t.Error("invalid native JSON Patch", marshalErr, decodeErr)
					}
					changed, applyErr := patch.Apply(current)
					if applyErr != nil {
						write(422, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Invalid", "code": 422, "message": "JSON Patch test failed"})
						return
					}
					applied++
					if fault != "readback unchanged" {
						if unmarshalErr := json.Unmarshal(changed, &journal); unmarshalErr != nil {
							t.Error(unmarshalErr)
						}
					}
					journal["metadata"].(object)["resourceVersion"] = "19"
					if fault == "readback replaced" {
						journal["metadata"].(object)["uid"] = "replacement-journal"
					}
					if fault == "write acknowledged then lost" {
						connection, _, hijackErr := w.(http.Hijacker).Hijack()
						if hijackErr != nil {
							t.Error(hijackErr)
							return
						}
						_ = connection.Close()
						return
					}
					write(200, journal)
				default:
					t.Error("unexpected fixture API verb:", r.Method)
					write(405, object{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": 405})
				}
			}))
			defer server.Close()
			config := object{"apiVersion": "v1", "kind": "Config", "current-context": "admin@prod",
				"clusters": []any{object{"name": "fixture", "cluster": object{"server": server.URL}}},
				"contexts": []any{object{"name": "admin@prod", "context": object{"cluster": "fixture", "user": "fixture"}}},
				"users":    []any{object{"name": "fixture", "user": object{}}}}
			configBody, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "config")
			if err = os.WriteFile(configPath, configBody, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", configPath)
			c := client{context: "admin@prod", nativeSeed: true, run: func(ctx context.Context, input []byte, program string, args ...string) ([]byte, error) {
				if program == "git" && input == nil && strings.Join(args, " ") == "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main" {
					return fmt.Appendf(nil, "%s\trefs/heads/main\n", record.Source), nil
				}
				if program != "kubectl" {
					t.Fatal("fixture escaped to an unexpected command:", program)
				}
				return execute(ctx, input, program, args...)
			}}
			err = saveCanarySeedState(t.Context(), c, "original-journal", before, after)
			mu.Lock()
			defer mu.Unlock()
			if (err == nil) != (fault == "") {
				t.Fatal("native transport accepted unconfirmed seed progress:", fault, err)
			}
			wantApplied := 1
			if fault == "revision changed" || fault == "UID changed" {
				wantApplied = 0
			}
			if patches != 1 || applied != wantApplied || reads < 1 {
				t.Fatalf("progress retried or escaped the conditional write: reads=%d PATCH=%d applied=%d", reads, patches, applied)
			}
			var saved canarySeedState
			if err = json.Unmarshal([]byte(text(journal, "data", "seed")), &saved); err != nil {
				t.Fatal(err)
			}
			if fault == "" && saved != after || text(journal, "data", "record") != string(recordBody) {
				t.Fatal("native progress lost its seed or changed the retirement record")
			}
		})
	}
}
