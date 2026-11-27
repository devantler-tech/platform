package main

import (
	"testing"
)

func TestKeeperCreationIntentReconcilesOnlyOwnedNativeAssets(t *testing.T) {
	const resourceUID = "b286e4f7-51db-4bc1-aa4d-65dd9cf592cb"
	const policyUID = "b4a63013-08ed-4bbf-9954-760fd678ed3f"
	const source = "acef1536f21e1b8cf32d62c6045fc989fa8e55d6"
	for index, phase := range keeperCutoverPhases {
		if !isKeeperAssetCreation(phase) {
			continue
		}
		t.Run(phase, func(t *testing.T) {
			base := corootKeeperCutoverRecord{
				Version: 1, Run: "991", Source: source, Phase: keeperCutoverPhases[index-1],
				FenceUser: "fixture-admin",
			}
			base.Before.Members[0] = corootKeeperMember{Pod: "coroot-keeper-0", Node: "worker-1"}
			base.Rebind.Original.Claim = benchmarkResource{
				Name: "data-coroot-keeper-0", Namespace: corootKeeperNamespace, UID: resourceUID,
			}
			base.Rebind.StagedPV.Name = "pvc-" + resourceUID
			if phase == "member-fenced" {
				base.PodFence.PolicyUID = policyUID
			}
			if phase == "claim-fenced" {
				base.ClaimFence.PolicyUID = policyUID
			}
			pending := base
			pending.Creating = phase
			if !validCorootKeeperProgress(base, pending) || !validKeeperCreationIntent(pending) {
				t.Fatal("the intended creation could not be durably recorded")
			}
			resuming := base
			resuming.Phase = keeperOriginalResuming
			if !validCorootKeeperProgress(base, resuming) {
				t.Fatal("rollback should be admitted before CREATE is recorded")
			}
			unsafe := pending
			unsafe.Phase = keeperOriginalResuming
			if validCorootKeeperProgress(pending, unsafe) {
				t.Fatal("rollback could skip the unresolved CREATE")
			}
			asset, err := keeperAssetFor(pending)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := keeperAssetFor(pending)
			if err != nil {
				t.Fatal(err)
			}
			metadata := valueObject(observed.body, "metadata")
			metadata["uid"], metadata["resourceVersion"] = resourceUID, "11"
			receipt, err := keeperAssetReceipt(pending, asset, observed.body)
			if err != nil || receipt.Creating != "" || receipt.Phase != phase || !validCorootKeeperProgress(pending, receipt) {
				t.Fatalf("the exact unreceipted object could not be reconciled: %v", err)
			}
			recordedUID := map[string]string{
				"staging-claim-created":  receipt.StagedClaim.UID,
				"staging-pod-created":    receipt.StagePod.UID,
				"member-fenced-policy":   receipt.PodFence.PolicyUID,
				"member-fenced":          receipt.PodFence.BindingUID,
				"claim-fenced-policy":    receipt.ClaimFence.PolicyUID,
				"claim-fenced":           receipt.ClaimFence.BindingUID,
				"copy-isolation-created": receipt.CopyIsolation.UID,
				"copy-gate-created":      receipt.CopyGate.UID,
				"copy-pod-created":       receipt.CopyPod.UID,
			}[phase]
			if recordedUID != resourceUID {
				t.Fatal("creation reconciliation did not capture the observed UID")
			}
			if !validCorootKeeperProgress(pending, base) {
				t.Fatal("a prior stopped run cannot clear an absent asset")
			}
			metadata["annotations"] = object{corootKeeperFenceOwner: "another run"}
			if _, err := keeperAssetReceipt(pending, asset, observed.body); err == nil {
				t.Fatal("a foreign object was adopted")
			}
			metadata["annotations"] = value(asset.body, "metadata", "annotations")
			if phase == "copy-gate-created" {
				valueObject(observed.body, "data")["state"] = "copy"
			} else {
				observed.body["spec"] = object{"unexpected": "identity"}
			}
			if _, err := keeperAssetReceipt(pending, asset, observed.body); err == nil {
				t.Fatal("the matching name did not preserve the reviewed object spec")
			}
		})
	}
}
