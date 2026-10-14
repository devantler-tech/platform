package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devantler-tech/platform/internal/portalstorage"
)

// nativeListResponses models Kubernetes' raw typed-list wire format only at the
// external read boundary. Individual GET and guarded PATCH responses stay typed.
func nativeListResponses(t *testing.T, a *storageAPI) command {
	t.Helper()
	return func(ctx context.Context, program string, args ...string) ([]byte, error) {
		b, err := a.execute(ctx, program, args...)
		if err != nil || program != "kubectl" || !strings.Contains(strings.Join(args, " "), "get --raw=/api/v1/") {
			return b, err
		}
		list := unmarshal(t, string(b))
		for _, raw := range list["items"].([]any) {
			delete(raw.(map[string]any), "apiVersion")
			delete(raw.(map[string]any), "kind")
		}
		return json.Marshal(list)
	}
}

// TestStorageNativeTypedLists proves the real invocation reaches a complete
// receipt with native list responses, without losing guarded patch preservation.
func TestStorageNativeTypedLists(t *testing.T) {
	for _, tc := range []struct {
		name, flag, operation, confirmation, result string
		patches, pauses                             int
	}{
		{"check", "--check", "check", "CHECK BACKSTAGE STORAGE ONLY", "CHECKED", 0, 1},
		{"protect", "--execute", "protect", "PROTECT BACKSTAGE STORAGE ONLY", "PROTECTED", 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := storageFixture(t)
			before := mustJSON(t, []any{a.claims, a.db, a.ns, a.pods})
			beforeVolumes := mustJSON(t, a.volumes)
			env := testEnvironment()
			env["PORTAL_STORAGE_OPERATION"], env["PORTAL_STORAGE_CONFIRM"] = tc.operation, tc.confirmation
			var out bytes.Buffer
			err := run(context.Background(), []string{tc.flag}, func(k string) string { return env[k] }, nativeListResponses(t, a), a.wait, &out)
			if err != nil {
				t.Fatalf("native Kubernetes typed-list response stopped the protected operation: %v", err)
			}
			var got map[string]any
			if json.Unmarshal(out.Bytes(), &got) != nil {
				t.Fatal("receipt is not JSON")
			}
			want := map[string]any{"result": tc.result, "claims": float64(2), "changed": float64(tc.patches), "alreadyRetained": float64(0), "separatedSamples": true, "retirement": "NOT_EXECUTED"}
			if tc.operation == "check" {
				want["wouldChange"] = float64(2)
			}
			if !reflect.DeepEqual(got, want) || a.patches != tc.patches || a.pauses != tc.pauses || !bytes.Equal(before, mustJSON(t, []any{a.claims, a.db, a.ns, a.pods})) {
				t.Fatalf("incorrect receipt or lost non-volume preservation: %s", out.String())
			}
			if tc.operation == "check" && !bytes.Equal(beforeVolumes, mustJSON(t, a.volumes)) {
				t.Fatal("read-only preflight changed a volume")
			}
			if tc.operation == "protect" {
				wantVolumes := unmarshal(t, string(beforeVolumes))
				for i := range 2 {
					mchild(mitem(wantVolumes, i), "spec")["persistentVolumeReclaimPolicy"] = "Retain"
					mchild(mitem(wantVolumes, i), "metadata")["resourceVersion"] = []string{"2011", "2021"}[i]
				}
				if !bytes.Equal(mustJSON(t, wantVolumes), mustJSON(t, a.volumes)) {
					t.Fatal("guarded patch changed more than reclaim policy and API bookkeeping")
				}
			}
		})
	}
}

