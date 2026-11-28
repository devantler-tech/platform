package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

func TestOpenBaoPairPlanBindsTheTwoOriginalDisks(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOpenBaoPairPlan(plan, plan.Run, plan.Source); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"run", "source", "audit handle", "other member", "same survivor"} {
		t.Run(fault, func(t *testing.T) {
			var changed openBaoPairPlan
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "run":
				changed.Run = "123456"
			case "source":
				changed.Source = "1234567890000000000000000000000000000000"
			case "audit handle":
				changed.Original.Disks[1].ProviderHandle = "400"
			case "other member":
				changed.Input.Member = "openbao-2"
			case "same survivor":
				changed.Survivors[0] = changed.Survivors[1]
			}
			if err := verifyOpenBaoPairPlan(changed, plan.Run, plan.Source); err == nil {
				t.Fatal("unverified plan was accepted")
			}
		})
	}
}

func TestOpenBaoJournalDoesNotAllowItsCopyOrReleaseToSkipAPhase(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	record := openBaoPairRecord{Plan: plan, Phase: openBaoPairPlanned}
	pair := openBaoReboundPairFixture(t)
	assets := [3]benchmarkResource{
		pair[0].Intent.StagingClaim, pair[1].Intent.StagingClaim,
		{Name: "lh-openbao-stage-" + plan.Run, Namespace: openBaoNamespace, UID: "fc291be3-5675-44e4-a5bf-45c3a1fa3a50"},
	}
	for i, name := range openBaoStageAssetKinds {
		intent := record
		intent.StagePending = name
		if err := verifyOpenBaoPairTransition(record, intent); err != nil {
			t.Fatal(err)
		}
		created := intent
		created.StagePending, created.StageAssets[i] = "", assets[i]
		if err := verifyOpenBaoPairTransition(record, created); err == nil {
			t.Fatal("a stage asset was recorded without its creation intent")
		}
		if err := verifyOpenBaoPairTransition(intent, created); err != nil {
			t.Fatal(err)
		}
		record = created
	}
	next := record
	next.Phase = openBaoPairStaged
	next.Destinations = [2]openBaoStorageRebind{pair[0].Intent, pair[1].Intent}
	if err := verifyOpenBaoPairTransition(record, next); err != nil {
		t.Fatal(err)
	}
	staged := next
	next.Evicting = true
	if err := verifyOpenBaoPairTransition(staged, next); err == nil {
		t.Fatal("an eviction was accepted before both admission fences were recorded")
	}
	next = staged
	next.FenceUser = "fixture-admin"
	if err := verifyOpenBaoPairTransition(staged, next); err != nil {
		t.Fatal(err)
	}
	staged = next
	pods, claims := "lh-openbao-storage-"+plan.Run, "lh-openbao-claims-"+plan.Run
	fences := [4]benchmarkResource{
		{Name: pods, UID: "5e9c845a-9019-4979-b76e-74012bc03270"},
		{Name: pods, UID: "06bccfda-06da-4e88-b52c-906b9911f515"},
		{Name: claims, UID: "e10901f3-f8bb-42a5-b92c-92120c0d790d"},
		{Name: claims, UID: "926cc031-9c8c-4941-a9ab-40935d746f6c"},
	}
	for index, kind := range openBaoFenceAssetKinds {
		intent := staged
		intent.FencePending = kind
		if err := verifyOpenBaoPairTransition(staged, intent); err != nil {
			t.Fatal(err)
		}
		next = intent
		next.FencePending, next.FenceAssets[index] = "", fences[index]
		if err := verifyOpenBaoPairTransition(intent, next); err != nil {
			t.Fatal(err)
		}
		staged = next
	}
	next.Evicting = true
	if err := verifyOpenBaoPairTransition(staged, next); err != nil {
		t.Fatal(err)
	}
	staged = next
	copyAssets := [3]benchmarkResource{
		{Name: openBaoCopyIsolation.Prefix + plan.Run, Namespace: openBaoNamespace, UID: "1fcd18f0-a6eb-4e4f-87cf-d7d2824506b1"},
		{Name: openBaoCopyGatePrefix + plan.Run, Namespace: openBaoNamespace, UID: "a3ac80b9-0450-4b7e-b068-bb345e66fe3c"},
		{Name: "lh-openbao-copy-" + plan.Run, Namespace: openBaoNamespace, UID: "e9d399c5-9614-41c0-bc1a-e07fb34bce02"},
	}
	for i, name := range openBaoCopyAssetKinds {
		intent := staged
		intent.CopyPending = name
		if err := verifyOpenBaoPairTransition(staged, intent); err != nil {
			t.Fatal(err)
		}
		if err := verifyOpenBaoPairTransition(intent, staged); err != nil {
			t.Errorf("a confirmed absent %s must allow the pending copy intent to clear: %v", name, err)
		}
		t.Run("recover "+name, func(t *testing.T) {
			testPendingOpenBaoCopy(t, intent, i, copyAssets[i])
		})
		created := intent
		created.CopyPending, created.CopyAssets[i] = "", copyAssets[i]
		if err := verifyOpenBaoPairTransition(intent, created); err != nil {
			t.Fatal(err)
		}
		staged = created
	}
	t.Run("copy scheduling", func(t *testing.T) {
		testOpenBaoCopyGateWaitsForScheduling(t, staged)
	})
	next = staged
	next.Phase = openBaoPairSourcesReleased
	if err := verifyOpenBaoPairTransition(staged, next); err == nil {
		t.Fatal("the source phase advanced before the offline copy was verified")
	}
	next.Phase = openBaoPairCopied
	next.Destinations[0].DestinationVolume.UID = pair[1].Intent.DestinationVolume.UID
	if err := verifyOpenBaoPairTransition(staged, next); err == nil {
		t.Fatal("a changed staging backend was accepted")
	}
	next.Destinations = staged.Destinations
	if err := verifyOpenBaoPairTransition(staged, next); err == nil {
		t.Fatal("the copy phase needs a recorded isolated copy identity")
	}
	next.CopyPod = copyAssets[2]
	if err := verifyOpenBaoPairTransition(staged, next); err != nil {
		t.Fatal(err)
	}
	copied := next
	next.Phase = openBaoPairSourcesReleased
	if err := verifyOpenBaoPairTransition(copied, next); err == nil {
		t.Fatal("both claim releases need a durable intent")
	}
	next.Phase = openBaoPairReleasing
	if err := verifyOpenBaoPairTransition(copied, next); err != nil {
		t.Fatal(err)
	}
	releasing := next
	next.Phase = openBaoPairSourcesReleased
	if err := verifyOpenBaoPairTransition(releasing, next); err != nil {
		t.Fatal(err)
	}
	released := next
	next.RolledBack = true
	if err := verifyOpenBaoPairTransition(released, next); err == nil {
		t.Fatal("the original member could be resumed after its claims had been released")
	}
	next = released
	next.Successors[0] = benchmarkIdentity(pair[0].NewClaim)
	if err := verifyOpenBaoPairTransition(released, next); err == nil {
		t.Fatal("a successor was recorded before its creation intent")
	}
	for index, destination := range pair {
		intent := released
		intent.ClaimPending = index + 1
		if err := verifyOpenBaoPairTransition(released, intent); err != nil {
			t.Fatal(err)
		}
		next = intent
		next.ClaimPending, next.Successors[index] = 0, benchmarkIdentity(destination.NewClaim)
		if err := verifyOpenBaoPairTransition(intent, next); err != nil {
			t.Fatal(err)
		}
		released = next
	}
	next.Phase = openBaoPairRebound
	if err := verifyOpenBaoPairTransition(released, next); err != nil {
		t.Fatal(err)
	}
}

