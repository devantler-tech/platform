package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validPlan() Plan {
	return Plan{
		Source: Location{
			Bucket: "platform-backups",
			Prefix: sourcePrefix,
			Secret: sourceSecret,
		},
		Destination: Location{
			Bucket: destinationBucket,
			Prefix: destinationPrefix,
			Secret: destinationSecret,
		},
	}
}

func TestValidatePlanAcceptsTheReviewedPlan(t *testing.T) {
	if err := ValidatePlan(validPlan()); err != nil {
		t.Fatalf("ValidatePlan() = %v, want nil", err)
	}
}

func TestValidatePlanRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
		want   error
	}{
		{"credential reuse", func(p *Plan) { p.Destination.Secret = sourceSecret }, ErrCredentialReuse},
		{"source uses the dedicated credential", func(p *Plan) { p.Source.Secret = destinationSecret }, ErrCredentialReuse},
		{"wrong destination bucket", func(p *Plan) { p.Destination.Bucket = "platform-backups-copy" }, ErrWrongDestination},
		{"wrong destination prefix", func(p *Plan) { p.Destination.Prefix = "cnpg/wedding-db/restore" }, ErrWrongDestination},
		{"destination is the source", func(p *Plan) { p.Source.Bucket = destinationBucket }, ErrWrongDestination},
		{"unexpected destination credential", func(p *Plan) { p.Destination.Secret = "other-r2" }, ErrWrongDestination},
		{"unexpected source prefix", func(p *Plan) { p.Source.Prefix = "cnpg" }, ErrWrongSource},
		{"unexpected source credential", func(p *Plan) { p.Source.Secret = "other-r2" }, ErrWrongSource},
		{"empty source bucket", func(p *Plan) { p.Source.Bucket = "" }, ErrWrongSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := validPlan()
			tt.mutate(&plan)
			if err := ValidatePlan(plan); !errors.Is(err, tt.want) {
				t.Fatalf("ValidatePlan() = %v, want %v", err, tt.want)
			}
		})
	}
}

// The command checks the locations and credentials the caller will actually
// use, so a typo in the mirror job is refused instead of silently replaced by
// the reviewed values.
func TestRunValidatePlanChecksTheCallersActualValues(t *testing.T) {
	valid := []string{"validate-plan",
		"platform-backups", sourcePrefix, sourceSecret,
		destinationBucket, destinationPrefix, destinationSecret}
	if err := run(valid, io.Discard); err != nil {
		t.Fatalf("run(valid plan) = %v, want nil", err)
	}
	tests := []struct {
		name  string
		index int
		value string
		want  error
	}{
		{"typo in the destination bucket", 4, "wedding-db-backup", ErrWrongDestination},
		{"typo in the destination credential", 6, "wedding-db-backup-r2-dedicate", ErrWrongDestination},
		{"typo in the destination prefix", 5, "cnpg/wedding", ErrWrongDestination},
		{"wrong source credential", 3, "other-r2", ErrWrongSource},
		{"wrong source prefix", 2, "cnpg", ErrWrongSource},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string(nil), valid...)
			args[tt.index] = tt.value
			if err := run(args, io.Discard); !errors.Is(err, tt.want) {
				t.Fatalf("run() = %v, want %v", err, tt.want)
			}
		})
	}
}

const (
	baseInfo = "wedding-db-20260909/base/20260908T030000/backup.info"
	baseData = "wedding-db-20260909/base/20260908T030000/data.tar.gz"
	olderWAL = "wedding-db-20260909/wals/0000000200000001/000000020000000100000003.gz"
	newerWAL = "wedding-db-20260909/wals/0000000300000001/000000030000000100000001.gz"
	lateWAL  = "wedding-db-20260909/wals/0000000300000001/000000030000000100000002.gz"

	sourceLocation      = "platform-backups/" + sourcePrefix
	destinationLocation = destinationBucket + "/" + destinationPrefix
)

