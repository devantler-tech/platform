package main

import "testing"

// KSail v7.209.0 NodeLabels and the Talos server-creation path use the
// provider role "controlplane", independently of Kubernetes' role-label spelling.
func TestPublishedProviderControlPlaneOwnership(t *testing.T) {
	servers, nodes := roster(t, true)
	if err := qualify(servers, nodes, true); err != nil {
		t.Fatalf("the published provider ownership contract was refused: %v", err)
	}
}

func TestProviderRoleAliasesDoNotEstablishOwnership(t *testing.T) {
	for _, tc := range []struct{ name, role string }{
		{"Kubernetes role spelling", "control-plane"},
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
