package longhorninventory

import (
	"os"
	"testing"
)

func directory(name string) Entry {
	return Entry{Name: name, Mode: uint32(os.ModeDir | 0700), IsDir: true, Modified: 100}
}

func TestUnreportedReplicaDirectoryRemainsPreserved(t *testing.T) {
	scan := Listing{Complete: true, Entries: []Entry{directory("."), directory("current-data"), directory("unreported-old-data")}}
	report, err := Analyze(scan, []Replica{{Name: "current", UID: "current-uid", Directory: "current-data"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Matched != 1 || report.Unmatched != 1 || report.EmptyAndAccounted || report.Digest == "" || len(report.UnmatchedNames) != 1 || report.UnmatchedNames[0] != "unreported-old-data" {
		t.Fatalf("unreported on-disk data was cleared by an API-only view: %+v", report)
	}
}

func TestEmptyDirectoryDoesNotClearAMissingReplica(t *testing.T) {
	scan := Listing{Complete: true, Entries: []Entry{directory(".")}}
	report, err := Analyze(scan, []Replica{{Name: "missing", UID: "missing-uid", Directory: "missing-data"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Missing != 1 || report.EmptyAndAccounted {
		t.Fatal("missing expected data was treated as an empty accounted disk")
	}
	report, err = Analyze(scan, nil)
	if err != nil || !report.EmptyAndAccounted || report.Digest == "" {
		t.Fatal("complete empty observation was lost")
	}
}

func TestIncompleteAmbiguousAndEscapingInventoriesFailClosed(t *testing.T) {
	for _, scan := range []Listing{
		{},
		{Complete: true},
		{Complete: true, Entries: []Entry{directory("data")}},
		{Complete: true, Entries: []Entry{directory("."), directory("data"), directory("data")}},
		{Complete: true, Entries: []Entry{directory("."), directory("../outside")}},
		{Complete: true, Entries: []Entry{directory("."), directory("data/child")}},
		{Complete: true, Entries: []Entry{directory("."), {Name: "link", Mode: uint32(os.ModeSymlink | 0700)}}},
		{Complete: true, Entries: []Entry{directory("."), {Name: "inconsistent", Mode: 0600, IsDir: true}}},
		{Complete: true, Entries: []Entry{directory("."), {Name: "invalid-size", Mode: 0600, Size: -1}}},
	} {
		if _, err := Analyze(scan, nil); err == nil {
			t.Fatalf("unqualified native listing accepted: %+v", scan)
		}
	}
	valid := Listing{Complete: true, Entries: []Entry{directory("."), directory("data")}}
	if _, err := Analyze(valid, []Replica{{Name: "a", UID: "a-uid", Directory: "data"}, {Name: "b", UID: "b-uid", Directory: "data"}}); err == nil {
		t.Fatal("ambiguous replica ownership accepted")
	}
}
