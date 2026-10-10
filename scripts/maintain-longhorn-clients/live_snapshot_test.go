package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Explicit, offline replay of a privately captured OIDC read-only inventory.
// This test performs no API call or write, and publishes no resource payload.
func TestPrivateReadOnlyPreflightSnapshot(t *testing.T) {
	path := os.Getenv("LONGHORN_PRIVATE_PREFLIGHT_SNAPSHOT")
	if path == "" {
		t.Skip("no private OIDC snapshot supplied")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var o object
	if err = json.Unmarshal(b, &o); err != nil {
		t.Fatal(err)
	}
	s := inventory{clusters: items(o, "clusters", "items"), pods: items(o, "pods", "items"), claims: items(o, "claims", "items"), pvs: items(o, "pvs", "items"), volumes: items(o, "volumes", "items"), backups: items(o, "backups", "items"), replicasets: items(o, "replicasets", "items"), release: val(o, "release").(map[string]any), deployment: val(o, "deployment").(map[string]any)}
	for _, w := range items(o, "extra") {
		r, rok := val(w, "release").(map[string]any)
		d, dok := val(w, "deployment").(map[string]any)
		if !rok || !dok {
			t.Fatal("additional consumer coverage is malformed")
		}
		s.extra = append(s.extra, workload{r, d})
	}
	p, err := buildPlan(s, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read-only preflight: %d clusters, %d retained claims, %d backing volumes", len(p.clusters), len(p.claims), len(p.volumes))
}
