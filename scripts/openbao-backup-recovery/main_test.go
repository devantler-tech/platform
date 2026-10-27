package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSnapshotSelectionRejectsUnexaminedAndStaleEvidence fences real provider input.
func TestSnapshotSelectionRejectsUnexaminedAndStaleEvidence(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	entry := `{"status":"success","type":"file","key":"openbao-20260102-030430.snap","size":123,"lastModified":"2026-01-02T03:04:40Z"}`
	for _, tc := range []struct {
		name, input string
		fresh, want bool
	}{
		{"fresh matching object", entry, true, true},
		{"empty listing", "", false, false},
		{"provider failure", `{"status":"error","error":{"message":"private provider detail"}}`, false, false},
		{"outside prefix", strings.Replace(entry, "openbao-20260102-030430.snap", "../other.snap", 1), false, false},
		{"empty object", strings.Replace(entry, `"size":123`, `"size":0`, 1), false, false},
		{"oversized object", strings.Replace(entry, `"size":123`, `"size":9999999999`, 1), false, false},
		{"stale name", strings.Replace(entry, "20260102", "20250102", 1), true, false},
		{"stale upload", strings.Replace(entry, "2026-01-02T03:04:40Z", "2025-01-02T03:04:40Z", 1), true, false},
		{"unexamined suffix", entry + "\nnot-json", false, false},
		{"duplicate key", entry + "\n" + entry, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := selectSnapshot(strings.NewReader(tc.input), tc.fresh, start, end)
			if (err == nil) != tc.want {
				t.Fatalf("selection success=%v, want=%v", err == nil, tc.want)
			}
		})
	}
}

// TestRecordComparisonRequiresAllThreeCompleteValues rejects partial recovery.
func TestRecordComparisonRequiresAllThreeCompleteValues(t *testing.T) {
	expected := map[string]json.RawMessage{}
	for _, key := range credentialPaths {
		expected[key] = json.RawMessage(`{"username":"fixture","password":"synthetic-only","extra":"retained"}`)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]json.RawMessage)
		want   bool
	}{
		{"complete", func(map[string]json.RawMessage) {}, true},
		{"missing record", func(v map[string]json.RawMessage) { delete(v, credentialPaths[0]) }, false},
		{"different password", func(v map[string]json.RawMessage) {
			v[credentialPaths[1]] = json.RawMessage(`{"username":"fixture","password":"changed","extra":"retained"}`)
		}, false},
		{"lost extra field", func(v map[string]json.RawMessage) {
			v[credentialPaths[1]] = json.RawMessage(`{"username":"fixture","password":"synthetic-only"}`)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := map[string]json.RawMessage{}
			for k, v := range expected {
				actual[k] = v
			}
			tc.mutate(actual)
			if recordsEqual(expected, actual) != tc.want {
				t.Fatal("incorrect complete-record verdict")
			}
		})
	}
}

// TestPrivateInputRefusesSymlinksAndOversizedFiles protects the sandbox handoff.
func TestPrivateInputRefusesSymlinksAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "input")
	if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := privateInput(file, 32); err != nil {
		t.Fatal("valid private input refused")
	}
	if _, err := privateInput(file, 2); err == nil {
		t.Fatal("oversized private input accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := privateInput(link, 32); err == nil {
		t.Fatal("symlink accepted")
	}
}
