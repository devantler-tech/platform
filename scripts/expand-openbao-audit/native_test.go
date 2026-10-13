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
	"testing"
)

func TestRealKubectlPatchCarriesIdentityPreconditions(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		if os.Getenv("AUDIT_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required kubectl control unavailable")
		}
		t.Skip("optional kubectl transport control unavailable")
	}
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "conflict"}[conflict], func(t *testing.T) {
			patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": "claim-uid"}, {"op": "test", "path": "/metadata/resourceVersion", "value": "42"}, {"op": "replace", "path": "/spec/resources/requests/storage", "value": "20Gi"}}
			patches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var response any
				switch r.URL.Path {
				case "/api":
					response = map[string]any{"kind": "APIVersions", "apiVersion": "v1", "versions": []string{"v1"}}
				case "/apis":
					response = map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{}}
				case "/api/v1":
					response = map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": "v1", "resources": []any{map[string]any{"name": "persistentvolumeclaims", "singularName": "persistentvolumeclaim", "namespaced": true, "kind": "PersistentVolumeClaim", "verbs": []string{"get", "patch"}}}}
				case "/api/v1/namespaces/openbao/persistentvolumeclaims/audit-openbao-0":
					if r.Method == "PATCH" {
						patches++
						b, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						var got []map[string]any
						if json.Unmarshal(b, &got) != nil || !reflect.DeepEqual(got, patch) || r.Header.Get("Content-Type") != "application/json-patch+json" {
							t.Error("actual PATCH lacked exact UID/RV or scope")
						}
						if conflict {
							w.WriteHeader(http.StatusConflict)
							response = map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Conflict", "code": 409}
							break
						}
					}
					response = map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "audit-openbao-0", "namespace": "openbao", "uid": "claim-uid", "resourceVersion": "43"}, "spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": "20Gi"}}}}
				default:
					t.Errorf("unexpected native API path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				if json.NewEncoder(w).Encode(response) != nil {
					t.Error("response failed")
				}
			}))
			defer server.Close()
			config := filepath.Join(t.TempDir(), "kubeconfig")
			b := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: anonymous\ncurrent-context: admin@prod\nusers:\n- name: anonymous\n  user: {}\n")
			if os.WriteFile(config, b, 0600) != nil {
				t.Fatal("fixture config failed")
			}
			t.Setenv("KUBECONFIG", config)
			e := engine{execute: capture}
			r, err := e.write(context.Background(), patch, "--namespace=openbao", "patch", "persistentvolumeclaim", "audit-openbao-0", "--type=json", "--patch-file=REQUEST", "--output=json")
			if patches != 1 || (err != nil) != conflict || !conflict && field(r, "spec", "resources", "requests", "storage") != "20Gi" {
				t.Fatalf("wrong native outcome patches=%d error=%v", patches, err)
			}
		})
	}
}
