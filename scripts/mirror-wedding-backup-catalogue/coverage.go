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
// pruned. Such an object is accepted only when it was written before the oldest
// complete base backup the dedicated store keeps in the same server directory
// and before the retention window began.
// Every other shared object must be present with matching content.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
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

// retentionWindow is the dedicated ObjectStore's retention policy. The runner
// refuses a store that declares another one.
const retentionWindow = 30 * 24 * time.Hour

// emptyDigest is the sha256 of no bytes. A non-empty object that hashes to it
// was not read.
const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// oldestCompleteBackups returns, per server directory, the start time of the
// oldest base backup that has both its backup.info and a non-empty data archive.
func oldestCompleteBackups(objects map[string]Object) map[string]time.Time {
	hasInfo := map[string]bool{}
	hasData := map[string]bool{}
	for key, object := range objects {
		id := baseBackupIDOf(key)
		parts := strings.Split(key, "/")
		if id == "" || len(parts) != 4 {
			continue
		}
		backup := parts[0] + "/" + id
		switch {
		case parts[3] == "backup.info":
			hasInfo[backup] = true
		case dataArchive.MatchString(parts[3]) && object.Size > 0:
			hasData[backup] = true
		}
	}
	oldest := map[string]time.Time{}
	for backup := range hasInfo {
		if !hasData[backup] {
			continue
		}
		server, id, _ := strings.Cut(backup, "/")
		started, err := time.Parse("20060102T150405", id)
		if err != nil {
			continue
		}
		if current, ok := oldest[server]; !ok || started.Before(current) {
			oldest[server] = started
		}
	}
	return oldest
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

// prunedByRetention reports whether a shared object the dedicated store lacks
// can only be missing because retention removed it. Retention never removes
// anything written at or after the start of the oldest backup it keeps, and
// never removes anything still inside the retention window, so the object must
// have been written before both. It is judged by when it was written, not by its
// name: segment names do not order across timelines, and an object that was
// never copied has a name just like a pruned one. A server directory with no
// complete dedicated backup has nothing to measure against, so nothing in it is
// accepted.
func prunedByRetention(object Object, oldest map[string]time.Time, listed time.Time) bool {
	if baseBackupIDOf(object.Key) == "" && walPositionOf(object.Key) == "" {
		return false
	}
	started, ok := oldest[strings.Split(object.Key, "/")[0]]
	return ok && object.LastModified.Before(started) &&
		object.LastModified.Before(listed.Add(-retentionWindow))
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

	oldest := oldestCompleteBackups(dedicatedIndex)
	matched, pruned := 0, 0
	for key, source := range sharedIndex {
		copied, ok := dedicatedIndex[key]
		if !ok {
			if !prunedByRetention(source, oldest, dedicated.Started) {
				return CoverageSummary{}, fmt.Errorf("%w: %s", ErrNotCovered, key)
			}
			pruned++
			continue
		}
		if copied.Size != source.Size {
			return CoverageSummary{}, fmt.Errorf("%w: %s", ErrPartialCopy, key)
		}
		if source.Size > 0 && (source.SHA256 == emptyDigest || copied.SHA256 == emptyDigest) {
			return CoverageSummary{}, fmt.Errorf("%w: %s was hashed as empty", ErrUnverifiable, key)
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
