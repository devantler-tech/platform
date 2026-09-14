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
//   - every listing is proven complete, the run start is the one recorded by
//     this pass's starting listing, that listing misses nothing that already
//     existed, and every object in it is unchanged when the run ends;
//   - every one of those objects exists in the destination with a matching size
//     and content checksum, and the newest base backup carries its data archive
//     rather than only its backup.info; and
//   - every destination object that was not in the starting listing is an
//     object archived during the run, copied with a matching size and checksum.
//
// Objects archived after the run started are expected. Those already copied are
// verified; the rest are reported as pending, and the result says the mirror has
// not converged, so it cannot serve as cutover proof until a later pass copies
// them. An object that is missing from the starting listing but was written
// before the run started proves that listing was incomplete, so it is refused.
//
// Usage:
//
//	mirror-wedding-backup-catalogue validate-plan <source-bucket> <source-prefix> <source-secret> <destination-bucket> <destination-prefix> <destination-secret>
//	mirror-wedding-backup-catalogue evaluate <run-start> <source-bucket> <source-before> <source-after> <destination>
//	mirror-wedding-backup-catalogue evaluate-catch-up <switch-time> <server-name> <source-bucket> <source-before> <source-after> <destination>
//	mirror-wedding-backup-catalogue validate-switch-time <switch-time>
//
// validate-plan takes the exact values the mirror job will use, so a typo in
// any of them is refused rather than replaced by the reviewed value.
//
// evaluate-catch-up runs after the Cluster switched its archive reference. The
// shared catalogue no longer changes, so it must be copied in full, and every
// other destination object must postdate <switch-time> and sit under the Cluster's
// <server-name> directory. Within that directory, the first WAL segment in the
// dedicated store newer than the shared catalogue's newest one must be its
// successor on the same timeline. Its starting listing must start after
// <switch-time>. validate-switch-time refuses a switch time that does not parse,
// so the wrapper can reject it before touching the cluster.
//
// <run-start> is the RFC 3339 timestamp the wrapper took immediately before
// starting the source listing. Each listing is the `mc ls --json --recursive`
// output for the catalogue prefix, with keys starting at the server directory,
// optionally carrying a "sha256" field (64 lowercase hex characters) per object.
//
// Raw `mc ls` output has no terminal record and its keys carry no bucket, so a
// listing cut short, or taken from the wrong bucket, is indistinguishable from
// the right one. The caller must therefore append one completion record as the
// last line, only after `mc ls` exits successfully, naming the bucket and
// prefix it listed and the time it started that listing:
//
//	{"status":"success","type":"listing-complete","location":"<bucket>/<prefix>","started":"<RFC 3339>","files":<number of file entries>}
//
// A listing without that record, whose count does not match, or whose location
// is not the one being evaluated, is refused. <run-start> must equal the
// starting listing's "started", and the ending listing must not have started
// before it, so a start time left over from an earlier pass is refused rather
// than letting objects written since then pass as archived during this run.
// The destination listing must carry a "started" no earlier than the ending
// listing's, so a destination listing left over from an earlier pass cannot
// prove a copy the destination may since have lost.
//
// The result is a single non-secret JSON line: counts, the objects still
// pending, whether the mirror has converged, the newest base backup, and the
// newest WAL segment on each side. Only a converged result proves the
// destination is ready for the cutover.
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
	ErrRunStartMismatch  = errors.New("run start does not belong to this pass")
	ErrSwitchTime        = errors.New("switch time does not fit the listings")
	ErrWALGap            = errors.New("WAL gap across the switch")

	ErrUnexpectedDestinationObject = errors.New("unexpected destination object")
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

// Listing is a parsed listing and the time its wrapper started it, which is
// zero when the completion record carries no "started" field.
type Listing struct {
	Objects []Object
	Started time.Time
}

// Summary is the non-secret evidence a successful evaluation reports.
// Converged is true only when no object archived during the run is still
// missing from the destination.
type Summary struct {
	SourceObjects               int    `json:"sourceObjects"`
	MatchedObjects              int    `json:"matchedObjects"`
	VerifiedLateObjects         int    `json:"verifiedLateObjects"`
	PendingObjects              int    `json:"pendingObjects"`
	Converged                   bool   `json:"converged"`
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
	walSegment  = regexp.MustCompile(`^[0-9A-F]{24}$`)
	backupID    = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	dataArchive = regexp.MustCompile(`^data\.tar(\.[a-z0-9]+)?$`)
)

