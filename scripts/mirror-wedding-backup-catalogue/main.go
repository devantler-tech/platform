// Command mirror-wedding-backup-catalogue decides whether a mirror of the
// shared Wedding backup catalogue into its dedicated bucket can be trusted.
//
// The dedicated bucket must hold the full recoverable history before the
// wedding-db Cluster switches its archive reference, or the new archive starts
// with no base backup to restore from. Copying needs two identities, because
// each R2 credential is scoped to its own bucket, and the shared catalogue keeps
// receiving WAL until the switch. Every part of that is easy to get silently
// wrong, so the mirror is only accepted when:
//
//   - the plan reads with the shared credential and writes with the dedicated
//     one, to exactly the reviewed destination;
//   - every listing is proven complete, the listing taken when the run started
//     misses nothing that already existed, and every object in it is unchanged
//     when the run ends; and
//   - every one of those objects exists in the destination with a matching size
//     and content checksum.
//
// Objects archived after the run started are expected and left to the next
// pass. An object that is missing from the starting listing but was written
// before the run started proves that listing was incomplete, so it is refused.
//
// Usage:
//
//	mirror-wedding-backup-catalogue validate-plan <source-bucket> <source-prefix> <source-secret> <destination-bucket> <destination-prefix> <destination-secret>
//	mirror-wedding-backup-catalogue evaluate <run-start> <source-bucket> <source-before> <source-after> <destination>
//
// validate-plan takes the exact values the mirror job will use, so a typo in
// any of them is refused rather than replaced by the reviewed value.
//
// <run-start> is an RFC 3339 timestamp taken before the starting listing. Each
// listing is the `mc ls --json --recursive` output for the catalogue prefix,
// with keys starting at the server directory, optionally carrying a "sha256"
// field (64 lowercase hex characters) per object.
//
// Raw `mc ls` output has no terminal record and its keys carry no bucket, so a
// listing cut short, or taken from the wrong bucket, is indistinguishable from
// the right one. The caller must therefore append one completion record as the
// last line, only after `mc ls` exits successfully, naming the bucket and
// prefix it listed:
//
//	{"status":"success","type":"listing-complete","location":"<bucket>/<prefix>","files":<number of file entries>}
//
// A listing without that record, whose count does not match, or whose location
// is not the one being evaluated, is refused.
//
// The result is a single non-secret JSON line: counts, the newest base backup,
// and the newest WAL segment on each side.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// These name the reviewed locations. The Secret values are Kubernetes Secret
// names, not credentials.
const (
	sourcePrefix      = "cnpg/wedding-db"
	sourceSecret      = "wedding-db-backup-r2"
	destinationBucket = "wedding-db-backups"
	destinationPrefix = "cnpg/wedding-db"
	destinationSecret = "wedding-db-backup-r2-dedicated"
)

var (
	ErrCredentialReuse   = errors.New("credential reuse")
	ErrWrongDestination  = errors.New("wrong destination")
	ErrWrongSource       = errors.New("wrong source")
	ErrPartialCopy       = errors.New("partial copy")
	ErrChecksumMismatch  = errors.New("checksum mismatch")
	ErrUnverifiable      = errors.New("unverifiable object")
	ErrSourceChanged     = errors.New("source changed during the run")
	ErrIncompleteListing = errors.New("incomplete listing")
	ErrEmptySource       = errors.New("empty source")
	ErrNoBaseBackup      = errors.New("no base backup")
	ErrNoArchivedWAL     = errors.New("no archived WAL")
	ErrMalformedListing  = errors.New("malformed listing")
	ErrListingLocation   = errors.New("listing from an unexpected location")
)

// Location is one side of the mirror: a bucket, the catalogue prefix inside it,
// and the name of the namespace Secret holding the credential for that bucket.
type Location struct {
	Bucket string
	Prefix string
	Secret string
}

