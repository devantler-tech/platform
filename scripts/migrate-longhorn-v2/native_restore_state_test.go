package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func nativeRestoreStateFixture(t *testing.T, phase string) (benchmarkRecord, canarySeedState, nativeRestoreState) {
	t.Helper()
	r, seed := seedStateFixture(t, "source-cleaned")
	digest, err := nativeRestoreSeedDigest(r, seed)
	if err != nil {
		t.Fatal(err)
	}
	targets, _, _ := nativeRestoreTargetsFixture()
	s := nativeRestoreState{Version: 1, Source: strings.Repeat("2", 40), Run: "5678", Phase: phase, SeedDigest: digest,
		SeedJournal: benchmarkResource{Name: nativeSeedJournal, Namespace: "longhorn-system", UID: "source-journal-uid"},
		Targets:     targets,
		Resources:   map[string]benchmarkResource{}}
	rank := nativeRestorePhaseIndex(phase)
	for i, step := range nativeRestorePhases {
		if i > rank {
			break
		}
		for _, kind := range step.resources {
			name, namespace := nativeRestoreResourceName(kind, s.Run)
			if name == "" {
				name = strings.ToLower(kind) + "-native-v2"
			}
			s.Resources[kind] = benchmarkResource{Name: name, Namespace: namespace, UID: "restore-" + kind + "-uid"}
		}
	}
	if rank >= nativeRestorePhaseIndex("restore-verified") {
		s.FirstNode = s.Targets[0].Name
	}
	if rank >= nativeRestorePhaseIndex("write-requested") {
		s.Token = strings.Repeat("e", 64)
	}
	if rank >= nativeRestorePhaseIndex("reattach-verified") {
		s.SecondNode = s.Targets[1].Name
	}
	if phase == "completed" {
		s.CompletedAt = "2026-11-15T10:00:00Z"
	}
	return r, seed, s
}

func TestNativeRestoreJournalRetainsBothConsumersAndTheOriginalSeed(t *testing.T) {
	for i, phase := range nativeRestorePhases {
		r, seed, state := nativeRestoreStateFixture(t, phase.name)
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeNativeRestoreState(string(raw), r, seed); err != nil {
			t.Fatal("the original-owned phase was refused", phase.name, err)
		}
		if i > 0 {
			_, _, before := nativeRestoreStateFixture(t, nativeRestorePhases[i-1].name)
			if err = validateNativeRestoreTransition(before, state, r, seed); err != nil {
				t.Fatal("the next original-owned step was refused", phase.name, err)
			}
		}
	}
	r, seed, completed := nativeRestoreStateFixture(t, "completed")
	if len(completed.Resources) != 10 || completed.Resources["FirstPod"].Name != completed.Resources["SecondPod"].Name ||
		completed.Resources["FirstPod"].UID == completed.Resources["SecondPod"].UID || completed.FirstNode == completed.SecondNode ||
		completed.validate(r, seed) != nil {
		t.Fatal("completion lost the first consumer when the same named Pod reattached")
	}
}

func TestNativeRestoreJournalRefusesForeignDataStorageConsumersAndTargets(t *testing.T) {
	for _, fault := range []string{"unknown phase", "old run", "bad source", "unfinished seed", "changed seed payload", "changed snapshot", "changed seed journal", "missing resource", "extra resource", "borrowed UID", "borrowed backend name", "aliased pods", "wrong claim", "wrong namespace", "bad resource name", "target alias", "target reordered", "missing target", "unlisted first node", "same second node", "missing token", "missing completion", "old completion", "borrowed completion"} {
		t.Run(fault, func(t *testing.T) {
			r, seed, s := nativeRestoreStateFixture(t, "completed")
			switch fault {
			case "unknown phase":
				s.Phase = "done"
			case "old run":
				s.Run = r.Run
			case "bad source":
				s.Source = "main"
			case "unfinished seed":
				r, seed = seedStateFixture(t, "backup-verified")
			case "changed seed payload":
				seed.PayloadSHA256 = strings.Repeat("f", 64)
			case "changed snapshot":
				seed.Snapshot = "other"
			case "changed seed journal":
				s.SeedJournal.Name = "other"
			case "missing resource":
				delete(s.Resources, "FirstPod")
			case "extra resource":
				s.Resources["Other"] = benchmarkResource{Name: "other", UID: "other-uid"}
			case "borrowed UID":
				s.Resources["LonghornVolume"] = r.Resources["LonghornVolume"]
			case "borrowed backend name":
				x := s.Resources["LonghornVolume"]
				x.Name = r.Resources["LonghornVolume"].Name
				s.Resources["LonghornVolume"] = x
			case "aliased pods":
				s.Resources["SecondPod"] = s.Resources["FirstPod"]
			case "wrong claim":
				x := s.Resources["PersistentVolumeClaim"]
				x.Name = "application-data"
				s.Resources["PersistentVolumeClaim"] = x
			case "wrong namespace":
				x := s.Resources["PodVolumeRestore"]
				x.Namespace = benchmarkNamespace
				s.Resources["PodVolumeRestore"] = x
			case "bad resource name":
				x := s.Resources["PodVolumeRestore"]
				x.Name = "../other"
				s.Resources["PodVolumeRestore"] = x
			case "target alias":
				s.Targets[1].UID = s.Targets[0].UID
			case "target reordered":
				s.Targets[1], s.Targets[0] = s.Targets[0], s.Targets[1]
			case "missing target":
				s.Targets = s.Targets[:2]
			case "unlisted first node":
				s.FirstNode = "other"
			case "same second node":
				s.SecondNode = s.FirstNode
			case "missing token":
				s.Token = ""
			case "missing completion":
				s.CompletedAt = ""
			case "old completion":
				s.CompletedAt = "2026-11-14T01:00:00Z"
			case "borrowed completion":
				s.CompletedAt = seed.CompletedAt
			}
			if s.validate(r, seed) == nil {
				t.Fatal("foreign or incomplete restore evidence was accepted:", fault)
			}
		})
	}
}