// TestStorageNativeListAmbiguityCannotWrite keeps the same fail-closed identity,
// pagination and reciprocal-binding gates after TypeMeta omission is supported.
func TestStorageNativeListAmbiguityCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"generic list cannot supply item type", func(l map[string]any) { l["kind"] = "List" }},
		{"unknown list cannot supply item type", func(l map[string]any) { l["kind"] = "SecretList" }},
		{"foreign API cannot supply item type", func(l map[string]any) { l["apiVersion"] = "example.test/v1" }},
		{"explicit wrong item API", func(l map[string]any) { mitem(l, 0)["apiVersion"] = "example.test/v1" }},
		{"explicit empty item API", func(l map[string]any) { mitem(l, 0)["apiVersion"] = "" }},
		{"explicit null item API", func(l map[string]any) { mitem(l, 0)["apiVersion"] = nil }},
		{"explicit wrong item kind", func(l map[string]any) { mitem(l, 0)["kind"] = "Secret" }},
		{"explicit null item kind", func(l map[string]any) { mitem(l, 0)["kind"] = nil }},
		{"remaining page", func(l map[string]any) { mchild(l, "metadata")["continue"] = "next" }},
		{"remaining items", func(l map[string]any) { mchild(l, "metadata")["remainingItemCount"] = float64(1) }},
		{"missing UID", func(l map[string]any) { delete(mchild(mitem(l, 0), "metadata"), "uid") }},
		{"duplicate item", func(l map[string]any) { l["items"] = append(l["items"].([]any), mitem(l, 0)) }},
	} {
		for _, resource := range []string{"persistentvolumeclaims", "persistentvolumes", "pods"} {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				a := storageFixture(t)
				read := nativeListResponses(t, a)
				external := func(ctx context.Context, name string, args ...string) ([]byte, error) {
					b, err := read(ctx, name, args...)
					if err != nil || !strings.HasSuffix(strings.Join(args, " "), "/"+resource) {
						return b, err
					}
					list := unmarshal(t, string(b))
					tc.mutate(list)
					return json.Marshal(list)
				}
				var out bytes.Buffer
				env := testEnvironment()
				if err := run(context.Background(), []string{"--execute"}, func(k string) string { return env[k] }, external, a.wait, &out); err == nil || a.patches != 0 || out.Len() != 0 {
					t.Fatal("ambiguous native list authorized a write or a successful receipt")
				}
			})
		}
	}
}

// TestNativeKubectlTypedListCensus exercises the real raw-read transport against
// an anonymous loopback API, followed by the actual binding and consumer guards.
func TestNativeKubectlTypedListCensus(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		if os.Getenv("PORTAL_STORAGE_REQUIRE_NATIVE_CONTROL") == "1" {
			t.Fatal("required native list control needs kubectl")
		}
		t.Skip("native list control requires kubectl")
	}
	a := storageFixture(t)
	responses := map[string]map[string]any{
		"/api/v1/namespaces/backstage/persistentvolumeclaims": a.claims,
		"/api/v1/persistentvolumes":                           a.volumes,
		"/api/v1/namespaces/backstage/pods":                   a.pods,
	}
	for _, list := range responses {
		for _, raw := range list["items"].([]any) {
			delete(raw.(map[string]any), "apiVersion")
			delete(raw.(map[string]any), "kind")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, found := responses[r.URL.Path]
		if r.Method != http.MethodGet || !found {
			t.Errorf("unexpected native census request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(list); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\n    user: anonymous\ncurrent-context: admin@prod\nusers:\n- name: anonymous\n  user: {}\n"
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", config)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	e := engine{execute: execute}
	claims, err := e.read(ctx, "get", "--raw=/api/v1/namespaces/backstage/persistentvolumeclaims")
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := e.read(ctx, "get", "--raw=/api/v1/persistentvolumes")
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := portalstorage.ValidateBindings("database", claims, volumes, false)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("real raw-read transport lost the complete storage join: bindings=%d error=%v", len(bindings), err)
	}
	pods, err := e.read(ctx, "get", "--raw=/api/v1/namespaces/backstage/pods")
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]map[string]any{}
	if err := validatePods(pods, "database", "backstage-db-4", 2, map[string]bool{"backstage-db-3": true, "backstage-db-4": true}, kept); err != nil || len(kept) != 2 {
		t.Fatalf("real raw-read transport lost healthy consumer coverage: kept=%d error=%v", len(kept), err)
	}
}
