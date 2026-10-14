package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v5"
)

// TestNativeKubectlRetentionPatchIsBoundAndPreservesStorage exercises real kubectl against an anonymous loopback API with positive and replaced-identity controls.
func TestNativeKubectlRetentionPatchIsBoundAndPreservesStorage(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		if os.Getenv("PORTAL_STORAGE_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required kubectl transport control unavailable")
		}
		t.Skip("optional kubectl transport control unavailable")
	}
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "conflict"}[conflict], func(t *testing.T) {
			a := storageFixture(t)
			volume := mitem(a.volumes, 0)
			before := mustJSON(t, volume)
			patches := 0
			var state sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state.Lock()
				defer state.Unlock()
				w.Header().Set("Content-Type", "application/json")
				var response any
				switch r.URL.Path {
				case "/api":
					response = map[string]any{"kind": "APIVersions", "apiVersion": "v1", "versions": []string{"v1"}}
				case "/apis":
					response = map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{}}
				case "/api/v1":
					response = map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": "v1", "resources": []any{map[string]any{"name": "persistentvolumes", "singularName": "persistentvolume", "namespaced": false, "kind": "PersistentVolume", "verbs": []string{"get", "patch"}}}}
				case "/api/v1/persistentvolumes/pv-3":
					if r.Method == "PATCH" {
						patches++
						body, err := io.ReadAll(io.LimitReader(r.Body, responseLimit+1))
						if err != nil || len(body) > responseLimit || r.Header.Get("Content-Type") != "application/json-patch+json" || r.URL.Query().Get("fieldManager") != "portal-storage-protection" {
							t.Error("wrong native patch transport")
						}
						var operations []map[string]any
						if json.Unmarshal(body, &operations) != nil || len(operations) != 6 {
							t.Error("native patch preconditions incomplete")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						for i, path := range []string{"/apiVersion", "/kind", "/metadata/uid", "/metadata/resourceVersion", "/spec"} {
							if operations[i]["op"] != "test" || operations[i]["path"] != path {
								t.Error("native patch omitted exact identity or full-spec precondition")
							}
						}
						if operations[2]["value"] != "volume-3" || operations[3]["value"] != "201" || operations[5]["op"] != "replace" || operations[5]["path"] != "/spec/persistentVolumeReclaimPolicy" || operations[5]["value"] != "Retain" {
							t.Error("native patch widened its write")
						}
						live := unmarshal(t, string(before))
						if conflict {
							mchild(live, "metadata")["uid"] = "replacement"
						}
						patch, err := jsonpatch.DecodePatch(body)
						if err != nil {
							t.Error(err)
							return
						}
						updated, err := patch.Apply(mustJSON(t, live))
						if conflict {
							if err == nil {
								t.Error("real patch accepted replaced identity")
							}
							w.WriteHeader(http.StatusConflict)
							response = map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409}
							break
						}
						if err != nil {
							t.Error(err)
							return
						}
						volume = unmarshal(t, string(updated))
						mchild(volume, "metadata")["resourceVersion"] = "203"
					}
					response = volume
				default:
					t.Errorf("unexpected native API path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if json.NewEncoder(w).Encode(response) != nil {
					t.Error("native response failed")
				}
			}))
			defer server.Close()
			config := filepath.Join(t.TempDir(), "kubeconfig")
			b := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: anonymous\ncurrent-context: admin@prod\nusers:\n- name: anonymous\n  user: {}\n")
			if err := os.WriteFile(config, b, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", config)
			input, err := decode(before)
			if err != nil {
				t.Fatal(err)
			}
			err = (engine{execute: execute}).protect(context.Background(), input)
			state.Lock()
			defer state.Unlock()
			if patches != 1 || (err != nil) != conflict {
				t.Fatalf("wrong native outcome patches=%d error=%v", patches, err)
			}
			if !conflict {
				got, decodeErr := decode(mustJSON(t, volume))
				if decodeErr != nil || field(got, "spec", "persistentVolumeReclaimPolicy") != "Retain" || !reflect.DeepEqual(projection(input, true), projection(got, true)) {
					t.Fatal("native write changed another storage field")
				}
			}
		})
	}
}
