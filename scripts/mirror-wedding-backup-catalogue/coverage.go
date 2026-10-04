package main

// The coverage evaluation decides whether the dedicated Wedding bucket holds
// everything the stale shared copy of the catalogue holds (#4481). It only
// reads: it is the evidence a person needs before deciding to delete the shared
// copy, and it deletes nothing itself.
//
// Usage:
//
//	mirror-wedding-backup-catalogue coverage-multipart-keys <shared-bucket> <shared> <dedicated>
//	mirror-wedding-backup-catalogue evaluate-coverage <shared-bucket> <shared> <dedicated> <shared-sums> <dedicated-sums>
//
// The two listings come from two pods, because after #3253 no namespace holds
// both credentials. coverage-multipart-keys prints the shared keys whose content
// an ETag cannot prove on at least one side; the caller has each pod hash those
// keys and passes the results as "<key>\t<sha256>" lines.
//
// The dedicated store applies its retention policy and the shared copy no longer
// does, so the shared copy can hold objects the dedicated store has since
// pruned. Such an object is accepted only when it is older than everything the
// dedicated store still keeps of the same kind in the same server directory.
// Every other shared object must be present with matching content.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

var (
	ErrNotCovered    = errors.New("shared object is not covered by the dedicated catalogue")
	ErrMalformedSums = errors.New("malformed checksum list")
	ErrStaleListing  = errors.New("dedicated listing does not follow the shared listing")
)

// CoverageSummary is the non-secret evidence a passing coverage evaluation reports.
type CoverageSummary struct {
	SharedObjects             int    `json:"sharedObjects"`
	MatchedObjects            int    `json:"matchedObjects"`
	RetentionPrunedObjects    int    `json:"retentionPrunedObjects"`
	Covered                   bool   `json:"covered"`
	SharedNewestBaseBackup    string `json:"sharedNewestBaseBackup"`
	DedicatedNewestBaseBackup string `json:"dedicatedNewestBaseBackup"`
}

