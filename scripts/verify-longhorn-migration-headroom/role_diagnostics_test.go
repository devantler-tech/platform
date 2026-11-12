package main

import (
	"strings"
	"testing"
)

// These native-boundary diagnostics classify only fixed spellings. They never
// accept a legacy value, publish an arbitrary label, or confer headroom.
func TestRoleHoldDistinguishesCanonicalLegacyAndUnknownLabels(t *testing.T) {
	for _, tc := range []struct{ role, index, observed, indexing string }{
		{"control-plane", "0", "legacy-hyphen-controlplane", "zero-based"},
		{"control_plane", "1", "legacy-underscore-controlplane", "matches-name"},
		{"worker", "", "canonical-worker", "missing"},
		{"", "private-index-canary", "missing", "unrecognized"},
		{"private-role-canary", "01", "unrecognized", "unrecognized"},
	} {
		t.Run(tc.observed+"/"+tc.indexing, func(t *testing.T) {
			servers, nodes := roster(t, true)
			servers[0].Name, nodes[0].Metadata.Name = "prod-control-plane-1", "prod-control-plane-1"
			servers[0].Labels["ksail.node.type"] = tc.role
			servers[0].Labels["ksail.node.index"] = tc.index
			err := qualify(servers, nodes, true)
			if err == nil || !strings.Contains(err.Error(), "field=role role_expected=controlplane role_observed="+tc.observed+" index="+tc.indexing) {
				t.Fatalf("role mismatch lacks a bounded diagnosis: %v", err)
			}
			for _, private := range []string{"private-role-canary", "private-index-canary", nodes[0].Metadata.Name} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("diagnosis published an unreviewed value or identity")
				}
			}
		})
	}
}