// BindRunStart refuses a run start that is not the one the starting listing
// recorded, an ending listing that started before it, and a destination
// listing that started before the ending listing. Without the first two, a
// start time reused from an earlier pass lets an object written after that
// time but missed by this pass's starting listing pass as archived during the
// run, so it is never checked against the destination. Without the last, a
// destination listing left over from an earlier pass can report objects the
// destination has since lost, so the result converges on evidence that no
// longer holds.
func BindRunStart(runStart time.Time, before, after, destination Listing) error {
	if before.Started.IsZero() || after.Started.IsZero() {
		return fmt.Errorf("%w: a source listing has no start time", ErrRunStartMismatch)
	}
	if !runStart.Equal(before.Started) {
		return fmt.Errorf("%w: run start %s, starting listing started %s",
			ErrRunStartMismatch, runStart.Format(time.RFC3339Nano), before.Started.Format(time.RFC3339Nano))
	}
	if after.Started.Before(before.Started) {
		return fmt.Errorf("%w: ending listing started before the starting listing", ErrRunStartMismatch)
	}
	// The ending listing's start is non-zero here, so this also refuses a
	// destination listing that carries no start time.
	if destination.Started.Before(after.Started) {
		return fmt.Errorf("%w: destination listing has no start time or started before the ending listing", ErrRunStartMismatch)
	}
	return nil
}

// EvaluateParity proves that the starting listing was complete, that every
// object in it is unchanged at the end and was copied intact, and that every
// other destination object is a verified copy of an object archived during the
// run. Objects archived during the run but not yet copied are reported as
// pending rather than refused.
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

	// A destination object absent from the starting listing is only trusted as a
	// copy of an object archived during the run; anything else, such as residue
	// from a failed earlier copy, could change what later backup discovery finds.
	verifiedLate := 0
	for key, copied := range destinationIndex {
		if _, ok := beforeIndex[key]; ok {
			continue
		}
		current, ok := afterIndex[key]
		if !ok {
			return Summary{}, fmt.Errorf("%w: %s", ErrUnexpectedDestinationObject, key)
		}
		if copied.Size != current.Size {
			return Summary{}, fmt.Errorf("%w: %s", ErrPartialCopy, key)
		}
		if err := sameContent(current, copied); err != nil {
			return Summary{}, fmt.Errorf("%w: %s", err, key)
		}
		verifiedLate++
	}
	pending := 0
	for key := range afterIndex {
		_, listed := beforeIndex[key]
		_, copied := destinationIndex[key]
		if !listed && !copied {
			pending++
		}
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
		VerifiedLateObjects:         verifiedLate,
		PendingObjects:              pending,
		Converged:                   pending == 0,
		SourceNewestBaseBackup:      sourceBackup,
		DestinationNewestBaseBackup: newestBaseBackup(destinationIndex),
		SourceNewestWAL:             sourceWAL,
		DestinationNewestWAL:        newestWAL(destinationIndex),
	}, nil
}

// CatchUpSummary is the non-secret evidence a successful catch-up reports.
// Converged is true only when the dedicated store holds a segment archived after
// the switch that continues the shared catalogue's WAL sequence without a gap.
type CatchUpSummary struct {
	ServerName         string `json:"serverName"`
	SourceObjects      int    `json:"sourceObjects"`
	MatchedObjects     int    `json:"matchedObjects"`
	PostSwitchObjects  int    `json:"postSwitchObjects"`
	Converged          bool   `json:"converged"`
	SourceNewestWAL    string `json:"sourceNewestWal"`
	FirstPostSwitchWAL string `json:"firstPostSwitchWal"`
}