// MultipartKeys returns, sorted, every shared key whose content cannot be
// compared by ETag because it is multipart on either side. A key the dedicated
// catalogue does not hold is left out: there is nothing to compare it with.
func MultipartKeys(shared, dedicated []Object) ([]string, error) {
	dedicatedIndex, err := index(dedicated)
	if err != nil {
		return nil, err
	}
	if _, err := index(shared); err != nil {
		return nil, err
	}
	var keys []string
	for _, object := range shared {
		copied, ok := dedicatedIndex[object.Key]
		if ok && (isMultipart(object.ETag) || isMultipart(copied.ETag)) {
			keys = append(keys, object.Key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// ApplySums sets the SHA256 of every object named in a "<key>\t<sha256>" list.
// A key the listing does not hold, a repeated key or a malformed digest is
// refused, so a list produced for another listing cannot vouch for this one.
func ApplySums(objects []Object, r io.Reader) error {
	positions := make(map[string]int, len(objects))
	for i, object := range objects {
		positions[object.Key] = i
	}
	seen := map[string]bool{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		key, sum, ok := strings.Cut(line, "\t")
		if !ok || !sha256Hex.MatchString(sum) {
			return fmt.Errorf("%w: line %q", ErrMalformedSums, line)
		}
		position, listed := positions[key]
		if !listed || seen[key] {
			return fmt.Errorf("%w: unexpected key %s", ErrMalformedSums, key)
		}
		seen[key] = true
		objects[position].SHA256 = sum
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedSums, err)
	}
	return nil
}

// retentionFloor records, per server directory, the oldest base backup and the
// oldest WAL segment the dedicated store still keeps. Retention only ever
// removes from the old end, so anything older than these is gone by design.
type retentionFloor struct {
	backup map[string]string
	wal    map[string]string
}

func retentionFloorOf(objects map[string]Object) retentionFloor {
	floor := retentionFloor{backup: map[string]string{}, wal: map[string]string{}}
	for key := range objects {
		parts := strings.Split(key, "/")
		server := parts[0]
		if id := baseBackupIDOf(key); id != "" {
			if current, ok := floor.backup[server]; !ok || id < current {
				floor.backup[server] = id
			}
		}
		if segment := walSegmentOf(key); segment != "" {
			if current, ok := floor.wal[server]; !ok || segment < current {
				floor.wal[server] = segment
			}
		}
	}
	return floor
}

// baseBackupIDOf returns the backup ID of a key inside a base backup directory.
func baseBackupIDOf(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || parts[1] != "base" || !backupID.MatchString(parts[2]) {
		return ""
	}
	return parts[2]
}

// walPositionOf returns the segment a WAL-directory object belongs to: the
// segment itself, or the one a backup label or partial file is named after. A
// timeline history file has no position and is never pruned by retention.
func walPositionOf(key string) string {
	if segment := walSegmentOf(key); segment != "" {
		return segment
	}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[1] != "wals" || len(parts[3]) < 25 || parts[3][24] != '.' {
		return ""
	}
	segment := parts[3][:24]
	if !walSegment.MatchString(segment) || parts[2] != segment[:16] {
		return ""
	}
	return segment
}

// prunedByRetention reports whether a shared object the dedicated store lacks is
// older than everything that store still keeps of its kind in its server
// directory. A server directory the dedicated store holds nothing of has no
// floor, so nothing in it is accepted as pruned.
func (floor retentionFloor) prunedByRetention(key string) bool {
	server := strings.Split(key, "/")[0]
	if id := baseBackupIDOf(key); id != "" {
		oldest, ok := floor.backup[server]
		return ok && id < oldest
	}
	if position := walPositionOf(key); position != "" {
		oldest, ok := floor.wal[server]
		return ok && position < oldest
	}
	return false
}

// EvaluateCoverage proves the dedicated catalogue covers the shared one. Every
// shared object must be in the dedicated catalogue with matching content, or be
// older than what the dedicated store's retention still keeps. The dedicated
// catalogue's newest complete base backup must be at least as new as the shared
// one's, so the shared copy never holds the most recent restore point.
func EvaluateCoverage(shared, dedicated Listing) (CoverageSummary, error) {
	if shared.Started.IsZero() || dedicated.Started.IsZero() ||
		dedicated.Started.Before(shared.Started) {
		return CoverageSummary{}, ErrStaleListing
	}
	sharedIndex, err := index(shared.Objects)
	if err != nil {
		return CoverageSummary{}, err
	}
	dedicatedIndex, err := index(dedicated.Objects)
	if err != nil {
		return CoverageSummary{}, err
	}
	if len(sharedIndex) == 0 {
		return CoverageSummary{}, ErrEmptySource
	}

	floor := retentionFloorOf(dedicatedIndex)
	matched, pruned := 0, 0
	for key, source := range sharedIndex {
		copied, ok := dedicatedIndex[key]
		if !ok {
			if !floor.prunedByRetention(key) {
				return CoverageSummary{}, fmt.Errorf("%w: %s", ErrNotCovered, key)
			}
			pruned++
			continue
		}
		if copied.Size != source.Size {
			return CoverageSummary{}, fmt.Errorf("%w: %s", ErrPartialCopy, key)
		}
		if err := sameContent(source, copied); err != nil {
			return CoverageSummary{}, fmt.Errorf("%w: %s", err, key)
		}
		matched++
	}

	sharedBackup := newestBaseBackup(sharedIndex)
	dedicatedBackup := newestBaseBackup(dedicatedIndex)
	if dedicatedBackup == "" {
		return CoverageSummary{}, ErrNoBaseBackup
	}
	_, sharedID, _ := strings.Cut(sharedBackup, "/")
	_, dedicatedID, _ := strings.Cut(dedicatedBackup, "/")
	if dedicatedID < sharedID {
		return CoverageSummary{}, fmt.Errorf("%w: the newest dedicated base backup %s is older than the shared %s",
			ErrNotCovered, dedicatedBackup, sharedBackup)
	}

	return CoverageSummary{
		SharedObjects:             len(sharedIndex),
		MatchedObjects:            matched,
		RetentionPrunedObjects:    pruned,
		Covered:                   true,
		SharedNewestBaseBackup:    sharedBackup,
		DedicatedNewestBaseBackup: dedicatedBackup,
	}, nil
}

// readCoverageListings reads the shared and dedicated listings for a coverage evaluation.
func readCoverageListings(sharedBucket, sharedName, dedicatedName string) (Listing, Listing, error) {
	if sharedBucket == "" || sharedBucket == destinationBucket {
		return Listing{}, Listing{}, fmt.Errorf("%w: shared bucket %q", ErrWrongSource, sharedBucket)
	}
	shared, err := readListing(sharedName, sharedBucket+"/"+sourcePrefix)
	if err != nil {
		return Listing{}, Listing{}, fmt.Errorf("%s: %w", sharedName, err)
	}
	dedicated, err := readListing(dedicatedName, destinationBucket+"/"+destinationPrefix)
	if err != nil {
		return Listing{}, Listing{}, fmt.Errorf("%s: %w", dedicatedName, err)
	}
	return shared, dedicated, nil
}

// applySumsFile applies one checksum list named on the command line.
func applySumsFile(objects []Object, name string) error {
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	applyErr := ApplySums(objects, file)
	if closeErr := file.Close(); closeErr != nil && applyErr == nil {
		return closeErr
	}
	if applyErr != nil {
		return fmt.Errorf("%s: %w", name, applyErr)
	}
	return nil
}
