package main

import (
	"errors"
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

const (
	baseInfo = "wedding-db-20260909/base/20260908T030000/backup.info"
	baseData = "wedding-db-20260909/base/20260908T030000/data.tar.gz"
	olderWAL = "wedding-db-20260909/wals/0000000200000001/000000020000000100000003.gz"
	newerWAL = "wedding-db-20260909/wals/0000000300000001/000000030000000100000001.gz"
	lateWAL  = "wedding-db-20260909/wals/0000000300000001/000000030000000100000002.gz"
)

var (
	runStart     = time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	beforeRun    = runStart.Add(-24 * time.Hour)
	duringTheRun = runStart.Add(5 * time.Minute)
)

func sourceListing() []Object {
	return []Object{
		{Key: baseInfo, Size: 1200, ETag: "a1", LastModified: beforeRun},
		{Key: baseData, Size: 90_000_000, ETag: "b2-6", SHA256: "sha-data", LastModified: beforeRun},
		{Key: olderWAL, Size: 4000, ETag: "c3", LastModified: beforeRun},
		{Key: newerWAL, Size: 4100, ETag: "d4", LastModified: beforeRun},
	}
}

// A mirror uploads large objects in a different number of parts, so the
// destination carries a different multipart ETag for identical bytes.
func destinationListing() []Object {
	return []Object{
		{Key: baseInfo, Size: 1200, ETag: "a1", LastModified: duringTheRun},
		{Key: baseData, Size: 90_000_000, ETag: "ff-4", SHA256: "sha-data", LastModified: duringTheRun},
		{Key: olderWAL, Size: 4000, ETag: "c3", LastModified: duringTheRun},
		{Key: newerWAL, Size: 4100, ETag: "d4", LastModified: duringTheRun},
	}
}

func TestEvaluateParityProvesAFullCopy(t *testing.T) {
	summary, err := EvaluateParity(runStart, sourceListing(), sourceListing(), destinationListing())
	if err != nil {
		t.Fatalf("EvaluateParity() = %v, want nil", err)
	}
	if summary.SourceObjects != 4 || summary.MatchedObjects != 4 || summary.ExtraDestinationObjects != 0 {
		t.Fatalf("summary counts = %+v, want 4 source, 4 matched, 0 extra", summary)
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
}

// A digest computed on only one of the two source listings says nothing about
// whether the object changed; size and ETag still do.
func TestEvaluateParityIgnoresADigestPresentOnOneSourceListing(t *testing.T) {
	after := sourceListing()
	after[0].SHA256 = "sha-info"
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
				d[1].SHA256 = "sha-other"
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
				s[1].SHA256 = "sha-rewritten"
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

func TestParseListingReadsMcJSONLines(t *testing.T) {
	input := strings.Join([]string{
		`{"status":"success","type":"file","lastModified":"2026-09-12T10:00:00.123Z","size":1200,"key":"` + baseInfo + `","etag":"a1"}`,
		`{"status":"success","type":"folder","size":0,"key":"wedding-db-20260909/base/"}`,
		`{"status":"success","type":"file","lastModified":"2026-09-12T10:00:00Z","size":4000,"key":"` + olderWAL + `","etag":"c3","sha256":"abc"}`,
		``,
	}, "\n")
	objects, err := ParseListing(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseListing() = %v, want nil", err)
	}
	if len(objects) != 2 {
		t.Fatalf("len(objects) = %d, want 2 files and no folders", len(objects))
	}
	want := Object{Key: olderWAL, Size: 4000, ETag: "c3", SHA256: "abc", LastModified: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)}
	if objects[1] != want {
		t.Fatalf("objects[1] = %+v, want %+v", objects[1], want)
	}
}

func TestParseListingRefusesFailedEntries(t *testing.T) {
	const modified = `"lastModified":"2026-09-12T10:00:00Z",`
	tests := map[string]string{
		"error status":            `{"status":"error","type":"file",` + modified + `"size":1,"key":"` + baseInfo + `","etag":"a"}`,
		"not json":                `not json`,
		"absolute key":            `{"status":"success","type":"file",` + modified + `"size":1,"key":"/` + baseInfo + `","etag":"a"}`,
		"parent key":              `{"status":"success","type":"file",` + modified + `"size":1,"key":"wedding-db-20260909/base/../x","etag":"a"}`,
		"negative size":           `{"status":"success","type":"file",` + modified + `"size":-1,"key":"` + baseInfo + `","etag":"a"}`,
		"missing etag":            `{"status":"success","type":"file",` + modified + `"size":1,"key":"` + baseInfo + `"}`,
		"missing last modified":   `{"status":"success","type":"file","size":1,"key":"` + baseInfo + `","etag":"a"}`,
		"listing rooted too high": `{"status":"success","type":"file",` + modified + `"size":1,"key":"wedding-db/` + baseInfo + `","etag":"a"}`,
		"listing rooted too low":  `{"status":"success","type":"file",` + modified + `"size":1,"key":"base/20260908T030000/backup.info","etag":"a"}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseListing(strings.NewReader(input)); !errors.Is(err, ErrMalformedListing) {
				t.Fatalf("ParseListing() = %v, want %v", err, ErrMalformedListing)
			}
		})
	}
}