// serverNamePattern accepts a Barman server directory name: one path segment.
var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// EvaluateCatchUp proves the pass that runs after the Cluster switched its
// archive reference to the dedicated store. Nothing archives to the shared store
// any more, so both of its listings must match exactly and none of its objects may
// be newer than the recorded switch. Every shared object must then be present in
// the destination with a matching size and content, and every other destination
// object must have been written after the switch, under the Cluster's server
// directory.
//
// Continuity is judged only within that server directory, because the shared
// catalogue can also hold an earlier server's WAL, whose segment names say nothing
// about this Cluster's history. The first segment archived through the dedicated
// store that is newer than the server's newest shared segment must be that
// segment's successor on the same timeline, so point-in-time recovery has no gap
// across the switch. Until such a segment exists there is nothing to prove that
// against, so the result is verified but not converged.
func EvaluateCatchUp(switchTime time.Time, serverName string, before, after, destination []Object) (CatchUpSummary, error) {
	if switchTime.IsZero() {
		return CatchUpSummary{}, fmt.Errorf("%w: no switch time", ErrMalformedListing)
	}
	if !serverNamePattern.MatchString(serverName) {
		return CatchUpSummary{}, fmt.Errorf("%w: server name %q", ErrMalformedListing, serverName)
	}
	beforeIndex, err := index(before)
	if err != nil {
		return CatchUpSummary{}, err
	}
	afterIndex, err := index(after)
	if err != nil {
		return CatchUpSummary{}, err
	}
	destinationIndex, err := index(destination)
	if err != nil {
		return CatchUpSummary{}, err
	}
	if len(beforeIndex) == 0 {
		return CatchUpSummary{}, ErrEmptySource
	}

	for key := range afterIndex {
		if _, ok := beforeIndex[key]; !ok {
			return CatchUpSummary{}, fmt.Errorf("%w: %s appeared in the shared catalogue during the pass", ErrSourceChanged, key)
		}
	}
	// A rewrite can keep size, ETag and digest while changing the modification
	// time, and the switch-time check below only sees the starting listing, so a
	// changed time is a change to the shared catalogue too.
	for key, source := range beforeIndex {
		current, ok := afterIndex[key]
		if !ok || current.Size != source.Size || current.ETag != source.ETag ||
			!current.LastModified.Equal(source.LastModified) ||
			(current.SHA256 != "" && source.SHA256 != "" && current.SHA256 != source.SHA256) {
			return CatchUpSummary{}, fmt.Errorf("%w: %s", ErrSourceChanged, key)
		}
	}
	for key, source := range beforeIndex {
		if source.LastModified.After(switchTime) {
			return CatchUpSummary{}, fmt.Errorf("%w: %s was archived to the shared catalogue at %s, after the recorded switch",
				ErrSwitchTime, key, source.LastModified.Format(time.RFC3339))
		}
	}

	matched := 0
	for key, source := range beforeIndex {
		copied, ok := destinationIndex[key]
		if !ok || copied.Size != source.Size {
			return CatchUpSummary{}, fmt.Errorf("%w: %s", ErrPartialCopy, key)
		}
		if err := sameContent(source, copied); err != nil {
			return CatchUpSummary{}, fmt.Errorf("%w: %s", err, key)
		}
		matched++
	}

	serverPrefix := serverName + "/"
	postSwitch := 0
	var postSwitchWAL []string
	for key, copied := range destinationIndex {
		if _, ok := beforeIndex[key]; ok {
			continue
		}
		if !copied.LastModified.After(switchTime) {
			return CatchUpSummary{}, fmt.Errorf("%w: %s is not in the shared catalogue and was written before the recorded switch",
				ErrUnexpectedDestinationObject, key)
		}
		// After the switch the Cluster writes only under its own server directory,
		// and a segment elsewhere could never be restored alongside its base backup.
		if !strings.HasPrefix(key, serverPrefix) {
			return CatchUpSummary{}, fmt.Errorf("%w: %s was written after the switch outside the server directory %s",
				ErrUnexpectedDestinationObject, key, serverName)
		}
		postSwitch++
		if segment := walSegmentOf(key); segment != "" {
			postSwitchWAL = append(postSwitchWAL, segment)
		}
	}

	serverSource := make(map[string]Object, len(beforeIndex))
	for key, object := range beforeIndex {
		if strings.HasPrefix(key, serverPrefix) {
			serverSource[key] = object
		}
	}
	if newestBaseBackup(serverSource) == "" {
		return CatchUpSummary{}, fmt.Errorf("%w: no complete base backup under %s", ErrNoBaseBackup, serverName)
	}
	sourceWAL := newestWAL(serverSource)
	if sourceWAL == "" {
		return CatchUpSummary{}, fmt.Errorf("%w: no WAL under %s", ErrNoArchivedWAL, serverName)
	}
	next, err := nextWALSegment(sourceWAL)
	if err != nil {
		return CatchUpSummary{}, err
	}

	// Only a segment newer than the shared catalogue's newest one can close the
	// gap. A post-switch segment at or below it, such as a retry, is ignored, so it
	// cannot stand in for a missing successor.
	first := ""
	for _, segment := range postSwitchWAL {
		if segment[:8] != sourceWAL[:8] {
			return CatchUpSummary{}, fmt.Errorf("%w: the shared catalogue ends on timeline %s and the dedicated store holds timeline %s",
				ErrWALGap, sourceWAL[:8], segment[:8])
		}
		if segment > sourceWAL && (first == "" || segment < first) {
			first = segment
		}
	}
	summary := CatchUpSummary{
		ServerName:         serverName,
		SourceObjects:      len(beforeIndex),
		MatchedObjects:     matched,
		PostSwitchObjects:  postSwitch,
		SourceNewestWAL:    sourceWAL,
		FirstPostSwitchWAL: first,
	}
	if first == "" {
		return summary, nil
	}
	if first != next {
		return CatchUpSummary{}, fmt.Errorf("%w: the shared catalogue ends at %s, so the first newer segment in the dedicated store must be %s, not %s",
			ErrWALGap, sourceWAL, next, first)
	}
	summary.Converged = true
	return summary, nil
}