var (
	runStart     = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	beforeRun    = runStart.Add(-24 * time.Hour)
	duringTheRun = runStart.Add(5 * time.Minute)
	afterTheRun  = runStart.Add(45 * time.Minute)

	dataDigest  = strings.Repeat("a", 64)
	infoDigest  = strings.Repeat("b", 64)
	otherDigest = strings.Repeat("c", 64)
)

func sourceListing() []Object {
	return []Object{
		{Key: baseInfo, Size: 1200, ETag: "a1", LastModified: beforeRun},
		{Key: baseData, Size: 90_000_000, ETag: "b2-6", SHA256: dataDigest, LastModified: beforeRun},
		{Key: olderWAL, Size: 4000, ETag: "c3", LastModified: beforeRun},
		{Key: newerWAL, Size: 4100, ETag: "d4", LastModified: beforeRun},
	}
}

// A mirror uploads large objects in a different number of parts, so the
// destination carries a different multipart ETag for identical bytes.
func destinationListing() []Object {
	return []Object{
		{Key: baseInfo, Size: 1200, ETag: "a1", LastModified: duringTheRun},
		{Key: baseData, Size: 90_000_000, ETag: "ff-4", SHA256: dataDigest, LastModified: duringTheRun},
		{Key: olderWAL, Size: 4000, ETag: "c3", LastModified: duringTheRun},
		{Key: newerWAL, Size: 4100, ETag: "d4", LastModified: duringTheRun},
	}
}

func TestEvaluateParityProvesAFullCopy(t *testing.T) {
	summary, err := EvaluateParity(runStart, sourceListing(), sourceListing(), destinationListing())
	if err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
	if summary.SourceObjects != 4 || summary.MatchedObjects != 4 || summary.VerifiedLateObjects != 0 ||
		summary.PendingObjects != 0 || !summary.Converged {
		t.Fatalf("summary = %+v, want 4 source, 4 matched, 0 late, 0 pending, converged", summary)
	}
	if summary.SourceNewestBaseBackup != "wedding-db-20260909/20260908T030000" {
		t.Fatalf("SourceNewestBaseBackup = %q", summary.SourceNewestBaseBackup)
	}
	if summary.DestinationNewestBaseBackup != summary.SourceNewestBaseBackup {
		t.Fatalf("DestinationNewestBaseBackup = %q, want %q", summary.DestinationNewestBaseBackup, summary.SourceNewestBaseBackup)
	}
	if summary.SourceNewestWAL != "000000030000000100000001" || summary.DestinationNewestWAL != "000000030000000100000001" {
		t.Fatalf("newest WAL = %q / %q", summary.SourceNewestWAL, summary.DestinationNewestWAL)
	}
}

// WAL keeps archiving to the shared store until the cutover, so objects that
// appear during the run are expected and only need to be copied by a later pass.
func TestEvaluateParityAllowsObjectsArchivedDuringTheRun(t *testing.T) {
	after := append(sourceListing(), Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: duringTheRun})
	summary, err := EvaluateParity(runStart, sourceListing(), after, destinationListing())
	if err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
	if summary.SourceObjects != 4 {
		t.Fatalf("SourceObjects = %d, want the 4 objects present when the run started", summary.SourceObjects)
	}
	// The late WAL is still only in the shared bucket, so this pass must not
	// read as cutover proof.
	if summary.PendingObjects != 1 || summary.Converged {
		t.Fatalf("summary = %+v, want 1 pending and not converged", summary)
	}
}

// An object archived during the run that the pass already copied is checked
// like any other copy, and once nothing is pending the mirror has converged.
func TestEvaluateParityVerifiesCopiesOfObjectsArchivedDuringTheRun(t *testing.T) {
	late := Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: duringTheRun}
	after := append(sourceListing(), late)
	destination := append(destinationListing(), late)
	summary, err := EvaluateParity(runStart, sourceListing(), after, destination)
	if err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
	if summary.VerifiedLateObjects != 1 || summary.PendingObjects != 0 || !summary.Converged {
		t.Fatalf("summary = %+v, want 1 verified late object, 0 pending, converged", summary)
	}
}