func testPendingOpenBaoCopy(t *testing.T, intent openBaoPairRecord, index int, identity benchmarkResource) {
	t.Helper()
	for _, outcome := range []string{"found", "absent", "foreign", "read error"} {
		t.Run(outcome, func(t *testing.T) {
			plan := intent.Plan
			desired := desiredOpenBaoCopyAssets(plan)[index]
			metadata := valueObject(desired, "metadata")
			metadata["uid"], metadata["resourceVersion"] = identity.UID, "21"
			if outcome == "foreign" {
				valueObject(metadata, "annotations")[openBaoCopyFenceOwner] = "another-run"
			}
			raw, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			journalName := openBaoPairJournalName(plan.Input.Member)
			journal := object{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": object{"name": journalName, "namespace": openBaoPairJournalNamespace,
					"uid": "f0852bc2-9061-486f-bb48-7c21e12e30a5", "resourceVersion": "100",
					"annotations": object{openBaoCopyFenceOwner: plan.Run + ":" + plan.Source}},
				"data": object{"record": string(raw)},
			}
			pendingReads, journalPatches := 0, 0
			c := client{context: "admin@prod", run: func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
				if program == "git" {
					return []byte(plan.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 5 || args[0] != "--context=admin@prod" {
					t.Fatalf("unexpected command %s %v", program, args)
				}
				switch {
				case args[2] == "get" && args[4] == identity.Name:
					want := [3]string{"ciliumnetworkpolicies.cilium.io", "configmap", "pod"}[index]
					if args[3] != want || !slices.Contains(args, "--namespace="+openBaoNamespace) ||
						!slices.Contains(args, "--ignore-not-found") {
						t.Fatalf("the pending read was not bound: %v", args)
					}
					pendingReads++
					switch outcome {
					case "absent":
						return nil, nil
					case "read error":
						return nil, errors.New("API unavailable")
					default:
						return json.Marshal(desired)
					}
				case args[2] == "get" && args[4] == journalName:
				case args[2] == "patch" && args[4] == journalName:
					journalPatches++
					var patch []object
					if err := json.Unmarshal(body, &patch); err != nil || len(patch) != 4 {
						t.Fatalf("invalid journal CAS: %v", err)
					}
					valueObject(journal, "data")["record"] = text(patch[3], "value")
				default:
					t.Fatalf("unexpected kubectl command %v", args)
				}
				return json.Marshal(journal)
			}}
			j := openBaoPairJournal{client: c, id: benchmarkIdentity(journal), record: intent}
			err = j.reconcilePendingCopy(t.Context())
			if outcome == "foreign" || outcome == "read error" {
				if err == nil || pendingReads != 1 || journalPatches != 0 || j.record != intent {
					t.Fatalf("unverified object changed the journal: %v", err)
				}
				return
			}
			want := intent
			want.CopyPending = ""
			if outcome == "found" {
				want.CopyAssets[index] = identity
			}
			if err != nil || pendingReads != 1 || journalPatches != 1 || j.record != want {
				t.Fatalf("pending asset was not reconciled exactly: reads=%d patches=%d error=%v", pendingReads, journalPatches, err)
			}
		})
	}
}