// walSegmentOf returns the WAL segment name a catalogue key holds, or "" for any
// other key, such as a timeline history file or a base backup. Barman stores a
// segment under the log directory named by its first 16 characters; a segment
// filename anywhere else is not where recovery looks for it, so it is not a
// segment of this catalogue.
func walSegmentOf(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[1] != "wals" {
		return ""
	}
	segment := strings.TrimSuffix(parts[3], ".gz")
	if !walSegment.MatchString(segment) || parts[2] != segment[:16] {
		return ""
	}
	return segment
}

// nextWALSegment returns the segment that follows segment on the same timeline.
// It assumes the default 16 MiB segment size, where one log file holds segments
// 00 to FF. A larger segment number is refused rather than guessed at.
func nextWALSegment(segment string) (string, error) {
	if !walSegment.MatchString(segment) {
		return "", fmt.Errorf("%w: WAL segment %q", ErrMalformedListing, segment)
	}
	var timeline, logID, number uint32
	if _, err := fmt.Sscanf(segment, "%08X%08X%08X", &timeline, &logID, &number); err != nil {
		return "", fmt.Errorf("%w: WAL segment %q: %w", ErrMalformedListing, segment, err)
	}
	if number > 0xFF {
		return "", fmt.Errorf("%w: WAL segment %q is not a 16 MiB segment", ErrMalformedListing, segment)
	}
	number++
	if number > 0xFF {
		if logID == 0xFFFFFFFF {
			return "", fmt.Errorf("%w: WAL segment %q has no successor", ErrMalformedListing, segment)
		}
		number = 0
		logID++
	}
	return fmt.Sprintf("%08X%08X%08X", timeline, logID, number), nil
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

// newestBaseBackup returns "<server>/<backup ID>" for the latest complete base
// backup in the Barman Cloud layout: one with both its backup.info file and a
// non-empty data archive. backup.info alone describes a backup but holds none
// of the database, so a directory that lost its archive is not restorable.
func newestBaseBackup(objects map[string]Object) string {
	hasInfo := map[string]bool{}
	hasData := map[string]bool{}
	for key, object := range objects {
		parts := strings.Split(key, "/")
		if len(parts) != 4 || parts[1] != "base" || !backupID.MatchString(parts[2]) {
			continue
		}
		backup := parts[0] + "/" + parts[2]
		switch {
		case parts[3] == "backup.info":
			hasInfo[backup] = true
		case dataArchive.MatchString(parts[3]) && object.Size > 0:
			hasData[backup] = true
		}
	}
	newest, newestID := "", ""
	for backup := range hasInfo {
		if !hasData[backup] {
			continue
		}
		_, id, _ := strings.Cut(backup, "/")
		if id > newestID || (id == newestID && backup > newest) {
			newest, newestID = backup, id
		}
	}
	return newest
}

// newestWAL returns the highest WAL segment name. Segment names order by
// timeline and then position, so the lexical maximum is the newest segment. It
// uses walSegmentOf, so a segment filename outside its log directory is ignored
// here exactly as it is by the catch-up continuity check.
func newestWAL(objects map[string]Object) string {
	newest := ""
	for key := range objects {
		if segment := walSegmentOf(key); segment != "" && segment > newest {
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
	Started      string `json:"started"`
}

// ParseListing reads `mc ls --json --recursive` output ending in a completion
// record. Any entry that is not a clean success is refused, because a silently
// skipped object would make a partial listing look like a complete one. Keys
// must start at the Barman server directory, so a listing rooted one level too
// high or too low is reported as malformed rather than as a catalogue with no
// backups.
func ParseListing(r io.Reader, location string) (Listing, error) {
	var started time.Time
	objects, err := parseListing(r, location, &started)
	if err != nil {
		return Listing{}, err
	}
	return Listing{Objects: objects, Started: started}, nil
}

// parseListing reads the objects and records the completion record's start
// time, if it has one, in started.
func parseListing(r io.Reader, location string, started *time.Time) ([]Object, error) {
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
			if entry.Started != "" {
				parsed, err := time.Parse(time.RFC3339Nano, entry.Started)
				if err != nil {
					return nil, fmt.Errorf("%w: completion start: %w", ErrMalformedListing, err)
				}
				*started = parsed
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
func readListing(name, location string) (Listing, error) {
	file, err := os.Open(name)
	if err != nil {
		return Listing{}, err
	}
	listing, parseErr := ParseListing(file, location)
	if closeErr := file.Close(); closeErr != nil && parseErr == nil {
		return Listing{}, closeErr
	}
	return listing, parseErr
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
		listings := make([]Listing, 0, 3)
		for i, name := range args[3:] {
			listing, err := readListing(name, locations[i])
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			listings = append(listings, listing)
		}
		if err := BindRunStart(runStart, listings[0], listings[1], listings[2]); err != nil {
			return err
		}
		summary, err := EvaluateParity(runStart, listings[0].Objects, listings[1].Objects, listings[2].Objects)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(summary)
	}
	if len(args) == 2 && args[0] == "validate-switch-time" {
		if _, err := time.Parse(time.RFC3339, args[1]); err != nil {
			return fmt.Errorf("%w: switch time: %w", ErrMalformedListing, err)
		}
		return nil
	}
	if len(args) == 7 && args[0] == "evaluate-catch-up" {
		switchTime, err := time.Parse(time.RFC3339Nano, args[1])
		if err != nil {
			return fmt.Errorf("%w: switch time: %w", ErrMalformedListing, err)
		}
		serverName := args[2]
		if !serverNamePattern.MatchString(serverName) {
			return fmt.Errorf("%w: server name %q", ErrMalformedListing, serverName)
		}
		if args[3] == "" || args[3] == destinationBucket {
			return fmt.Errorf("%w: source bucket %q", ErrWrongSource, args[3])
		}
		locations := []string{
			args[3] + "/" + sourcePrefix,
			args[3] + "/" + sourcePrefix,
			destinationBucket + "/" + destinationPrefix,
		}
		listings := make([]Listing, 0, 3)
		for i, name := range args[4:] {
			listing, err := readListing(name, locations[i])
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			listings = append(listings, listing)
		}
		// A listing taken before the switch cannot tell a copy the Cluster archived
		// through the dedicated store from residue, so the pass must start after it.
		if !listings[0].Started.After(switchTime) {
			return fmt.Errorf("%w: the starting listing started %s, not after the recorded switch %s",
				ErrSwitchTime, listings[0].Started.Format(time.RFC3339Nano), switchTime.Format(time.RFC3339Nano))
		}
		if err := BindRunStart(listings[0].Started, listings[0], listings[1], listings[2]); err != nil {
			return err
		}
		summary, err := EvaluateCatchUp(switchTime, serverName, listings[0].Objects, listings[1].Objects, listings[2].Objects)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(summary)
	}
	return errors.New("usage: mirror-wedding-backup-catalogue validate-plan <source-bucket> <source-prefix> <source-secret> <destination-bucket> <destination-prefix> <destination-secret> | evaluate <run-start> <source-bucket> <source-before> <source-after> <destination> | evaluate-catch-up <switch-time> <server-name> <source-bucket> <source-before> <source-after> <destination> | validate-switch-time <switch-time>")
}

// main exits non-zero with the refusal reason when a plan or mirror is not trusted.
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "mirror-wedding-backup-catalogue:", err)
		os.Exit(1)
	}
}