// backup.info describes a base backup but holds none of the database. A newer
// directory that lost its data archive must not hide an older complete one.
func TestEvaluateParitySelectsTheNewestCompleteBaseBackup(t *testing.T) {
	newerInfo := Object{Key: "wedding-db-20260909/base/20260910T030000/backup.info", Size: 1200, ETag: "f6", LastModified: beforeRun}
	before := append(sourceListing(), newerInfo)
	destination := append(destinationListing(), newerInfo)
	summary, err := EvaluateParity(runStart, before, before, destination)
	if err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
	if summary.SourceNewestBaseBackup != "wedding-db-20260909/20260908T030000" {
		t.Fatalf("SourceNewestBaseBackup = %q, want the older complete backup", summary.SourceNewestBaseBackup)
	}
}

func without(objects []Object, key string) []Object {
	kept := make([]Object, 0, len(objects))
	for _, object := range objects {
		if object.Key != key {
			kept = append(kept, object)
		}
	}
	return kept
}

// A digest computed on only one of the two source listings says nothing about
// whether the object changed; size and ETag still do.
func TestEvaluateParityIgnoresADigestPresentOnOneSourceListing(t *testing.T) {
	after := sourceListing()
	after[0].SHA256 = infoDigest
	if _, err := EvaluateParity(runStart, sourceListing(), after, destinationListing()); err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
}

