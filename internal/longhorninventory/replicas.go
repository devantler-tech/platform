package longhorninventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxEntries       = 16384
	MaxMetadataBytes = 4 << 20
)

type Entry struct {
	Name           string
	Mode           uint32
	UID, GID       uint32
	IsDir          bool
	Size, Modified int64
}

type Listing struct {
	Entries  []Entry
	Complete bool
}

type Replica struct {
	Name, UID, Directory string
}

type Report struct {
	Directories, Files, Matched, Unmatched, Missing int
	EmptyAndAccounted                               bool
	Digest                                          string
	UnmatchedNames                                  []string `json:"-"`
}

// Analyze reconciles a complete, immediate-child listing of the fixed V1
// replicas directory with the API's replica identities. It does not establish
// backup coverage, historical recoverability or authorization to erase data.
func Analyze(listing Listing, replicas []Replica) (Report, error) {
	invalid := errors.New("native replica inventory is incomplete or ambiguous")
	if !listing.Complete || len(listing.Entries) == 0 || len(listing.Entries) > MaxEntries || len(replicas) > MaxEntries {
		return Report{}, invalid
	}
	entries := slices.Clone(listing.Entries)
	refs := slices.Clone(replicas)
	seen := make(map[string]bool, len(entries))
	metadataBytes := 0
	for _, entry := range entries {
		metadataBytes += len(entry.Name) + 32
		mode := os.FileMode(entry.Mode)
		if (!baseName(entry.Name) && entry.Name != ".") || seen[entry.Name] ||
			entry.Size < 0 || entry.Modified < 0 || mode.IsDir() != entry.IsDir ||
			(mode.Type() != 0 && mode.Type() != os.ModeDir) || metadataBytes > MaxMetadataBytes {
			return Report{}, invalid
		}
		if entry.Name == "." && !entry.IsDir {
			return Report{}, invalid
		}
		seen[entry.Name] = true
	}
	if !seen["."] {
		return Report{}, invalid
	}
	owners := make(map[string]bool, len(refs))
	names := make(map[string]bool, len(refs))
	uids := make(map[string]bool, len(refs))
	for _, replica := range refs {
		metadataBytes += len(replica.Name) + len(replica.UID) + len(replica.Directory)
		if !baseName(replica.Name) || !baseName(replica.UID) || !baseName(replica.Directory) ||
			owners[replica.Directory] || names[replica.Name] || uids[replica.UID] || metadataBytes > MaxMetadataBytes {
			return Report{}, invalid
		}
		owners[replica.Directory], names[replica.Name], uids[replica.UID] = true, true, true
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(refs, func(a, b Replica) int { return strings.Compare(a.Name, b.Name) })
	report := Report{Missing: len(refs)}
	for _, entry := range entries {
		if entry.Name == "." {
			continue
		}
		if !entry.IsDir {
			report.Files++
			continue
		}
		report.Directories++
		if owners[entry.Name] {
			report.Matched++
			report.Missing--
		} else {
			report.Unmatched++
			report.UnmatchedNames = append(report.UnmatchedNames, entry.Name)
		}
	}
	report.EmptyAndAccounted = len(entries) == 1 && len(refs) == 0
	encoded, err := json.Marshal(struct {
		Version  int
		Entries  []Entry
		Replicas []Replica
	}{1, entries, refs})
	if err != nil {
		return Report{}, errors.New("encode native replica inventory")
	}
	digest := sha256.Sum256(encoded)
	report.Digest = hex.EncodeToString(digest[:])
	return report, nil
}

func baseName(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "/\\") && !strings.ContainsFunc(value, unicode.IsControl)
}
