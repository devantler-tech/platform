package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func temporaryJournalDocument(t *testing.T) object {
	t.Helper()
	data, err := json.Marshal(journalFixture())
	if err != nil {
		t.Fatal(err)
	}
	var document object
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["schema"] = float64(2)
	document["target"] = "autoscale-longhorn-v1-0123456789abcdef"
	document["originalDisks"] = object{}
	document["temporarySelection"] = "temporary-v1-1"
	document["nativeEmptyDigest"] = strings.Repeat("a", 64)
	document["originalBootID"] = "12345678-1234-4234-8234-123456789abc"
	document["rawVolumeAbsent"] = true
	return document
}

func TestLegacyPreparationCannotAdoptAnEmptyTemporaryJournal(t *testing.T) {
	document := temporaryJournalDocument(t)
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	storage := resource("longhorn-v2-prepare-autoscale-longhorn-v1-0123456789abcdef")
	storage["data"] = object{"record": string(data)}
	p := preparation{target: text(document, "target"), name: text(storage, "metadata", "name"), c: client{context: "admin@prod", run: func(_ context.Context, input []byte, program string, args ...string) ([]byte, error) {
		if input != nil || program != "kubectl" || len(args) < 3 || args[2] != "get" {
			t.Fatal("journal inspection escaped its read scope")
		}
		return json.Marshal(storage)
	}}}
	if _, _, err = p.readRecord(context.Background()); err == nil {
		t.Fatal("the permanent-worker preparation adopted a temporary reset contract")
	}
}

func TestTemporaryJournalRequiresItsOwnExplicitCallerAndJournalName(t *testing.T) {
	record := decodeTemporaryJournal(t, temporaryJournalDocument(t))
	p := preparation{target: record.Target, name: "longhorn-v2-prepare-temporary-v1-1", temporarySelection: "temporary-v1-1"}
	if err := p.validateRecord(record); err != nil {
		t.Fatal("matching temporary caller rejected:", err)
	}
	p.name = "longhorn-v2-prepare-temporary-v1-2"
	if p.validateRecord(record) == nil {
		t.Fatal("another slot's journal was adopted")
	}
	p.name, p.temporarySelection = "longhorn-v2-prepare-temporary-v1-1", "temporary-v1-2"
	if p.validateRecord(record) == nil {
		t.Fatal("another explicitly selected slot was adopted")
	}
	legacy := journalFixture()
	p.target, p.name = legacy.Target, "longhorn-v2-prepare-"+legacy.Target
	if p.validateRecord(legacy) == nil {
		t.Fatal("a temporary caller adopted the permanent-worker reset profile")
	}
}

func decodeTemporaryJournal(t *testing.T, document object) journal {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var record journal
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestEmptyTemporaryJournalKeepsASeparateResetContract(t *testing.T) {
	document := temporaryJournalDocument(t)
	record := decodeTemporaryJournal(t, document)
	if err := validateJournal(record, record.Target); err != nil {
		t.Fatalf("positively bound empty temporary reset cannot be journaled: %v", err)
	}
	for name, mutate := range map[string]func(object){
		"legacy schema":            func(d object) { d["schema"] = float64(1) },
		"permanent target":         func(d object) { d["target"] = "prod-worker-1" },
		"fresh V2 pool":            func(d object) { d["target"] = "autoscale-longhorn-v2-0123456789abcdef" },
		"ordinary compute":         func(d object) { d["target"] = "autoscale-cx43-0123456789abcdef" },
		"unknown slot":             func(d object) { d["temporarySelection"] = "temporary-v1-3" },
		"missing physical proof":   func(d object) { delete(d, "nativeEmptyDigest") },
		"malformed physical proof": func(d object) { d["nativeEmptyDigest"] = "empty" },
		"missing boot identity":    func(d object) { delete(d, "originalBootID") },
		"raw state unknown":        func(d object) { delete(d, "rawVolumeAbsent") },
		"raw volume exists":        func(d object) { d["rawVolumeAbsent"] = false },
		"registered old disk":      func(d object) { d["originalDisks"] = journalFixture().OriginalDisks },
	} {
		t.Run(name, func(t *testing.T) {
			document := temporaryJournalDocument(t)
			mutate(document)
			record := decodeTemporaryJournal(t, document)
			if err := validateJournal(record, record.Target); err == nil {
				t.Fatal("an unbound or data-bearing reset was accepted")
			}
		})
	}
	legacy := journalFixture()
	if err := validateJournal(legacy, legacy.Target); err != nil {
		t.Fatal("legacy permanent-worker contract changed:", err)
	}
}