func TestEvaluateParityRefusals(t *testing.T) {
	tests := []struct {
		name        string
		start       time.Time
		before      func() []Object
		after       func() []Object
		destination func() []Object
		want        error
	}{
		{
			name:        "partial copy",
			before:      sourceListing,
			after:       sourceListing,
			destination: func() []Object { return destinationListing()[:3] },
			want:        ErrPartialCopy,
		},
		{
			name:   "truncated object",
			before: sourceListing,
			after:  sourceListing,
			destination: func() []Object {
				d := destinationListing()
				d[2].Size = 10
				return d
			},
			want: ErrPartialCopy,
		},
		{
			name:   "same size different content",
			before: sourceListing,
			after:  sourceListing,
			destination: func() []Object {
				d := destinationListing()
				d[1].SHA256 = otherDigest
				return d
			},
			want: ErrChecksumMismatch,
		},
		{
			name:   "single-part etag differs",
			before: sourceListing,
			after:  sourceListing,
			destination: func() []Object {
				d := destinationListing()
				d[0].ETag = "zz"
				return d
			},
			want: ErrChecksumMismatch,
		},
		{
			name:   "multipart object without a digest",
			before: sourceListing,
			after:  sourceListing,
			destination: func() []Object {
				d := destinationListing()
				d[1].SHA256 = ""
				return d
			},
			want: ErrUnverifiable,
		},
		{
			name:        "source object removed during the run",
			before:      sourceListing,
			after:       func() []Object { return sourceListing()[1:] },
			destination: destinationListing,
			want:        ErrSourceChanged,
		},
		{
			name:   "source object rewritten during the run",
			before: sourceListing,
			after: func() []Object {
				s := sourceListing()
				s[0].ETag = "rewritten"
				return s
			},
			destination: destinationListing,
			want:        ErrSourceChanged,
		},
		{
			name:   "source digests disagree",
			before: sourceListing,
			after: func() []Object {
				s := sourceListing()
				s[1].SHA256 = otherDigest
				return s
			},
			destination: destinationListing,
			want:        ErrSourceChanged,
		},
		{
			// The starting listing stopped part-way: the base backup existed
			// before the run but was never listed, so it was never checked.
			name:        "truncated starting listing",
			before:      func() []Object { return sourceListing()[2:] },
			after:       sourceListing,
			destination: func() []Object { return destinationListing()[2:] },
			want:        ErrIncompleteListing,
		},
		{
			name:        "missing run start",
			start:       time.Time{},
			before:      sourceListing,
			after:       sourceListing,
			destination: destinationListing,
			want:        ErrMalformedListing,
		},
		{
			name:        "empty source",
			before:      func() []Object { return nil },
			after:       func() []Object { return nil },
			destination: destinationListing,
			want:        ErrEmptySource,
		},
		{
			name:        "source without a base backup",
			before:      func() []Object { return sourceListing()[2:] },
			after:       func() []Object { return sourceListing()[2:] },
			destination: func() []Object { return destinationListing()[2:] },
			want:        ErrNoBaseBackup,
		},
		{
			// A base backup cannot be restored to a consistent state without the
			// WAL archived alongside it, so a catalogue with none is not a
			// recoverable history however faithfully it was copied.
			name:        "source without archived WAL",
			before:      func() []Object { return sourceListing()[:2] },
			after:       func() []Object { return sourceListing()[:2] },
			destination: func() []Object { return destinationListing()[:2] },
			want:        ErrNoArchivedWAL,
		},
		{
			// backup.info survived but the data archive did not, so there is no
			// database payload to restore even though WAL is present.
			name:        "base backup without its data archive",
			before:      func() []Object { return without(sourceListing(), baseData) },
			after:       func() []Object { return without(sourceListing(), baseData) },
			destination: func() []Object { return without(destinationListing(), baseData) },
			want:        ErrNoBaseBackup,
		},
		{
			name: "base backup with an empty data archive",
			before: func() []Object {
				s := sourceListing()
				s[1].Size = 0
				return s
			},
			after: func() []Object {
				s := sourceListing()
				s[1].Size = 0
				return s
			},
			destination: func() []Object {
				d := destinationListing()
				d[1].Size = 0
				return d
			},
			want: ErrNoBaseBackup,
		},
		{
			// Residue from a failed earlier copy is in neither source listing, so
			// nothing proves it belongs in the dedicated catalogue.
			name:   "destination object absent from both source listings",
			before: sourceListing,
			after:  sourceListing,
			destination: func() []Object {
				return append(destinationListing(), Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: duringTheRun})
			},
			want: ErrUnexpectedDestinationObject,
		},
		{
			name:   "truncated copy of an object archived during the run",
			before: sourceListing,
			after: func() []Object {
				return append(sourceListing(), Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: duringTheRun})
			},
			destination: func() []Object {
				return append(destinationListing(), Object{Key: lateWAL, Size: 10, ETag: "e5", LastModified: duringTheRun})
			},
			want: ErrPartialCopy,
		},
		{
			name:   "altered copy of an object archived during the run",
			before: sourceListing,
			after: func() []Object {
				return append(sourceListing(), Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: duringTheRun})
			},
			destination: func() []Object {
				return append(destinationListing(), Object{Key: lateWAL, Size: 4200, ETag: "zz", LastModified: duringTheRun})
			},
			want: ErrChecksumMismatch,
		},
		{
			name: "duplicate key in a listing",
			before: func() []Object {
				return append(sourceListing(), sourceListing()[0])
			},
			after:       sourceListing,
			destination: destinationListing,
			want:        ErrMalformedListing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := runStart
			if tt.name == "missing run start" {
				start = tt.start
			}
			_, err := EvaluateParity(start, tt.before(), tt.after(), tt.destination())
			if !errors.Is(err, tt.want) {
				t.Fatalf("EvaluateParity() = %v, want %v", err, tt.want)
			}
		})
	}
}

const modified = `"lastModified":"2026-09-12T10:00:00Z",`

func fileLine(key, extra string) string {
	return `{"status":"success","type":"file",` + modified + `"size":1,"key":"` + key + `","etag":"a"` + extra + `}`
}

func completeLineAt(location string, files int) string {
	return `{"status":"success","type":"listing-complete","location":"` + location + `","files":` + itoa(files) + `}`
}

