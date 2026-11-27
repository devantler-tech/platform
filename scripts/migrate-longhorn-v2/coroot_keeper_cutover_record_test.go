package main

import "testing"

func TestKeeperMigrationRequiresFencedProgressAndPreservesBoundIdentities(t *testing.T) {
	staged := benchmarkResource{Name: "coroot-keeper-v2-data-991", UID: "b286e4f7-51db-4bc1-aa4d-65dd9cf592cb"}
	plan := corootKeeperCutoverRecord{
		Version: 1, Run: "991", Source: "acef1536f21e1b8cf32d62c6045fc989fa8e55d6",
		Input: corootKeeperCutoverInput{Ordinal: 1}, StagedClaim: staged, Phase: "member-fenced",
	}
	next := plan
	next.Phase = "claim-fenced-policy"
	next.ClaimFence.PolicyUID = "b4a63013-08ed-4bbf-9954-760fd678ed3f"
	if !validCorootKeeperProgress(plan, next) {
		t.Fatal("the member fence could not be followed by the claim fence")
	}
	next.Phase = "claim-release-requested"
	if validCorootKeeperProgress(plan, next) {
		t.Fatal("claim release skipped the copy")
	}
	next = plan
	next.Phase = "claim-fenced-policy"
	next.StagedClaim.UID = "70eaa3f7-5657-40d8-9e80-06f41925c4aa"
	if validCorootKeeperProgress(plan, next) {
		t.Fatal("the original prepared claim was replaced")
	}
	next = plan
	next.Phase = "original-claim-preserved"
	if !validCorootKeeperProgress(plan, next) {
		t.Fatal("rollback of a staged original claim was blocked")
	}
	plan.Phase = "claim-release-requested"
	if validCorootKeeperProgress(plan, next) {
		t.Fatal("a claim already entering release was accepted for automatic rollback")
	}
}
