package main

import "testing"

func TestOpenBaoCopyAssetsRequireClosedGateAndExactDeny(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	const uid = "1fcd18f0-a6eb-4e4f-87cf-d7d2824506b1"
	policy := openBaoCopyIsolation.denyPolicy(plan.Run, plan.Source, openBaoCopyFenceOwner)
	observed := openBaoCopyIsolation.denyPolicy(plan.Run, plan.Source, openBaoCopyFenceOwner)
	valueObject(observed, "metadata")["uid"] = uid
	valueObject(observed, "metadata")["resourceVersion"] = "23"
	if !matchOpenBaoNewCopyAsset(0, observed, policy, plan) {
		t.Fatal("exact created isolation was rejected")
	}
	delete(valueObject(observed, "spec"), "egressDeny")
	if matchOpenBaoNewCopyAsset(0, observed, policy, plan) {
		t.Fatal("the copy could proceed with egress allowed")
	}

	gate := openBaoCopyGate(plan.Run, plan.Source)
	observed = openBaoCopyGate(plan.Run, plan.Source)
	valueObject(observed, "metadata")["uid"] = uid
	valueObject(observed, "metadata")["resourceVersion"] = "24"
	if !matchOpenBaoNewCopyAsset(1, observed, gate, plan) {
		t.Fatal("exact closed gate was rejected")
	}
	valueObject(observed, "data")["state"] = plan.Run + ":" + plan.Source
	if matchOpenBaoNewCopyAsset(1, observed, gate, plan) {
		t.Fatal("an already open gate was accepted for a new copy")
	}
}