func TestOpenBaoJournalBindsReadbackAndCompareAndSwap(t *testing.T) {
	for _, outcome := range []string{"success", "changed identity", "stale readback"} {
		t.Run(outcome, func(t *testing.T) {
			plan := openBaoPairPlanFixture(t)
			ctx := context.Background()
			var stored object
			patches := 0
			runner := func(_ context.Context, body []byte, program string, args ...string) ([]byte, error) {
				if program == "git" && len(args) > 0 && args[0] == "ls-remote" {
					return []byte(plan.Source + "\trefs/heads/main\n"), nil
				}
				if program != "kubectl" || len(args) < 4 || args[0] != "--context=admin@prod" {
					t.Fatalf("unexpected command: %s %v", program, args)
				}
				switch args[2] {
				case "create":
					if err := json.Unmarshal(body, &stored); err != nil {
						t.Fatal(err)
					}
					valueObject(stored, "metadata")["uid"] = "f0852bc2-9061-486f-bb48-7c21e12e30a5"
					valueObject(stored, "metadata")["resourceVersion"] = "100"
				case "get":
					if !slices.Contains(args, "--namespace=longhorn-system") {
						t.Fatal("journal read was not namespace-scoped")
					}
				case "patch":
					patches++
					var changes []object
					if err := json.Unmarshal(body, &changes); err != nil {
						t.Fatal(err)
					}
					if len(changes) != 4 || text(changes[0], "path") != "/metadata/uid" ||
						text(changes[0], "value") != text(stored, "metadata", "uid") ||
						text(changes[1], "path") != "/metadata/resourceVersion" || text(changes[1], "value") != "100" ||
						text(changes[2], "op") != "test" || text(changes[2], "path") != "/data/record" ||
						text(changes[2], "value") != text(stored, "data", "record") ||
						text(changes[3], "op") != "replace" || text(changes[3], "path") != "/data/record" {
						t.Fatal("journal patch did not bind UID, resource version and exact previous record")
					}
					if outcome != "stale readback" {
						valueObject(stored, "data")["record"] = text(changes[3], "value")
					}
				default:
					t.Fatalf("unexpected kubectl command: %v", args)
				}
				return json.Marshal(stored)
			}
			journal, err := beginOpenBaoPairJournal(ctx, client{run: runner, context: "admin@prod"}, plan, [3]benchmarkResource{})
			if err != nil {
				t.Fatal(err)
			}
			previous := journal.record
			next := previous
			next.StagePending = "data"
			if outcome == "changed identity" {
				valueObject(stored, "metadata")["uid"] = "e4acb67f-e386-4283-b498-1b79d49edaf3"
			}
			err = journal.advance(ctx, next)
			switch outcome {
			case "success":
				if err != nil || journal.record != next || patches != 1 {
					t.Fatalf("the exact written stage did not advance: %v", err)
				}
			case "changed identity":
				if err == nil || journal.record != previous || patches != 0 {
					t.Fatal("a replaced journal permitted a write")
				}
			case "stale readback":
				if err == nil || journal.record != previous || patches != 1 {
					t.Fatal("an unconfirmed journal write advanced in memory")
				}
			}
		})
	}
}

