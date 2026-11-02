package backupcatalogue

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/logging"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

func repositoryFixture(t *testing.T, corrupt bool) (context.Context, repo.Repository, manifest.ID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(logging.WithLogger(context.Background(), nil), time.Minute)
	root := t.TempDir()
	options := &filesystem.Options{Path: filepath.Join(root, "blobs")}
	st, err := filesystem.New(ctx, options, true)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = repo.Initialize(ctx, st, nil, "fixture-password"); err != nil {
		cancel()
		t.Fatal(err)
	}
	lc := repo.LocalConfig{Storage: &blob.ConnectionInfo{Type: "filesystem", Config: options}, Caching: nil, ClientOptions: repo.ClientOptions{Hostname: "fixture-host", Username: "fixture-user", EnableActions: false}}
	data, err := json.Marshal(lc)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	config := filepath.Join(root, "repository.config")
	if err = os.WriteFile(config, data, 0600); err != nil {
		cancel()
		t.Fatal(err)
	}
	r, err := repo.Open(ctx, config, "fixture-password", &repo.Options{DisableRepositoryLog: true, DoNotWaitForUpgrade: true, OnFatalError: func(error) { cancel() }})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := r.Close(ctx); closeErr != nil {
			t.Error(closeErr)
		}
		cancel()
	})
	var id manifest.ID
	err = repo.WriteSession(ctx, r, repo.WriteSessionOptions{Purpose: "owned-test-fixture"}, func(ctx context.Context, w repo.RepositoryWriter) error {
		summary := &fs.DirectorySummary{}
		rootID, writeErr := snapshotfs.WriteDirManifest(ctx, w, "", &snapshot.DirManifest{StreamType: "kopia:directory", Entries: []*snapshot.DirEntry{}, Summary: summary}, "")
		if writeErr != nil {
			return writeErr
		}
		when := time.Date(2026, 6, 16, 9, 21, 54, 0, time.UTC)
		good := &snapshot.Manifest{Source: snapshot.SourceInfo{Host: "fixture-host", UserName: "fixture-user", Path: "/fixture"}, StartTime: fs.UTCTimestampFromTime(when), EndTime: fs.UTCTimestampFromTime(when.Add(time.Second)),
			RootEntry: &snapshot.DirEntry{Name: "fixture", Type: snapshot.EntryTypeDirectory, Permissions: 0700, ModTime: fs.UTCTimestampFromTime(when), ObjectID: rootID, DirSummary: summary}, Tags: map[string]string{"fixture-pvc-uid": "old-fixture"}}
		id, writeErr = snapshot.SaveSnapshot(ctx, w, good)
		if writeErr != nil || !corrupt {
			return writeErr
		}
		_, writeErr = w.PutManifest(ctx, map[string]string{manifest.TypeLabelKey: snapshot.ManifestType, snapshot.HostnameLabel: "fixture-host", snapshot.UsernameLabel: "fixture-user", snapshot.PathLabel: "/fixture"}, map[string]any{"startTime": "not-a-timestamp"})
		return writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, r, id
}

func TestCatalogueLoadsNativeManifestAndPreservesPrivateProvenance(t *testing.T) {
	ctx, r, id := repositoryFixture(t, false)
	c, err := readCatalogue(ctx, r)
	if err != nil || c.Snapshots != 1 || len(c.entries) != 1 || c.entries[0].Snapshot.ID != id || c.entries[0].Snapshot.Tags["fixture-pvc-uid"] != "old-fixture" || len(c.Digest) != 64 || !c.MetadataOnly {
		t.Fatalf("native snapshot catalogue not preserved: count=%d err=%v", c.Snapshots, err)
	}
	public, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{string(id), "fixture-host", "old-fixture", "/fixture"} {
		if strings.Contains(string(public), private) {
			t.Fatal("public receipt disclosed raw provenance")
		}
	}
}

func TestCatalogueFailsIfAnyNativeSnapshotCannotLoad(t *testing.T) {
	ctx, r, _ := repositoryFixture(t, true)
	c, err := readCatalogue(ctx, r)
	if err == nil || c.Snapshots != 0 || c.Digest != "" || len(c.entries) != 0 {
		t.Fatalf("corrupt manifest was silently omitted: count=%d err=%v", c.Snapshots, err)
	}
}

type noFindRepository struct {
	repo.Repository
	t *testing.T
}

func (r noFindRepository) FindManifests(context.Context, map[string]string) ([]*manifest.EntryMetadata, error) {
	r.t.Fatal("unsafe environment reached repository")
	return nil, nil
}

func TestCatalogueRejectsManifestSkipEnvironmentBeforeAccess(t *testing.T) {
	t.Setenv("KOPIA_IGNORE_MALFORMED_MANIFEST_CONTENTS", "true")
	if _, err := readCatalogue(t.Context(), noFindRepository{t: t}); err == nil {
		t.Fatal("manifest skip override accepted")
	}
}

type changingRepository struct {
	repo.Repository
	calls     int
	duplicate bool
}

func (r *changingRepository) FindManifests(ctx context.Context, labels map[string]string) ([]*manifest.EntryMetadata, error) {
	entries, err := r.Repository.FindManifests(ctx, labels)
	r.calls++
	if err == nil && len(entries) > 0 {
		if r.duplicate {
			entries = append(entries, entries[0])
		} else if r.calls > 1 {
			copyEntry := *entries[0]
			copyEntry.Length++
			entries[0] = &copyEntry
		}
	}
	return entries, err
}

func TestCatalogueRejectsDuplicateOrChangedNativeIndex(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		ctx, r, _ := repositoryFixture(t, false)
		c, err := readCatalogue(ctx, &changingRepository{Repository: r, duplicate: duplicate})
		if err == nil || c.Digest != "" || len(c.entries) != 0 {
			t.Fatalf("changed or duplicate manifest index accepted: duplicate=%v err=%v", duplicate, err)
		}
	}
}

