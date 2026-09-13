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
//   - every object present when the run started is unchanged when it ends; and
//   - every one of those objects exists in the destination with a matching size
//     and content checksum.
//
// Objects archived during the run are expected and left to the next pass.
//
// Usage:
//
//	mirror-wedding-backup-catalogue validate-plan <source-bucket>
//	mirror-wedding-backup-catalogue evaluate <source-before> <source-after> <destination>
//
// Each listing is the `mc ls --json --recursive` output for the catalogue
// prefix, optionally carrying a "sha256" field per object. The result is a
// single non-secret JSON line: counts, the newest base backup, and the newest
// WAL segment on each side.
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
)

const (
	sourcePrefix      = "cnpg/wedding-db"
	sourceSecret      = "wedding-db-backup-r2"
	destinationBucket = "wedding-db-backups"
	destinationPrefix = "cnpg/wedding-db"
	destinationSecret = "wedding-db-backup-r2-dedicated"
)

var (
	ErrCredentialReuse  = errors.New("credential reuse")
	ErrWrongDestination = errors.New("wrong destination")
	ErrWrongSource      = errors.New("wrong source")
	ErrPartialCopy      = errors.New("partial copy")
	ErrChecksumMismatch = errors.New("checksum mismatch")
	ErrUnverifiable     = errors.New("unverifiable object")
	ErrSourceChanged    = errors.New("source changed during the run")
	ErrEmptySource      = errors.New("empty source")
	ErrNoBaseBackup     = errors.New("no base backup")
	ErrMalformedListing = errors.New("malformed listing")
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
	Key    string
	Size   int64
	ETag   string
	SHA256 string
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
)

// EvaluateParity proves that every object present when the run started is
// unchanged at the end and was copied intact.
func EvaluateParity(before, after, destination []Object) (Summary, error) {
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
			current.SHA256 != source.SHA256 {
			return Summary{}, fmt.Errorf("%w: %s", ErrSourceChanged, key)
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
	return Summary{
		SourceObjects:               len(beforeIndex),
		MatchedObjects:              matched,
		ExtraDestinationObjects:     len(destinationIndex) - matched,
		SourceNewestBaseBackup:      sourceBackup,
		DestinationNewestBaseBackup: newestBaseBackup(destinationIndex),
		SourceNewestWAL:             newestWAL(beforeIndex),
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

func isMultipart(etag string) bool {
	return strings.Contains(etag, "-")
}

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

type listingEntry struct {
	Status string `json:"status"`
	Type   string `json:"type"`
	Size   *int64 `json:"size"`
	Key    string `json:"key"`
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"`
}

// ParseListing reads `mc ls --json --recursive` output. Any entry that is not a
// clean success is refused, because a silently skipped object would make a
// partial listing look like a complete one.
func ParseListing(r io.Reader) ([]Object, error) {
	var objects []Object
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry listingEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformedListing, err)
		}
		if entry.Status != "success" {
			return nil, fmt.Errorf("%w: status %q", ErrMalformedListing, entry.Status)
		}
		if entry.Type == "folder" {
			continue
		}
		if entry.Type != "file" || entry.Size == nil || *entry.Size < 0 || entry.ETag == "" ||
			!cleanKey(entry.Key) {
			return nil, fmt.Errorf("%w: entry %q", ErrMalformedListing, entry.Key)
		}
		objects = append(objects, Object{Key: entry.Key, Size: *entry.Size, ETag: entry.ETag, SHA256: entry.SHA256})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedListing, err)
	}
	return objects, nil
}

func cleanKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && path.Clean(key) == key
}

func readListing(name string) ([]Object, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ParseListing(file)
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 2 && args[0] == "validate-plan" {
		return ValidatePlan(Plan{
			Source:      Location{Bucket: args[1], Prefix: sourcePrefix, Secret: sourceSecret},
			Destination: Location{Bucket: destinationBucket, Prefix: destinationPrefix, Secret: destinationSecret},
		})
	}
	if len(args) == 4 && args[0] == "evaluate" {
		listings := make([][]Object, 0, 3)
		for _, name := range args[1:] {
			objects, err := readListing(name)
			if err != nil {
				return err
			}
			listings = append(listings, objects)
		}
		summary, err := EvaluateParity(listings[0], listings[1], listings[2])
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(summary)
	}
	return errors.New("usage: mirror-wedding-backup-catalogue validate-plan <source-bucket> | evaluate <source-before> <source-after> <destination>")
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mirror-wedding-backup-catalogue:", err)
		os.Exit(1)
	}
}