func completeLine(files int) string {
	return completeLineAt(sourceLocation, files)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func TestParseListingReadsMcJSONLines(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"success","type":"file","lastModified":"2026-09-12T10:00:00.123Z","size":1200,"key":"` + baseInfo + `","etag":"a1"}`,
		`{"status":"success","type":"folder","size":0,"key":"wedding-db-20260909/base/"}`,
		`{"status":"success","type":"file","lastModified":"2026-09-12T10:00:00Z","size":4000,"key":"` + olderWAL + `","etag":"c3","sha256":"` + dataDigest + `"}`,
		completeLine(2),
		``,
	}, "\n")
	listing, err := ParseListing(strings.NewReader(input), sourceLocation)
	if err != nil {
		t.Fatalf("ParseListing() = %v, want nil", err)
	}
	if len(listing.Objects) != 2 {
		t.Fatalf("len(objects) = %d, want 2 files and no folders", len(listing.Objects))
	}
	want := Object{Key: olderWAL, Size: 4000, ETag: "c3", SHA256: dataDigest, LastModified: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)}
	if listing.Objects[1] != want {
		t.Fatalf("objects[1] = %+v, want %+v", listing.Objects[1], want)
	}
	if !listing.Started.IsZero() {
		t.Fatalf("Started = %v, want zero for a completion record without a start", listing.Started)
	}
}

func TestParseListingReadsTheStartTime(t *testing.T) {
	input := fileLine(baseInfo, "") + "\n" + completeLineStarted(sourceLocation, 1, "2026-09-13T10:00:00Z")
	listing, err := ParseListing(strings.NewReader(input), sourceLocation)
	if err != nil {
		t.Fatalf("ParseListing() = %v, want nil", err)
	}
	if !listing.Started.Equal(runStart) {
		t.Fatalf("Started = %v, want %v", listing.Started, runStart)
	}
	bad := fileLine(baseInfo, "") + "\n" + completeLineStarted(sourceLocation, 1, "yesterday")
	if _, err := ParseListing(strings.NewReader(bad), sourceLocation); !errors.Is(err, ErrMalformedListing) {
		t.Fatalf("ParseListing(bad start) = %v, want %v", err, ErrMalformedListing)
	}
}

// An empty catalogue still needs the completion record, and then parses as
// empty rather than as malformed; EvaluateParity refuses an empty source.
func TestParseListingAcceptsACompletedEmptyListing(t *testing.T) {
	listing, err := ParseListing(strings.NewReader(completeLine(0)+"\n"), sourceLocation)
	if err != nil || len(listing.Objects) != 0 {
		t.Fatalf("ParseListing() = %v, %v, want no objects and nil", listing.Objects, err)
	}
}

func TestParseListingRefusesFailedEntries(t *testing.T) {
	tests := map[string]string{
		"error status":            `{"status":"error","type":"file",` + modified + `"size":1,"key":"` + baseInfo + `","etag":"a"}`,
		"not json":                `not json`,
		"absolute key":            fileLine("/"+baseInfo, ""),
		"parent key":              fileLine("wedding-db-20260909/base/../x", ""),
		"negative size":           `{"status":"success","type":"file",` + modified + `"size":-1,"key":"` + baseInfo + `","etag":"a"}`,
		"missing etag":            `{"status":"success","type":"file",` + modified + `"size":1,"key":"` + baseInfo + `"}`,
		"missing last modified":   `{"status":"success","type":"file","size":1,"key":"` + baseInfo + `","etag":"a"}`,
		"listing rooted too high": fileLine("wedding-db/"+baseInfo, ""),
		"listing rooted too low":  fileLine("base/20260908T030000/backup.info", ""),
		"placeholder sha256":      fileLine(baseInfo, `,"sha256":"sha-data"`),
		"short sha256":            fileLine(baseInfo, `,"sha256":"`+strings.Repeat("a", 63)+`"`),
		"uppercase sha256":        fileLine(baseInfo, `,"sha256":"`+strings.Repeat("A", 64)+`"`),
		"record after completion": completeLine(0) + "\n" + fileLine(baseInfo, ""),
		"negative completion":     `{"status":"success","type":"listing-complete","location":"` + sourceLocation + `","files":-1}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if name != "record after completion" && name != "negative completion" && !strings.HasPrefix(input, "not json") {
				input += "\n" + completeLine(1)
			}
			if _, err := ParseListing(strings.NewReader(input), sourceLocation); !errors.Is(err, ErrMalformedListing) {
				t.Fatalf("ParseListing() = %v, want %v", err, ErrMalformedListing)
			}
		})
	}
}