func TestCatalogueRejectsAmbiguousNativeProvenance(t *testing.T) {
	for _, mode := range []string{"tag mismatch", "index-only tag", "reserved tag", "case alias", "duplicate field", "raw id"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, id := repositoryFixture(t, false)
			sm, err := snapshot.LoadSnapshot(ctx, r, id)
			if err != nil {
				t.Fatal(err)
			}
			sm.ID = ""
			labels := map[string]string{manifest.TypeLabelKey: snapshot.ManifestType, snapshot.HostnameLabel: sm.Source.Host, snapshot.UsernameLabel: sm.Source.UserName, snapshot.PathLabel: sm.Source.Path, "fixture-pvc-uid": "old-fixture"}
			var payload any = sm
			switch mode {
			case "tag mismatch":
				labels["fixture-pvc-uid"] = "different-index-uid"
			case "index-only tag":
				delete(sm.Tags, "fixture-pvc-uid")
			case "reserved tag":
				sm.Tags[snapshot.HostnameLabel] = sm.Source.Host
			case "raw id":
				sm.ID = "unrelated-id"
			case "case alias":
				data, marshalErr := json.Marshal(sm)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				var object map[string]any
				if err = json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				object["Source"] = object["source"]
				payload = object
			case "duplicate field":
				data, marshalErr := json.Marshal(sm)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				payload = json.RawMessage(append(bytes.TrimSuffix(data, []byte("}")), []byte(`,"tags":{"fixture-pvc-uid":"old-fixture"}}`)...))
			}
			if err = repo.WriteSession(ctx, r, repo.WriteSessionOptions{Purpose: "invalid-provenance-fixture"}, func(ctx context.Context, w repo.RepositoryWriter) error {
				_, writeErr := w.PutManifest(ctx, labels, payload)
				return writeErr
			}); err != nil {
				t.Fatal(err)
			}
			c, err := readCatalogue(ctx, r)
			if err == nil || c.Digest != "" || len(c.entries) != 0 {
				t.Fatalf("ambiguous provenance accepted: %s err=%v", mode, err)
			}
		})
	}
}
