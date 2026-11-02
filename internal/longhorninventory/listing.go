package longhorninventory

import (
	"errors"
	"os"
	"slices"
)

// Qualify complete, bounded immediate-child metadata before either observer
// assigns meaning to entry names. Neither observer follows links or reads data.
func normalizedEntries(listing Listing) ([]Entry, int, error) {
	invalid := errors.New("native metadata listing is incomplete or ambiguous")
	if !listing.Complete || len(listing.Entries) == 0 || len(listing.Entries) > MaxEntries {
		return nil, 0, invalid
	}
	entries := slices.Clone(listing.Entries)
	seen := make(map[string]bool, len(entries))
	metadataBytes := 0
	for _, entry := range entries {
		metadataBytes += len(entry.Name) + 32
		mode := os.FileMode(entry.Mode)
		if (!baseName(entry.Name) && entry.Name != ".") || seen[entry.Name] || entry.Size < 0 || entry.Modified < 0 || mode.IsDir() != entry.IsDir ||
			(mode.Type() != 0 && mode.Type() != os.ModeDir) || metadataBytes > MaxMetadataBytes {
			return nil, 0, invalid
		}
		if entry.Name == "." && !entry.IsDir {
			return nil, 0, invalid
		}
		seen[entry.Name] = true
	}
	if !seen["."] {
		return nil, 0, invalid
	}
	return entries, metadataBytes, nil
}
