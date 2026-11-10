package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestCNPGPostgresReadCannotInheritRemoteConnectionSettings binds SQL to the observed Pod.
func TestCNPGPostgresReadCannotInheritRemoteConnectionSettings(t *testing.T) {
	fixture := cnpgObservationFixture(t)
	pod := fixture.Pods[0]
	value(pod, "spec", "containers").([]any)[0].(object)["env"] = []any{
		object{"name": "PGHOST", "value": "unrelated.invalid"},
		object{"name": "PGHOSTADDR", "value": "203.0.113.10"},
		object{"name": "PGSERVICE", "value": "unrelated"},
		object{"name": "PGPORT", "value": "15432"},
	}
	c := client{context: "admin@prod", run: func(_ context.Context, _ []byte, program string, args ...string) ([]byte, error) {
		if program != "kubectl" || len(args) < 3 {
			t.Fatal("unexpected executable")
		}
		if args[2] == "get" {
			return json.Marshal(pod)
		}
		if args[2] != "exec" {
			t.Fatal("unexpected production operation")
		}
		separator := -1
		for i, arg := range args {
			if arg == "--" {
				separator = i
				break
			}
		}
		if separator < 0 || len(args) < separator+5 || args[separator+1] != "/usr/bin/env" || args[separator+2] != "-i" {
			t.Fatal("SQL could inherit remote connection or executable settings")
		}
		command := strings.Join(args[separator+1:], " ")
		for _, required := range []string{"PATH=/usr/lib/postgresql/18/bin:/usr/local/bin:/usr/bin:/bin", "PGCONNECT_TIMEOUT=10", "--host=/controller/run", "--port=5432", "--no-psqlrc", "--no-password"} {
			if !strings.Contains(command, required) {
				t.Fatalf("local PostgreSQL command missing %s", required)
			}
		}
		return json.Marshal(fixture.States[text(pod, "metadata", "name")])
	}}
	if _, err := c.observePostgres(context.Background(), fixture.Cluster, pod); err != nil {
		t.Fatal(err)
	}
}
