package main

import (
	"encoding/json"
	"testing"
	"time"
)

// TestPauseAcknowledgmentMatchesAuditedInfoProtocol catches confusing the
// controller's Warning method with its structured controller log level.
func TestPauseAcknowledgmentMatchesAuditedInfoProtocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		level    any
		previous map[string]bool
		want     bool
	}{
		{name: "audited info", level: "info", want: true},
		{name: "warning", level: "warning"},
		{name: "warn", level: "warn"},
		{name: "debug", level: "debug"},
		{name: "error", level: "error"},
		{name: "empty", level: ""},
		{name: "missing", level: nil},
		{name: "numeric", level: float64(0)},
		{name: "replayed info", level: "info", previous: map[string]bool{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Independently specified public protocol with synthetic identities;
			// never a copied production log or the production message constant.
			record := object{
				"level": tc.level, "ts": testNow.Format(time.RFC3339Nano),
				"msg":        "Disable reconciliation loop annotation set, skipping the reconciliation.",
				"controller": "cluster", "controllerGroup": "postgresql.cnpg.io", "controllerKind": "Cluster",
				"namespace": "wedding-app", "name": "wedding-db",
				"Cluster":     object{"namespace": "wedding-app", "name": "wedding-db"},
				"reconcileID": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			}
			if tc.level == nil {
				delete(record, "level")
			}
			b, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			got, err := pauseAcknowledged(b, testNow.Add(-time.Second), testNow, tc.previous)
			if err != nil || got != tc.want {
				t.Fatalf("acknowledgment=%v, want=%v: %v", got, tc.want, err)
			}
		})
	}
}
