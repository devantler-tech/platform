package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestOwnershipFailureIdentifiesTheAssertionWithoutPublishingItsValue catches
// combined errors that hide the native cause, and diagnostics that leak labels.
func TestOwnershipFailureIdentifiesTheAssertionWithoutPublishingItsValue(t *testing.T) {
	for _, index := range []int{0, 3} {
		for _, tc := range []struct{ label, code string }{
			{"ksail.owned", "owned"},
			{"ksail.cluster.name", "cluster"},
			{"ksail.node.type", "role"},
			{"ksail.node.index", "index"},
			{"hcloud/node-group", "provider_pool"},
			{"storage.platform.devantler.tech/migration-pool", "node_pool"},
		} {
			t.Run(fmt.Sprintf("node%d/%s", index, tc.code), func(t *testing.T) {
				servers, nodes := roster(t, true)
				if tc.code == "node_pool" {
					nodes[index].Metadata.Labels[tc.label] = "private-label-canary"
				} else {
					servers[index].Labels[tc.label] = "private-label-canary"
				}
				err := qualify(servers, nodes, true)
				want := "static provider ownership does not match the production roster: field=" + tc.code
				if err == nil || err.Error() != want {
					t.Fatalf("failed ownership assertion was not identified: got=%v want=%s", err, want)
				}
				if strings.Contains(err.Error(), "private-label-canary") || strings.Contains(err.Error(), nodes[index].Metadata.Name) {
					t.Fatal("ownership diagnostic published a label value or node identity")
				}
			})
		}
	}
}
