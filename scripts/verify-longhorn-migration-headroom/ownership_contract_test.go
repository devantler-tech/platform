package main

import "testing"

// KSail's pinned Talos producer passes its "control-plane" role directly to
// NodeLabels, preserving the one-based server-name index. Generic provider
// constants and read-side normalization do not change that producer contract.
func TestPublishedProviderControlPlaneOwnership(t *testing.T) {
	servers, nodes := roster(t, true)
	if err := qualify(servers, nodes, true); err != nil {
		t.Fatalf("the published provider ownership contract was refused: %v", err)
	}
}

func TestProviderRoleAliasesDoNotEstablishOwnership(t *testing.T) {
	for _, tc := range []struct{ name, role string }{
		{"generic provider role", "controlplane"},
		{"different case", "controlPlane"},
		{"worker role", "worker"},
		{"missing role", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			servers, nodes := roster(t, true)
			servers[0].Labels["ksail.node.type"] = tc.role
			if err := qualify(servers, nodes, true); err == nil {
				t.Fatal("a role outside the published ownership contract qualified")
			}
		})
	}
}
