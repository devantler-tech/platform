package backupcatalogue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/devantler-tech/platform/internal/strictjson"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
)

type catalogueEntry struct {
	Metadata manifest.EntryMetadata
	Snapshot snapshot.Manifest
	Raw      json.RawMessage
}
type Catalogue struct {
	MetadataOnly     bool   `json:"metadataOnly"`
	Snapshots        int    `json:"snapshots"`
	Digest           string `json:"digest"`
	RepositoryDigest string `json:"repositoryDigest,omitempty"`
	Incomplete       int    `json:"incompleteMetadata"`
	entries          []catalogueEntry
}

func safeEnvironment() bool {
	return os.Getenv("KOPIA_IGNORE_MALFORMED_MANIFEST_CONTENTS") == "" && os.Getenv("KOPIA_DEBUG_MANIFEST_MANAGER") == ""
}

func catalogueIndex(ctx context.Context, r repo.Repository) ([]*manifest.EntryMetadata, error) {
	if !contextActive(ctx) || r.Refresh(ctx) != nil {
		return nil, errReadHeld
	}
	entries, err := r.FindManifests(ctx, map[string]string{manifest.TypeLabelKey: snapshot.ManifestType})
	if err != nil || !contextActive(ctx) || len(entries) > 16384 {
		return nil, errReadHeld
	}
	seen := map[manifest.ID]bool{}
	entries = slices.Clone(entries)
	total := 0
	for i, e := range entries {
		if e == nil || e.ID == "" || len(e.ID) > 256 || seen[e.ID] || e.Length <= 0 || e.Length > 1<<20 || e.ModTime.IsZero() ||
			e.Labels[manifest.TypeLabelKey] != snapshot.ManifestType || len(e.Labels) > 256 {
			return nil, errReadHeld
		}
		seen[e.ID] = true
		copyEntry := *e
		copyEntry.Labels = maps.Clone(e.Labels)
		entries[i] = &copyEntry
		raw, marshalErr := json.Marshal(copyEntry)
		total += len(raw)
		if marshalErr != nil || total > 16<<20 {
			return nil, errReadHeld
		}
	}
	slices.SortFunc(entries, func(a, b *manifest.EntryMetadata) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return entries, nil
}

type strictSnapshotRepository struct {
	repo.Repository
	expected *manifest.EntryMetadata
	raw      json.RawMessage
}

var snapshotCanonical = map[string][]string{
	"":                        {"id", "source", "description", "startTime", "endTime", "stats", "incomplete", "rootEntry", "tags", "pins", "storageStats"},
	"source":                  {"host", "userName", "path"},
	"rootEntry":               {"name", "type", "mode", "size", "mtime", "uid", "gid", "obj", "summ"},
	"rootEntry.summ":          {"size", "files", "symlinks", "dirs", "maxTime", "incomplete", "numFailed", "numIgnoredErrors", "errors"},
	"rootEntry.summ.errors[]": {"path", "error"},
}

func (r *strictSnapshotRepository) GetManifest(ctx context.Context, id manifest.ID, out any) (*manifest.EntryMetadata, error) {
	if id != r.expected.ID || !contextActive(ctx) {
		return nil, errReadHeld
	}
	var raw json.RawMessage
	m, err := r.Repository.GetManifest(ctx, id, &raw)
	if err != nil || m == nil || m.ID != r.expected.ID || m.Length != r.expected.Length || len(raw) != m.Length || len(raw) > 1<<20 ||
		!m.ModTime.Equal(r.expected.ModTime) || !maps.Equal(m.Labels, r.expected.Labels) || strictjson.Validate(raw, snapshotCanonical) != nil ||
		json.Unmarshal(raw, out) != nil || !contextActive(ctx) {
		return nil, errReadHeld
	}
	var identity struct {
		ID manifest.ID `json:"id"`
	}
	// Native SaveSnapshot clears this field; LoadSnapshot later replaces it with
	// the authoritative index ID. A conflicting raw ID is not provenance.
	if json.Unmarshal(raw, &identity) != nil || identity.ID != "" && identity.ID != id {
		return nil, errReadHeld
	}
	r.raw = slices.Clone(raw)
	return m, nil
}

func snapshotLabelsMatch(sm *snapshot.Manifest, labels map[string]string) bool {
	base := map[string]string{manifest.TypeLabelKey: snapshot.ManifestType, snapshot.HostnameLabel: sm.Source.Host, snapshot.UsernameLabel: sm.Source.UserName, snapshot.PathLabel: sm.Source.Path}
	for k, v := range base {
		if v == "" || labels[k] != v {
			return false
		}
	}
	for k, v := range sm.Tags {
		if _, reserved := base[k]; reserved {
			return false
		}
		base[k] = v
	}
	return maps.Equal(base, labels)
}

// readCatalogue retains every discovered snapshot and fails on any load or
// changed index. Its receipt is metadata only; it does not verify content bytes,
// recoverability, an atomic repository snapshot, or permission to erase storage.
func readCatalogue(ctx context.Context, r repo.Repository) (Catalogue, error) {
	if !safeEnvironment() || r == nil || !contextActive(ctx) {
		return Catalogue{}, errReadHeld
	}
	before, err := catalogueIndex(ctx, r)
	if err != nil {
		return Catalogue{}, errReadHeld
	}
	result := Catalogue{MetadataOnly: true}
	total := 0
	for _, entry := range before {
		strict := &strictSnapshotRepository{Repository: r, expected: entry}
		sm, loadErr := snapshot.LoadSnapshot(ctx, strict, entry.ID)
		if loadErr != nil || sm == nil || sm.ID != entry.ID || !contextActive(ctx) ||
			!snapshotLabelsMatch(sm, entry.Labels) ||
			sm.StartTime == 0 || sm.EndTime == 0 || sm.EndTime.Before(sm.StartTime) {
			return Catalogue{}, errReadHeld
		}
		total += len(strict.raw)
		if total > 32<<20 {
			return Catalogue{}, errReadHeld
		}
		if sm.IncompleteReason != "" || sm.RootEntry == nil || sm.RootObjectID().String() == "" {
			result.Incomplete++
		}
		result.entries = append(result.entries, catalogueEntry{Metadata: *entry, Snapshot: *sm, Raw: strict.raw})
	}
	after, err := catalogueIndex(ctx, r)
	if err != nil || len(before) != len(after) {
		return Catalogue{}, errReadHeld
	}
	for i, a := range after {
		b := before[i]
		if a.ID != b.ID || a.Length != b.Length || !a.ModTime.Equal(b.ModTime) || !maps.Equal(a.Labels, b.Labels) {
			return Catalogue{}, errReadHeld
		}
	}
	encoded, err := json.Marshal(result.entries)
	if err != nil || !contextActive(ctx) {
		return Catalogue{}, errReadHeld
	}
	digest := sha256.Sum256(encoded)
	result.Digest = hex.EncodeToString(digest[:])
	result.Snapshots = len(result.entries)
	return result, nil
}