// Plan is the pair of locations a mirror run reads from and writes to.
type Plan struct {
	Source      Location
	Destination Location
}

// Object is one listed catalogue object, keyed relative to the catalogue prefix.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	SHA256       string
	LastModified time.Time
}

// Summary is the non-secret evidence a successful evaluation reports.
type Summary struct {
	SourceObjects               int    `json:"sourceObjects"`
	MatchedObjects              int    `json:"matchedObjects"`
	ExtraDestinationObjects     int    `json:"extraDestinationObjects"`
	SourceNewestBaseBackup      string `json:"sourceNewestBaseBackup"`
	DestinationNewestBaseBackup string `json:"destinationNewestBaseBackup"`
	SourceNewestWAL             string `json:"sourceNewestWal"`
	DestinationNewestWAL        string `json:"destinationNewestWal"`
}

// ValidatePlan refuses any plan other than the reviewed one. The source bucket
// is the only free value, because the shared bucket name is a cluster variable.
func ValidatePlan(plan Plan) error {
	if plan.Source.Secret == plan.Destination.Secret ||
		plan.Source.Secret == destinationSecret ||
		plan.Destination.Secret == sourceSecret {
		return ErrCredentialReuse
	}
	if plan.Destination.Bucket != destinationBucket ||
		plan.Destination.Prefix != destinationPrefix ||
		plan.Destination.Secret != destinationSecret ||
		plan.Source.Bucket == plan.Destination.Bucket {
		return ErrWrongDestination
	}
	if plan.Source.Bucket == "" ||
		plan.Source.Prefix != sourcePrefix ||
		plan.Source.Secret != sourceSecret {
		return ErrWrongSource
	}
	return nil
}