// Raw `mc ls` output has no terminal record, so a listing cut short looks like
// a complete one. Only the wrapper's completion record, written after mc exits
// successfully, proves the listing ran to the end.
func TestParseListingRequiresCompletionProof(t *testing.T) {
	tests := map[string]string{
		"no completion record":          fileLine(baseInfo, "") + "\n" + fileLine(olderWAL, ""),
		"completion count too high":     fileLine(baseInfo, "") + "\n" + completeLine(2),
		"completion count too low":      fileLine(baseInfo, "") + "\n" + fileLine(olderWAL, "") + "\n" + completeLine(1),
		"empty input":                   "",
		"completion without file count": fileLine(baseInfo, "") + "\n" + `{"status":"success","type":"listing-complete","location":"` + sourceLocation + `"}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseListing(strings.NewReader(input), sourceLocation); !errors.Is(err, ErrIncompleteListing) {
				t.Fatalf("ParseListing() = %v, want %v", err, ErrIncompleteListing)
			}
		})
	}
}

// Raw `mc ls` keys carry no bucket, so a complete listing of the wrong bucket
// is indistinguishable from the right one. The completion record names the
// location the wrapper listed, and it must be the one being evaluated.
func TestParseListingBindsTheListingToItsLocation(t *testing.T) {
	tests := map[string]string{
		"listing of another bucket":     fileLine(baseInfo, "") + "\n" + completeLineAt("platform-backups-copy/"+sourcePrefix, 1),
		"listing of another prefix":     fileLine(baseInfo, "") + "\n" + completeLineAt("platform-backups/cnpg", 1),
		"completion without a location": fileLine(baseInfo, "") + "\n" + `{"status":"success","type":"listing-complete","files":1}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseListing(strings.NewReader(input), sourceLocation); !errors.Is(err, ErrListingLocation) {
				t.Fatalf("ParseListing() = %v, want %v", err, ErrListingLocation)
			}
		})
	}
}

func completeLineStarted(location string, files int, started string) string {
	return `{"status":"success","type":"listing-complete","location":"` + location + `","started":"` + started + `","files":` + itoa(files) + `}`
}

// writeListing writes a listing whose completion record carries started, or no
// start time when started is empty.
func writeListing(t *testing.T, name, location, started string, keys ...string) string {
	t.Helper()
	lines := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		lines = append(lines, fileLine(key, ""))
	}
	if started == "" {
		lines = append(lines, completeLineAt(location, len(keys)))
	} else {
		lines = append(lines, completeLineStarted(location, len(keys), started))
	}
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

const (
	runStartArg        = "2026-09-13T10:00:00Z"
	afterStarted       = "2026-09-13T10:30:00Z"
	destinationStarted = "2026-09-13T10:45:00Z"
)

// A copy that landed in the wrong bucket produces matching listings there, so
// the destination listing must be proven to come from the reviewed bucket.
func TestRunEvaluateRefusesListingsFromTheWrongLocation(t *testing.T) {
	keys := []string{baseInfo, baseData, olderWAL}
	before := writeListing(t, "before", sourceLocation, runStartArg, keys...)
	after := writeListing(t, "after", sourceLocation, afterStarted, keys...)
	good := writeListing(t, "destination", destinationLocation, destinationStarted, keys...)
	if err := run([]string{"evaluate", runStartArg, "platform-backups", before, after, good}, io.Discard); err != nil {
		t.Fatalf("run(evaluate) = %v, want nil", err)
	}
	wrongBucket := writeListing(t, "wrong", "platform-backups-copy/"+destinationPrefix, destinationStarted, keys...)
	if err := run([]string{"evaluate", runStartArg, "platform-backups", before, after, wrongBucket}, io.Discard); !errors.Is(err, ErrListingLocation) {
		t.Fatalf("run(evaluate, wrong destination) = %v, want %v", err, ErrListingLocation)
	}
	swapped := writeListing(t, "swapped", destinationLocation, runStartArg, keys...)
	if err := run([]string{"evaluate", runStartArg, "platform-backups", swapped, after, good}, io.Discard); !errors.Is(err, ErrListingLocation) {
		t.Fatalf("run(evaluate, source listed from destination) = %v, want %v", err, ErrListingLocation)
	}
}

