package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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

func TestKeeperRebindResumeRequiresTheExactReleasedCohort(t *testing.T) {
	const uid = "b286e4f7-51db-4bc1-aa4d-65dd9cf592cb"
	const policy = "b4a63013-08ed-4bbf-9954-760fd678ed3f"
	const binding = "70eaa3f7-5657-40d8-9e80-06f41925c4aa"
	const source = "acef1536f21e1b8cf32d62c6045fc989fa8e55d6"
	for _, phase := range []string{"copy-bytes-verified", "copied-volumes-detached", "claim-release-requested", "destination-rebind-requested", "replacement-verified"} {
		for _, fault := range []string{"", "different ordinal", "different source", "stale fence", "executor missing source"} {
			t.Run(phase+"/"+fault, func(t *testing.T) {
				input := corootKeeperCutoverInput{Ordinal: 1, Digest: strings.Repeat("a", 64)}
				record := corootKeeperCutoverRecord{
					Version: 1, Run: "991", Source: source, Phase: phase, Input: input, FenceUser: "fixture-admin",
					PodFence: benchmarkFenceReceipt{PolicyUID: policy, BindingUID: binding}, ClaimFence: benchmarkFenceReceipt{PolicyUID: policy, BindingUID: binding},
				}
				record.Before.Disks[1].Digest = input.Digest
				record.Rebind.Original = record.Before.Disks[1]
				record.Rebind.StagedPV.UID = uid
				record.Rebind.StagedVolume.UID = uid
				annotation := record.Run + ":" + record.Source
				switch fault {
				case "different ordinal":
					record.Input.Ordinal = 2
				case "different source":
					annotation = record.Run + ":" + strings.Repeat("a", 40)
				case "stale fence":
					record.PodFence.BindingUID = ""
				case "executor missing source":
					record.ResumeRun = "993"
				}
				encoded, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				doc := object{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": object{"namespace": "longhorn-system", "name": corootKeeperJournalName(input.Ordinal),
						"resourceVersion": "11", "uid": uid, "annotations": object{corootKeeperFenceOwner: annotation}},
					"data": object{"record": string(encoded)},
				}
				_, err = bindCorootKeeperRebindResume(client{}, doc, input)
				allowed := phase != "copy-bytes-verified" && fault == ""
				if (err == nil) != allowed {
					t.Fatalf("unexpected Keeper resume result: %v", err)
				}
			})
		}
	}
}

func TestKeeperRollbackJournalAcceptsOnlyAnIntactOriginalBeforeRelease(t *testing.T) {
	const uid = "b286e4f7-51db-4bc1-aa4d-65dd9cf592cb"
	const source = "acef1536f21e1b8cf32d62c6045fc989fa8e55d6"
	input := corootKeeperCutoverInput{Ordinal: 1, Digest: strings.Repeat("a", 64)}
	for _, phase := range []string{"eviction-requested", "copy-permitted", "original-claim-preserved", "claim-release-requested", "unknown"} {
		for _, fault := range []string{"", "different digest", "new claim already created", "missing original claim", "different journal owner"} {
			t.Run(phase+"/"+fault, func(t *testing.T) {
				record := corootKeeperCutoverRecord{Version: 1, Run: "991", Source: source, Phase: phase, Input: input, FenceUser: "fixture-admin"}
				original := &record.Before.Disks[1]
				original.Digest = input.Digest
				original.Claim = benchmarkResource{Name: "keeper-original", Namespace: corootKeeperNamespace, UID: uid}
				original.PV = benchmarkResource{Name: "keeper-original-volume", UID: uid}
				record.Rebind.Original = *original
				owner := record.Run + ":" + record.Source
				switch fault {
				case "different digest":
					original.Digest = strings.Repeat("b", 64)
				case "new claim already created":
					record.NewClaim = benchmarkResource{Name: original.Claim.Name, Namespace: original.Claim.Namespace, UID: "70eaa3f7-5657-40d8-9e80-06f41925c4aa"}
				case "missing original claim":
					original.Claim.UID = ""
				case "different journal owner":
					owner = "990:" + record.Source
				}
				encoded, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				doc := object{"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": object{"namespace": "longhorn-system", "name": corootKeeperJournalName(input.Ordinal), "resourceVersion": "11",
						"uid": uid, "annotations": object{corootKeeperFenceOwner: owner}}, "data": object{"record": string(encoded)}}
				_, err = bindCorootKeeperRollback(client{}, doc, input)
				allowed := fault == "" && phase != "claim-release-requested" && phase != "unknown"
				if (err == nil) != allowed {
					t.Fatalf("unexpected rollback qualification: %v", err)
				}
			})
		}
	}
}