var (
	walSegment = regexp.MustCompile(`^[0-9A-F]{24}$`)
	backupID   = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// EvaluateParity proves that the starting listing was complete, that every
// object in it is unchanged at the end, and that each was copied intact.
func EvaluateParity(runStart time.Time, before, after, destination []Object) (Summary, error) {
	if runStart.IsZero() {
		return Summary{}, fmt.Errorf("%w: no run start", ErrMalformedListing)
	}
	beforeIndex, err := index(before)
	if err != nil {
		return Summary{}, err
	}
	afterIndex, err := index(after)
	if err != nil {
		return Summary{}, err
	}
	destinationIndex, err := index(destination)
	if err != nil {
		return Summary{}, err
	}
	if len(beforeIndex) == 0 {
		return Summary{}, ErrEmptySource
	}

	for key, source := range beforeIndex {
		current, ok := afterIndex[key]
		if !ok || current.Size != source.Size || current.ETag != source.ETag ||
			(current.SHA256 != "" && source.SHA256 != "" && current.SHA256 != source.SHA256) {
			return Summary{}, fmt.Errorf("%w: %s", ErrSourceChanged, key)
		}
	}
	for key, current := range afterIndex {
		if _, ok := beforeIndex[key]; !ok && !current.LastModified.After(runStart) {
			return Summary{}, fmt.Errorf("%w: %s predates the run", ErrIncompleteListing, key)
		}
	}

	matched := 0
	for key, source := range beforeIndex {
		copied, ok := destinationIndex[key]
		if !ok || copied.Size != source.Size {
			return Summary{}, fmt.Errorf("%w: %s", ErrPartialCopy, key)
		}
		if err := sameContent(source, copied); err != nil {
			return Summary{}, fmt.Errorf("%w: %s", err, key)
		}
		matched++
	}

	sourceBackup := newestBaseBackup(beforeIndex)
	if sourceBackup == "" {
		return Summary{}, ErrNoBaseBackup
	}
	// A base backup only restores to a consistent state by replaying the WAL
	// archived with it, so a catalogue without any is not recoverable history.
	sourceWAL := newestWAL(beforeIndex)
	if sourceWAL == "" {
		return Summary{}, ErrNoArchivedWAL
	}
	return Summary{
		SourceObjects:               len(beforeIndex),
		MatchedObjects:              matched,
		ExtraDestinationObjects:     len(destinationIndex) - matched,
		SourceNewestBaseBackup:      sourceBackup,
		DestinationNewestBaseBackup: newestBaseBackup(destinationIndex),
		SourceNewestWAL:             sourceWAL,
		DestinationNewestWAL:        newestWAL(destinationIndex),
	}, nil
}

// sameContent compares a content digest when both sides carry one. Otherwise
// it falls back to the ETag, which is a content MD5 only for a single-part
// upload: a multipart ETag depends on how the object was split, so identical
// bytes can differ and different bytes cannot be told apart. That case is
// refused rather than accepted on size alone.
func sameContent(source, copied Object) error {
	if source.SHA256 != "" && copied.SHA256 != "" {
		if source.SHA256 != copied.SHA256 {
			return ErrChecksumMismatch
		}
		return nil
	}
	if isMultipart(source.ETag) || isMultipart(copied.ETag) {
		return ErrUnverifiable
	}
	if source.ETag != copied.ETag {
		return ErrChecksumMismatch
	}
	return nil
}

// isMultipart reports whether an ETag has the "<hash>-<parts>" multipart form.
func isMultipart(etag string) bool {
	return strings.Contains(etag, "-")
}

// index keys objects by name and refuses a listing that names an object twice.
func index(objects []Object) (map[string]Object, error) {
	result := make(map[string]Object, len(objects))
	for _, object := range objects {
		if _, seen := result[object.Key]; seen {
			return nil, fmt.Errorf("%w: duplicate key %s", ErrMalformedListing, object.Key)
		}
		result[object.Key] = object
	}
	return result, nil
}

// newestBaseBackup returns "<server>/<backup ID>" for the latest base backup,
// identified by its backup.info file in the Barman Cloud layout.
func newestBaseBackup(objects map[string]Object) string {
	newest, newestID := "", ""
	for key := range objects {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[1] != "base" || parts[3] != "backup.info" ||
			!backupID.MatchString(parts[2]) {
			continue
		}
		if parts[2] > newestID || (parts[2] == newestID && parts[0]+"/"+parts[2] > newest) {
			newest, newestID = parts[0]+"/"+parts[2], parts[2]
		}
	}
	return newest
}

// newestWAL returns the highest WAL segment name. Segment names order by
// timeline and then position, so the lexical maximum is the newest segment.
func newestWAL(objects map[string]Object) string {
	newest := ""
	for key := range objects {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[1] != "wals" {
			continue
		}
		segment := strings.TrimSuffix(parts[3], ".gz")
		if walSegment.MatchString(segment) && segment > newest {
			newest = segment
		}
	}
	return newest
}

// listingEntry is one line of a listing: an mc object or folder record, or the
// caller's completion record.
type listingEntry struct {
	Status       string `json:"status"`
	Type         string `json:"type"`
	Size         *int64 `json:"size"`
	Key          string `json:"key"`
	ETag         string `json:"etag"`
	SHA256       string `json:"sha256"`
	LastModified string `json:"lastModified"`
	Files        *int   `json:"files"`
	Location     string `json:"location"`
}

// ParseListing reads `mc ls --json --recursive` output ending in a completion
// record. Any entry that is not a clean success is refused, because a silently
// skipped object would make a partial listing look like a complete one. Keys
// must start at the Barman server directory, so a listing rooted one level too
// high or too low is reported as malformed rather than as a catalogue with no
// backups.
func ParseListing(r io.Reader, location string) ([]Object, error) {
	var objects []Object
	complete := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if complete {
			return nil, fmt.Errorf("%w: record after the completion record", ErrMalformedListing)
		}
		var entry listingEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMalformedListing, err)
		}
		if entry.Status != "success" {
			return nil, fmt.Errorf("%w: status %q", ErrMalformedListing, entry.Status)
		}
		switch entry.Type {
		case "folder":
			continue
		case "listing-complete":
			if entry.Files == nil {
				return nil, fmt.Errorf("%w: completion record without a file count", ErrIncompleteListing)
			}
			if location == "" || entry.Location != location {
				return nil, fmt.Errorf("%w: listed %q, want %q", ErrListingLocation, entry.Location, location)
			}
			if *entry.Files < 0 {
				return nil, fmt.Errorf("%w: negative completion count", ErrMalformedListing)
			}
			if *entry.Files != len(objects) {
				return nil, fmt.Errorf("%w: completion record counts %d files, listing has %d",
					ErrIncompleteListing, *entry.Files, len(objects))
			}
			complete = true
			continue
		}
		modified, err := time.Parse(time.RFC3339Nano, entry.LastModified)
		if entry.Type != "file" || entry.Size == nil || *entry.Size < 0 || entry.ETag == "" ||
			err != nil || !catalogueKey(entry.Key) ||
			(entry.SHA256 != "" && !sha256Hex.MatchString(entry.SHA256)) {
			return nil, fmt.Errorf("%w: entry %q", ErrMalformedListing, entry.Key)
		}
		objects = append(objects, Object{
			Key:          entry.Key,
			Size:         *entry.Size,
			ETag:         entry.ETag,
			SHA256:       entry.SHA256,
			LastModified: modified,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedListing, err)
	}
	if !complete {
		return nil, fmt.Errorf("%w: no completion record", ErrIncompleteListing)
	}
	return objects, nil
}

