package main

import "testing"

// The pinned Talos producer passes RoleControlPlane (control-plane) unchanged
// into NodeLabels and uses nodeIndex+1 for both the server name and label.
// The generic provider helper's controlplane constant is not used by that path.
func TestTalosProducerContractQualifiesTheCompleteCensus(t *testing.T) {
	servers, nodes := roster(t, true)
	for i := range 3 {
		servers[i].Labels["ksail.node.type"] = "control-plane"
	}
	result, err := classifyAccountCensus(servers, nodes)
	if err != nil || result.Servers != 9 || result.Nodes != 9 || result.Extra != 0 {
		t.Fatalf("the reviewed Talos producer contract did not qualify: result=%+v err=%v", result, err)
	}
}
