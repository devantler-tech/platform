package longhorninventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// StorageRoot is the fixed directory observed for storage-root inventories.
const StorageRoot = "/var/lib/longhorn"

// StorageRootReport summarizes immediate storage-root entries and their digest.
type StorageRootReport struct {
	Directories, Files, OtherEntries                                                                  int
	ReplicaEntryPresent, ReplicaDirectoryPresent                                                      bool
	EngineBinariesPresent, MetadataPresent, LogsPresent, BackingImagesPresent, SocketDirectoryPresent bool
	Digest                                                                                            string
}

// ListStorageRoot lists immediate children of the fixed storage directory.
func (n Native) ListStorageRoot(ctx context.Context) (Listing, error) {
	return n.listFixedRoot(ctx, StorageRoot)
}

// AnalyzeStorageRoot summarizes and hashes a complete storage-root listing.
func AnalyzeStorageRoot(listing Listing) (StorageRootReport, error) {
	entries, _, err := normalizedEntries(listing)
	if err != nil {
		return StorageRootReport{}, err
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	report := StorageRootReport{}
	for _, entry := range entries {
		if entry.Name == "." {
			continue
		}
		if entry.IsDir {
			report.Directories++
		} else {
			report.Files++
		}
		switch entry.Name {
		case "replicas":
			report.ReplicaEntryPresent = true
			report.ReplicaDirectoryPresent = entry.IsDir
		case "engine-binaries":
			report.EngineBinariesPresent = entry.IsDir
		case "metadata":
			report.MetadataPresent = entry.IsDir
		case "logs":
			report.LogsPresent = entry.IsDir
		case "backing-images":
			report.BackingImagesPresent = entry.IsDir
		case "unix-domain-socket":
			report.SocketDirectoryPresent = entry.IsDir
		default:
			report.OtherEntries++
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return StorageRootReport{}, errNative
	}
	digest := sha256.Sum256(encoded)
	report.Digest = hex.EncodeToString(digest[:])
	return report, nil
}

type nativeFailure struct{ code string }

func (e nativeFailure) Error() string {
	return "native file observation held [reason=" + FailureCode(e) + "]"
}
func (nativeFailure) Unwrap() error { return errNative }

// FailureCode returns a package-authored diagnostic classification.
//
// Only package-authored classifications enter logs. Transport messages and
// file-error strings never select a diagnostic class or grant missing-path proof.
func FailureCode(err error) string {
	var failure nativeFailure
	if !errors.As(err, &failure) {
		return "unknown"
	}
	switch failure.code {
	case "request", "transport", "not-found", "permission", "unimplemented", "stream", "bounds", "metadata", "entry-error", "entry-shape", "structure", "block-client", "block-list", "block-shape", "block-change":
		return failure.code
	}
	return "unknown"
}