// catalogueKey accepts a clean relative key of the form
// <server>/base/... or <server>/wals/..., the only trees Barman Cloud writes.
func catalogueKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || path.Clean(key) != key {
		return false
	}
	parts := strings.Split(key, "/")
	return len(parts) >= 3 && parts[0] != "" && (parts[1] == "base" || parts[1] == "wals")
}

// readListing parses one listing file named on the command line.
func readListing(name, location string) ([]Object, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	objects, parseErr := ParseListing(file, location)
	if closeErr := file.Close(); closeErr != nil && parseErr == nil {
		return nil, closeErr
	}
	return objects, parseErr
}

// run dispatches the command line and writes the summary for a passing evaluation.
func run(args []string, stdout io.Writer) error {
	if len(args) == 7 && args[0] == "validate-plan" {
		return ValidatePlan(Plan{
			Source:      Location{Bucket: args[1], Prefix: args[2], Secret: args[3]},
			Destination: Location{Bucket: args[4], Prefix: args[5], Secret: args[6]},
		})
	}
	if len(args) == 6 && args[0] == "evaluate" {
		runStart, err := time.Parse(time.RFC3339Nano, args[1])
		if err != nil {
			return fmt.Errorf("%w: run start: %w", ErrMalformedListing, err)
		}
		if args[2] == "" || args[2] == destinationBucket {
			return fmt.Errorf("%w: source bucket %q", ErrWrongSource, args[2])
		}
		locations := []string{
			args[2] + "/" + sourcePrefix,
			args[2] + "/" + sourcePrefix,
			destinationBucket + "/" + destinationPrefix,
		}
		listings := make([][]Object, 0, 3)
		for i, name := range args[3:] {
			objects, err := readListing(name, locations[i])
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			listings = append(listings, objects)
		}
		summary, err := EvaluateParity(runStart, listings[0], listings[1], listings[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(summary)
	}
	return errors.New("usage: mirror-wedding-backup-catalogue validate-plan <source-bucket> <source-prefix> <source-secret> <destination-bucket> <destination-prefix> <destination-secret> | evaluate <run-start> <source-bucket> <source-before> <source-after> <destination>")
}

// main exits non-zero with the refusal reason when a plan or mirror is not trusted.
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mirror-wedding-backup-catalogue:", err)
		os.Exit(1)
	}
}
