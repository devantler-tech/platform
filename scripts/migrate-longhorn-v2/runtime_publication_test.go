package main

import (
	"strings"
	"testing"
)

func TestRuntimePublicationRequiresFreshCompleteOverlapBeforeFirstSelection(t *testing.T) {
	fit := runtimeInventoryReceipt{Nodes: 6, KubernetesNodes: 9, Pods: 200, InstanceManagers: 6,
		V1Volumes: 21, Default: "stock", EpochSHA: strings.Repeat("a", 64), DeclaredOverlap: "FIT", OverlapNodes: 6}
	for _, tc := range []struct {
		name, image, want string
		change            func(*runtimeInventoryReceipt)
	}{
		{"ordinary stock publication", "docker.io/longhornio/longhorn-instance-manager:v1.13.0", "NOT_REQUESTED", nil},
		{"first reviewed selection", runtimeRetirementImage, "FIT", nil},
		{"already selected", runtimeRetirementImage, "ALREADY_SELECTED", func(r *runtimeInventoryReceipt) {
			r.Default, r.DeclaredOverlap, r.OverlapNodes = "reviewed", "UNVERIFIED", 0
		}},
		{"request hold", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.DeclaredOverlap = "HOLD" }},
		{"unverified requests", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.DeclaredOverlap = "UNVERIFIED" }},
		{"partial storage coverage", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.OverlapNodes = 5 }},
		{"no storage nodes", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.Nodes, r.OverlapNodes = 0, 0 }},
		{"missing Kubernetes nodes", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.KubernetesNodes = 5 }},
		{"no processes", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.InstanceManagers = 0 }},
		{"no pods", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.Pods = 0 }},
		{"missing epoch", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.EpochSHA = "" }},
		{"malformed epoch", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.EpochSHA = strings.Repeat("z", 64) }},
		{"unknown live image", runtimeRetirementImage, "", func(r *runtimeInventoryReceipt) { r.Default = "other" }},
		{"unknown candidate image", "unreviewed/image:latest", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := fit
			if tc.change != nil {
				tc.change(&receipt)
			}
			got, err := runtimePublicationVerdict(tc.image, receipt)
			if tc.want == "" {
				if err == nil || got != "" {
					t.Fatal("publication was allowed without its required evidence")
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("publication verdict = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
