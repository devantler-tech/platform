package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the actual kubectl raw collection/watch transport, including its
// streaming output. The API fixture has no credentials and is not a cluster.
func TestCSIIdentityCaptureThroughNativeKubectl(t *testing.T) {
	if os.Getenv("LONGHORN_V2_NATIVE_TEST") != "1" {
		t.Skip("set LONGHORN_V2_NATIVE_TEST=1 with the repository-approved kubectl")
	}
	claim, pv, volume, op, source, class := csiScratchFixture("DataUpload")
	objects := map[string]object{
		"/api/v1/namespaces/velero/persistentvolumeclaims":             claim,
		"/api/v1/persistentvolumes":                                    pv,
		"/apis/longhorn.io/v1beta2/namespaces/longhorn-system/volumes": volume,
	}
	watching := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o, ok := objects[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") != "true" {
			if r.URL.Query().Get("limit") != "500" {
				http.Error(w, "missing collection bound", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(object{"apiVersion": "v1", "kind": "List", "metadata": object{"resourceVersion": "12345"}, "items": []any{}})
			return
		}
		if r.URL.Query().Get("resourceVersion") != "12345" || r.URL.Query().Get("allowWatchBookmarks") != "true" {
			http.Error(w, "watch lost its collection snapshot", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(object{"type": "ADDED", "object": o})
		w.(http.Flusher).Flush()
		watching <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: %s\ncontexts:\n- name: admin@prod\n  context:\n    cluster: fixture\ncurrent-context: admin@prod\n", server.URL)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	capture := newCSICapture("1234")
	if err := capture.start(ctx, client{run: execute, context: "admin@prod"}); err != nil {
		t.Fatal(err)
	}
	defer capture.stop()
	for range 3 {
		select {
		case <-watching:
		case <-ctx.Done():
			t.Fatal("native watches did not stream all three storage identities")
		}
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if proof, err := capture.proof(op, source, class); err == nil && proof.Volume.UID == text(volume, "metadata", "uid") {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("native watch events did not produce the bound CSI storage proof")
		}
	}
}
