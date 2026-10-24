package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCommitsPrivateJournal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	unlock, err := lockStore(context.Background(), dir)
	if err != nil {
		t.Fatal("lock", err)
	}
	defer unlock()
	s, err := readJournal(dir)
	if err != nil {
		t.Fatal("read", err)
	}
	if err := saveJournal(dir, s); err != nil {
		t.Fatal("save", err)
	}
	for _, name := range []string{"state.json", "lock", "initialized"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private file mode", name, err)
		}
	}
}

func TestMissingPreviouslyInitializedJournalCannotDuplicateAnIncident(t *testing.T) {
	api := &fakeGitHub{}
	p := engine(t, t.TempDir(), api)
	if err := p.run(context.Background(), ptr(eventAt("firing", 0))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := p.run(context.Background(), ptr(eventAt("firing", 1))); !errors.Is(err, errStore) {
		t.Fatal("lost state was silently reinitialized", err)
	}
	if api.writes != 1 {
		t.Fatal("lost journal duplicated the incident")
	}
}

func TestCorruptOrPublicStateIsNeverReinitialized(t *testing.T) {
	for _, data := range []string{
		`{"version":2,"routes":{},"pending":[]}`,
		`{"version":1,"routes":{"cnpg-degraded":{"state":"injected"}},"pending":[]}`,
		`{"version":1,"routes":{},"pending":[],"attemptCount":-1}`,
		`{"version":1,"routes":{},"pending":[],"unknown":"private-canary"}`,
		`{"version":1,"routes":{},"pending":[]} {}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readJournal(dir); !errors.Is(err, errStore) {
			t.Fatal("corrupt journal accepted", err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":1,"routes":{},"pending":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(dir); !errors.Is(err, errStore) {
		t.Fatal("public journal accepted")
	}
}
