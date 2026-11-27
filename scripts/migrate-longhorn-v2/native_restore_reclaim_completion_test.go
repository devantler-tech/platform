package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDatabaseArchiveRequiresTheCompletedReclaimForThisRestore(t *testing.T) {
	record, seed, state := nativeRestoreStateFixture(t, "completed")
	restore := nativeRestoreReclaimInputs{
		journal: benchmarkResource{Name: nativeRestoreJournal, Namespace: "longhorn-system", UID: "restore-journal-uid"},
		record:  record, seed: seed, state: state, executor: strings.Repeat("a", 40) + ":6789",
	}
	faults := []struct {
		name   string
		change func(object, *nativeRestoreReclaimProgress)
	}{
		{name: "completed reclaim"},
		{name: "retirement incomplete", change: func(_ object, p *nativeRestoreReclaimProgress) { p.Phase = "storage-retired" }},
		{name: "wrong restore", change: func(_ object, p *nativeRestoreReclaimProgress) { p.OriginalRun = "5679" }},
		{name: "wrong restore journal", change: func(_ object, p *nativeRestoreReclaimProgress) { p.RestoreJournalUID = "foreign-journal-uid" }},
		{name: "wrong executor", change: func(_ object, p *nativeRestoreReclaimProgress) { p.RestoreExecutor = "" }},
		{name: "same as restore run", change: func(_ object, p *nativeRestoreReclaimProgress) { p.Run = state.Run }},
		{name: "same as continuation run", change: func(_ object, p *nativeRestoreReclaimProgress) { p.Run = "6789" }},
		{name: "same as archive run", change: func(_ object, p *nativeRestoreReclaimProgress) { p.Run = "9999" }},
		{name: "invalid SHA", change: func(_ object, p *nativeRestoreReclaimProgress) { p.Source = "main" }},
		{name: "wrong ConfigMap name", change: func(m object, _ *nativeRestoreReclaimProgress) { m["name"] = "foreign" }},
		{name: "missing version", change: func(m object, _ *nativeRestoreReclaimProgress) { delete(m, "resourceVersion") }},
		{name: "deleting ConfigMap", change: func(m object, _ *nativeRestoreReclaimProgress) { m["deletionTimestamp"] = "2026-11-26T10:00:00Z" }},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			progress := nativeRestoreReclaimProgress{
				Source: strings.Repeat("b", 40), Run: "7890", Phase: "completed",
				OriginalRun: state.Run, RestoreJournalUID: restore.journal.UID, RestoreExecutor: restore.executor,
			}
			meta := object{"name": nativeRestoreReclaimJournal, "namespace": "longhorn-system", "uid": "reclaim-journal-uid", "resourceVersion": "1042"}
			if tc.change != nil {
				tc.change(meta, &progress)
			}
			raw, err := json.Marshal(progress)
			if err != nil {
				t.Fatal(err)
			}
			cm := object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta, "data": object{"reclaim": string(raw)}}
			got, err := decodeNativeRestoreReclaimCompletion(cm, restore, "9999")
			if (err == nil) != (tc.change == nil) {
				t.Fatalf("unexpected reclaim decision: %v", err)
			}
			if err == nil && (got.Run != progress.Run || got.Source != progress.Source) {
				t.Fatal("the successful Actions run could not be rebound")
			}
		})
	}
}
