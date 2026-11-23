package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func seedStateFixture(t *testing.T, phase string) (benchmarkRecord, canarySeedState) {
	t.Helper()
	r := nativeSeedRetirementFixture(t)
	s := canarySeedState{Version: 1, Source: r.Source, Run: r.Run, Phase: phase,
		Location: benchmarkResource{Name: "default", Namespace: "velero", UID: "location-uid"}, LocationSpec: strings.Repeat("a", 64),
		Backup: benchmarkResource{Name: "longhorn-v2-seed-" + r.Run, Namespace: "velero"}}
	rank := seedPhaseIndex(phase)
	if rank >= 2 {
		s.PayloadSHA256 = strings.Repeat("b", 64)
	}
	if rank >= 4 {
		s.Backup.UID = "backup-uid"
	}
	if rank >= 5 {
		s.Transfer = benchmarkResource{Name: "native-transfer", Namespace: "velero", UID: "transfer-uid"}
		s.Repository = benchmarkResource{Name: "native-repository", Namespace: "velero", UID: "repository-uid"}
		s.RepositorySpec = strings.Repeat("c", 64)
		s.Snapshot = "snapshot-id"
	}
	if rank >= 7 {
		s.CompletedAt = "2026-11-15T05:00:00Z"
		r.FenceCheckpoint = r.Run + "-" + strings.Repeat("d", 32)
		r.Fences = map[string]benchmarkFenceReceipt{}
		for i, name := range benchmarkFenceNames("v1", r.Run) {
			suffix := strconv.Itoa(i)
			r.Fences[name] = benchmarkFenceReceipt{PolicyUID: "source-policy-" + suffix, BindingUID: "source-binding-" + suffix}
		}
	}
	return r, s
}

func TestCanarySeedStateRetainsOnlyItsOriginalTransferAndRetirementOwner(t *testing.T) {
	for _, phase := range seedPhases {
		r, s := seedStateFixture(t, phase)
		body, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeCanarySeedState(string(body), r)
		if err != nil || got != s {
			t.Fatalf("phase %s lost its original evidence: %v", phase, err)
		}
	}
	for _, fault := range []string{"unknown phase", "different source", "different run", "missing location", "missing payload", "missing backup", "missing transfer", "missing repository", "missing snapshot", "duplicate UID", "claimed benchmark", "missing completion"} {
		t.Run(fault, func(t *testing.T) {
			r, s := seedStateFixture(t, "source-cleaned")
			switch fault {
			case "unknown phase":
				s.Phase = "done"
			case "different source":
				s.Source = strings.Repeat("2", 40)
			case "different run":
				s.Run = "5678"
			case "missing location":
				s.Location.UID = ""
			case "missing payload":
				s.PayloadSHA256 = ""
			case "missing backup":
				s.Backup.UID = ""
			case "missing transfer":
				s.Transfer.UID = ""
			case "missing repository":
				s.RepositorySpec = ""
			case "missing snapshot":
				s.Snapshot = ""
			case "duplicate UID":
				s.Transfer.UID = s.Repository.UID
			case "claimed benchmark":
				r.Workload = benchmarkWorkload()
			case "missing completion":
				s.CompletedAt = ""
			}
			body, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = decodeCanarySeedState(string(body), r); err == nil {
				t.Fatal("foreign or incomplete seed evidence was accepted")
			}
		})
	}
}

func TestCanarySeedStateRejectsAliasedDuplicateAndUnknownJSON(t *testing.T) {
	r, s := seedStateFixture(t, "created")
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(string(body), `"Version":1`, `"Version":1,"Version":1`, 1),
		strings.Replace(string(body), `"Version":1`, `"Version":1,"version":1`, 1),
		strings.Replace(string(body), `"Version":1`, `"Version":1,"Unknown":true`, 1),
		string(body) + ` {}`,
	} {
		if _, err = decodeCanarySeedState(raw, r); err == nil {
			t.Fatal("ambiguous seed JSON was accepted")
		}
	}
}

func TestCanarySeedTransitionsCannotSkipProofsOrReplaceCapturedEvidence(t *testing.T) {
	for i := 1; i < len(seedPhases); i++ {
		_, before := seedStateFixture(t, seedPhases[i-1])
		r, after := seedStateFixture(t, seedPhases[i])
		if err := validateSeedTransition(before, after, r); err != nil {
			t.Fatal("the next original-owned step was refused", err)
		}
	}
	r, before := seedStateFixture(t, "backup-verified")
	for _, fault := range []string{"skip", "backward", "replace backup", "replace repository", "different payload", "different location"} {
		t.Run(fault, func(t *testing.T) {
			_, after := seedStateFixture(t, "source-retirement-requested")
			switch fault {
			case "skip":
				after.Phase = "source-cleaned"
				after.CompletedAt = "2026-11-15T05:00:00Z"
			case "backward":
				after.Phase = "backup-owned"
			case "replace backup":
				after.Backup.UID = "replacement"
			case "replace repository":
				after.Repository.UID = "replacement"
			case "different payload":
				after.PayloadSHA256 = strings.Repeat("d", 64)
			case "different location":
				after.LocationSpec = strings.Repeat("d", 64)
			}
			if err := validateSeedTransition(before, after, r); err == nil {
				t.Fatal("a transition skipped proof or rewrote an original identity")
			}
		})
	}
}

func TestCanarySeedSourceCleanupRequiresTheOriginalCompleteRetirementReceipts(t *testing.T) {
	for _, fault := range []string{"", "missing fences", "missing binding", "missing checkpoint", "pending fence", "borrowed UID"} {
		t.Run(fault, func(t *testing.T) {
			r, seed := seedStateFixture(t, "source-cleaned")
			name := benchmarkFenceNames("v1", r.Run)[0]
			switch fault {
			case "missing fences":
				r.Fences = nil
			case "missing binding":
				fence := r.Fences[name]
				fence.BindingUID = ""
				r.Fences[name] = fence
			case "missing checkpoint":
				r.FenceCheckpoint = ""
			case "pending fence":
				r.FencePending = name + "/binding"
			case "borrowed UID":
				fence := r.Fences[name]
				fence.BindingUID = r.Resources["PersistentVolume"].UID
				r.Fences[name] = fence
			}
			if err := seed.validate(r); (err == nil) != (fault == "") {
				t.Fatal("source-cleaned was accepted without its completed original retirement evidence:", fault, err)
			}
		})
	}
}