func TestNativeRestoreJournalCannotSkipIntentsOrReplaceRecordedEvidence(t *testing.T) {
	r, seed, before := nativeRestoreStateFixture(t, "write-requested")
	for _, fault := range []string{"skip", "backward", "same phase", "source", "run", "seed UID", "roster UID", "first Pod UID", "token", "first node"} {
		t.Run(fault, func(t *testing.T) {
			_, _, after := nativeRestoreStateFixture(t, "write-verified")
			switch fault {
			case "skip":
				_, _, after = nativeRestoreStateFixture(t, "first-pod-delete-requested")
			case "backward":
				_, _, after = nativeRestoreStateFixture(t, "restore-verified")
			case "same phase":
				after.Phase = before.Phase
			case "source":
				after.Source = strings.Repeat("3", 40)
			case "run":
				after.Run = "7890"
			case "seed UID":
				after.SeedJournal.UID = "replacement-uid"
			case "roster UID":
				after.Targets[0].UID = "replacement-node-uid"
			case "first Pod UID":
				x := after.Resources["FirstPod"]
				x.UID = "replacement-pod-uid"
				after.Resources["FirstPod"] = x
			case "token":
				after.Token = strings.Repeat("f", 64)
			case "first node":
				after.FirstNode = after.Targets[2].Name
			}
			if validateNativeRestoreTransition(before, after, r, seed) == nil {
				t.Fatal("a phase skip or replaced original receipt was accepted:", fault)
			}
		})
	}
}

func TestNativeRestoreJournalRequiresEveryDirectRequestIntent(t *testing.T) {
	for _, jump := range [][2]string{
		{"created", "policy-owned"},
		{"policy-owned", "class-owned"},
		{"class-owned", "claim-owned"},
		{"storage-bound", "modifier-owned"},
		{"modifier-owned", "restore-owned"},
		{"restore-verified", "write-verified"},
		{"write-verified", "first-pod-deleted"},
		{"first-pod-deleted", "reattach-owned"},
	} {
		r, seed, before := nativeRestoreStateFixture(t, jump[0])
		_, _, after := nativeRestoreStateFixture(t, jump[1])
		if validateNativeRestoreTransition(before, after, r, seed) == nil {
			t.Fatal("a direct mutation can finish without its recorded request:", jump)
		}
	}
}

func TestNativeRestoreJournalRejectsPrematureAndAmbiguousJSON(t *testing.T) {
	for phase, mutate := range map[string]func(*nativeRestoreState){
		"created": func(s *nativeRestoreState) {
			s.Resources["FirstPod"] = benchmarkResource{Name: "fio", Namespace: benchmarkNamespace, UID: "premature"}
		},
		"restore-owned":     func(s *nativeRestoreState) { s.FirstNode = s.Targets[0].Name },
		"restore-verified":  func(s *nativeRestoreState) { s.Token = strings.Repeat("f", 64) },
		"reattach-owned":    func(s *nativeRestoreState) { s.SecondNode = s.Targets[1].Name },
		"reattach-verified": func(s *nativeRestoreState) { s.CompletedAt = "2026-11-15T10:00:00Z" },
	} {
		r, seed, state := nativeRestoreStateFixture(t, phase)
		mutate(&state)
		if state.validate(r, seed) == nil {
			t.Fatal("a future phase's evidence appeared before its original intent:", phase)
		}
	}
	r, seed, state := nativeRestoreStateFixture(t, "created")
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"Version":1`, `"Version":1,"Version":1`, 1),
		strings.Replace(string(raw), `"Version":1`, `"Version":1,"version":1`, 1),
		strings.Replace(string(raw), `"Version":1`, `"Version":1,"Unknown":true`, 1),
		strings.Replace(string(raw), `"UID":"`+state.Targets[0].UID+`"`, `"UID":"`+state.Targets[0].UID+`","uid":"other"`, 1),
		string(raw) + ` {}`,
	} {
		if bad == string(raw) {
			t.Fatal("the negative JSON control did not alter the journal")
		}
		if _, err = decodeNativeRestoreState(bad, r, seed); err == nil {
			t.Fatal("ambiguous restore journal JSON was accepted")
		}
	}
}