func TestOpenBaoRetryOnlyUsesAnOwnedPairFromBeforeEviction(t *testing.T) {
	plan := openBaoPairPlanFixture(t)
	pair := openBaoReboundPairFixture(t)
	previous := openBaoPairRecord{
		Plan: plan, Phase: openBaoPairStaged, RolledBack: true,
		Destinations: [2]openBaoStorageRebind{pair[0].Intent, pair[1].Intent},
		StageAssets: [3]benchmarkResource{
			pair[0].Intent.StagingClaim, pair[1].Intent.StagingClaim,
			{Name: "lh-openbao-stage-" + plan.Run, Namespace: openBaoNamespace, UID: "fc291be3-5675-44e4-a5bf-45c3a1fa3a50"},
		},
	}
	if !previous.valid() {
		t.Fatal("invalid previous attempt fixture")
	}
	for _, problem := range []string{"", "unrolled", "evicted", "uncertain copy", "wrong dispatch", "same run", "foreign original"} {
		t.Run(problem, func(t *testing.T) {
			old, next, confirmed := previous, plan, plan.Run
			next.Run = "987654321"
			switch problem {
			case "unrolled":
				old.RolledBack = false
			case "evicted":
				old.Evicting = true
			case "uncertain copy":
				old.CopyPending = "pod"
			case "wrong dispatch":
				confirmed = "986"
			case "same run":
				next.Run = old.Plan.Run
			case "foreign original":
				next.Original.Disks[0].PVC.UID = "ea7ae5dd-dc8b-4207-a89b-a2e6ba5d3412"
			}
			bound, assets, err := bindOpenBaoStageReuse(old, next, confirmed)
			if (err == nil) != (problem == "") {
				t.Fatalf("bound=%t: %v", problem == "", err)
			}
			if err == nil && (!(openBaoPairRecord{Plan: bound, Phase: openBaoPairPlanned, StageAssets: assets}).valid() ||
				assets[0] != pair[0].Intent.StagingClaim || assets[1] != pair[1].Intent.StagingClaim || assets[2] != (benchmarkResource{})) {
				t.Fatal("retry did not carry the exact retained claims while starting a fresh verification pod")
			}
			if err == nil {
				for _, pod := range []object{bound.stagePod(), bound.copyPod()} {
					if text(pod, "metadata", "annotations", openBaoCopyFenceOwner) != next.Run+":"+next.Source {
						t.Fatal("a retry pod retained the previous run's authority")
					}
					volumes := value(pod, "spec", "volumes").([]any)
					for i := range 2 {
						if text(volumes[i].(object), "persistentVolumeClaim", "claimName") != assets[i].Name {
							t.Fatal("a retry pod did not reference the journaled staging claim")
						}
					}
				}
			}
		})
	}
}

func openBaoPairPlanFixture(t *testing.T) openBaoPairPlan {
	t.Helper()
	stateful, pods, claims, pvs, attachments := openBaoProviderStorageFixture()
	original, err := bindOpenBaoProviderStorage("openbao-1", stateful, pods, claims, pvs, attachments)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := openBaoProviderStorageDigest(original)
	if err != nil {
		t.Fatal(err)
	}
	pair := openBaoReboundPairFixture(t)
	return openBaoPairPlan{
		Run: pair[0].Intent.Run, Source: pair[0].Intent.SourceSHA, Input: openBaoPairInput{Member: "openbao-1", Digest: digest},
		Original: original,
		Survivors: [2]openBaoSurvivingMember{
			{Pod: benchmarkResource{Name: "openbao-0", Namespace: openBaoNamespace, UID: "f393217d-805a-4c16-a6ae-8a7bf83844c3"},
				Node: benchmarkResource{Name: "worker-2", UID: "a77fb2d1-362d-413c-b22c-34ebf244406b"}},
			{Pod: benchmarkResource{Name: "openbao-2", Namespace: openBaoNamespace, UID: "043920f8-77d6-4d34-a159-523428bd117e"},
				Node: benchmarkResource{Name: "worker-3", UID: "c02e7ffd-16b6-405b-8389-bed3b1857b15"}},
		},
	}
}