// The command binds the run start it is given to the starting listing, so a
// start time left over from an earlier pass is refused end to end.
func TestRunEvaluateRefusesARunStartFromAnotherPass(t *testing.T) {
	keys := []string{baseInfo, baseData, olderWAL}
	before := writeListing(t, "before", sourceLocation, runStartArg, keys...)
	after := writeListing(t, "after", sourceLocation, afterStarted, keys...)
	destination := writeListing(t, "destination", destinationLocation, destinationStarted, keys...)
	stale := "2026-09-12T10:00:00Z"
	if err := run([]string{"evaluate", stale, "platform-backups", before, after, destination}, io.Discard); !errors.Is(err, ErrRunStartMismatch) {
		t.Fatalf("run(evaluate, stale run start) = %v, want %v", err, ErrRunStartMismatch)
	}
}

// The command binds the destination listing to this pass too, so a destination
// listing without a start time, or one taken before the ending listing, is
// refused end to end rather than proving a copy from stale evidence.
func TestRunEvaluateRefusesADestinationListingFromAnotherPass(t *testing.T) {
	keys := []string{baseInfo, baseData, olderWAL}
	before := writeListing(t, "before", sourceLocation, runStartArg, keys...)
	after := writeListing(t, "after", sourceLocation, afterStarted, keys...)
	tests := []struct {
		name    string
		started string
	}{
		{"destination listing without a start time", ""},
		{"destination listing started before the ending listing", runStartArg},
		{"destination listing from an earlier pass", "2026-09-12T10:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			destination := writeListing(t, "destination", destinationLocation, tt.started, keys...)
			if err := run([]string{"evaluate", runStartArg, "platform-backups", before, after, destination}, io.Discard); !errors.Is(err, ErrRunStartMismatch) {
				t.Fatalf("run(evaluate) = %v, want %v", err, ErrRunStartMismatch)
			}
		})
	}
}

func TestBindRunStart(t *testing.T) {
	started := func(at time.Time) Listing { return Listing{Started: at} }
	if err := BindRunStart(runStart, started(runStart), started(duringTheRun), started(duringTheRun)); err != nil {
		t.Fatalf("BindRunStart(this pass, destination listed with the ending listing) = %v, want nil", err)
	}
	if err := BindRunStart(runStart, started(runStart), started(duringTheRun), started(afterTheRun)); err != nil {
		t.Fatalf("BindRunStart(this pass, destination listed after the ending listing) = %v, want nil", err)
	}
	tests := []struct {
		name                       string
		start                      time.Time
		before, after, destination Listing
	}{
		// Objects written between an earlier pass and this one would otherwise
		// pass as archived during the run and never be checked.
		{"run start from an earlier pass", beforeRun, started(runStart), started(duringTheRun), started(afterTheRun)},
		{"starting listing without a start time", runStart, Listing{}, started(duringTheRun), started(afterTheRun)},
		// A zero run start equals a zero listing start and is not after it, so
		// only the explicit missing-start check refuses this.
		{"no start time anywhere", time.Time{}, Listing{}, Listing{}, Listing{}},
		{"ending listing without a start time", runStart, started(runStart), Listing{}, started(afterTheRun)},
		{"ending listing started before the starting listing", runStart, started(runStart), started(beforeRun), started(afterTheRun)},
		// A destination listing from an earlier pass can show objects the
		// destination has since lost, so parity would converge on stale evidence.
		{"destination listing without a start time", runStart, started(runStart), started(duringTheRun), Listing{}},
		{"destination listing started before the ending listing", runStart, started(runStart), started(duringTheRun), started(runStart)},
		{"destination listing from an earlier pass", runStart, started(runStart), started(duringTheRun), started(beforeRun)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := BindRunStart(tt.start, tt.before, tt.after, tt.destination); !errors.Is(err, ErrRunStartMismatch) {
				t.Fatalf("BindRunStart() = %v, want %v", err, ErrRunStartMismatch)
			}
		})
	}
}
