package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	oldServerInfo = "wedding-db/base/20260813T030000/backup.info"
	oldServerData = "wedding-db/base/20260813T030000/data.tar.gz"
	oldServerWAL  = "wedding-db/wals/0000000100000000/000000010000000000000007.gz"

	prunedInfo  = "wedding-db-20260909/base/20260901T030000/backup.info"
	prunedData  = "wedding-db-20260909/base/20260901T030000/data.tar.gz"
	prunedWAL   = "wedding-db-20260909/wals/0000000200000000/0000000200000000000000FE.gz"
	prunedLabel = "wedding-db-20260909/wals/0000000200000000/0000000200000000000000FE.00000028.backup"
	history     = "wedding-db-20260909/wals/00000003.history"
	postSwitch  = "wedding-db-20260909/wals/0000000300000001/000000030000000100000009.gz"
)

var (
	sharedListed    = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	dedicatedListed = sharedListed.Add(time.Minute)
	// Written before the oldest dedicated base backup started and before the
	// retention window began.
	longAgo = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
)

func sharedCatalogue() Listing {
	return Listing{Started: sharedListed, Objects: sourceListing()}
}

// The dedicated store keeps archiving after the switch, so it holds more than
// the shared copy.
func dedicatedCatalogue() Listing {
	objects := append(destinationListing(),
		Object{Key: postSwitch, Size: 4300, ETag: "f6", LastModified: sharedListed})
	return Listing{Started: dedicatedListed, Objects: objects}
}

func TestEvaluateCoverageProvesTheDedicatedCatalogueCoversTheSharedOne(t *testing.T) {
	summary, err := EvaluateCoverage(sharedCatalogue(), dedicatedCatalogue())
	if err != nil {
		t.Fatalf("EvaluateCoverage() = %v, want nil", err)
	}
	if summary.SharedObjects != 4 || summary.MatchedObjects != 4 ||
		summary.RetentionPrunedObjects != 0 || !summary.Covered {
		t.Fatalf("summary = %+v, want 4 shared, 4 matched, 0 pruned, covered", summary)
	}
}

// Retention runs on the dedicated store only, so the shared copy outlives
// backups the dedicated store has pruned. Those are older than everything the
// dedicated store keeps and are not a reason to keep the shared copy.
func TestEvaluateCoverageAcceptsObjectsTheDedicatedStorePrunedByRetention(t *testing.T) {
	shared := sharedCatalogue()
	shared.Objects = append(shared.Objects,
		Object{Key: prunedInfo, Size: 1100, ETag: "p1", LastModified: longAgo},
		Object{Key: prunedData, Size: 80_000_000, ETag: "p2-5", LastModified: longAgo},
		Object{Key: prunedWAL, Size: 3900, ETag: "p3", LastModified: longAgo},
		Object{Key: prunedLabel, Size: 300, ETag: "p4", LastModified: longAgo},
	)
	summary, err := EvaluateCoverage(shared, dedicatedCatalogue())
	if err != nil {
		t.Fatalf("EvaluateCoverage() = %v, want nil", err)
	}
	if summary.MatchedObjects != 4 || summary.RetentionPrunedObjects != 4 {
		t.Fatalf("summary = %+v, want 4 matched and 4 pruned", summary)
	}
}

func TestEvaluateCoverageRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(shared, dedicated *Listing)
		want   error
	}{
		{"an object the dedicated store never held", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects, Object{Key: lateWAL, Size: 4200, ETag: "e5", LastModified: beforeRun})
		}, ErrNotCovered},
		{"a missing object inside the retained range", func(_, dedicated *Listing) {
			dedicated.Objects = dedicated.Objects[:3]
		}, ErrNotCovered},
		{"a server directory the dedicated store holds nothing of", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects,
				Object{Key: oldServerInfo, Size: 900, ETag: "o1", LastModified: longAgo},
				Object{Key: oldServerData, Size: 900, ETag: "o2", LastModified: longAgo},
				Object{Key: oldServerWAL, Size: 900, ETag: "o3", LastModified: longAgo})
		}, ErrNotCovered},
		{"a timeline history file is never pruned", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects, Object{Key: history, Size: 80, ETag: "h1", LastModified: longAgo})
		}, ErrNotCovered},
		{"WAL archived after the oldest dedicated backup started", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects, Object{Key: prunedWAL, Size: 3900, ETag: "p3", LastModified: beforeRun})
		}, ErrNotCovered},
		{"an old backup missing while the oldest dedicated backup is inside the window", func(shared, dedicated *Listing) {
			shared.Started = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
			dedicated.Started = shared.Started.Add(time.Minute)
			shared.Objects = append(shared.Objects,
				Object{Key: prunedInfo, Size: 1100, ETag: "p1", LastModified: longAgo},
				Object{Key: prunedData, Size: 80_000_000, ETag: "p2-5", LastModified: longAgo})
		}, ErrNotCovered},
		{"an object written within the clock margin of the oldest dedicated backup", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects,
				Object{Key: prunedWAL, Size: 3900, ETag: "p3", LastModified: time.Date(2026, 9, 8, 2, 30, 0, 0, time.UTC)})
		}, ErrNotCovered},
		{"a dedicated backup ID that is not a real time", func(_, dedicated *Listing) {
			dedicated.Objects = append(dedicated.Objects,
				Object{Key: "wedding-db-20260909/base/20260231T030000/backup.info", Size: 1, ETag: "x1", LastModified: beforeRun},
				Object{Key: "wedding-db-20260909/base/20260231T030000/data.tar.gz", Size: 1, ETag: "x2", LastModified: beforeRun})
		}, ErrMalformedListing},
		{"a non-empty object hashed as empty", func(shared, dedicated *Listing) {
			shared.Objects[1].SHA256 = emptyDigest
			dedicated.Objects[1].SHA256 = emptyDigest
		}, ErrUnverifiable},
		{"a different size", func(_, dedicated *Listing) {
			dedicated.Objects[0].Size++
		}, ErrPartialCopy},
		{"a different single-part checksum", func(_, dedicated *Listing) {
			dedicated.Objects[0].ETag = "zz"
		}, ErrChecksumMismatch},
		{"a different content digest", func(_, dedicated *Listing) {
			dedicated.Objects[1].SHA256 = otherDigest
		}, ErrChecksumMismatch},
		{"a multipart object nobody hashed", func(shared, _ *Listing) {
			shared.Objects[1].SHA256 = ""
		}, ErrUnverifiable},
		{"an empty shared listing", func(shared, _ *Listing) {
			shared.Objects = nil
		}, ErrEmptySource},
		{"a dedicated listing taken before the shared one", func(_, dedicated *Listing) {
			dedicated.Started = sharedListed.Add(-time.Second)
		}, ErrStaleListing},
		{"a dedicated listing with no start time", func(_, dedicated *Listing) {
			dedicated.Started = time.Time{}
		}, ErrStaleListing},
		{"a shared listing with no start time", func(shared, _ *Listing) {
			shared.Started = time.Time{}
		}, ErrStaleListing},
		{"a repeated shared key", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects, shared.Objects[0])
		}, ErrMalformedListing},
		{"a dedicated catalogue with no complete base backup", func(shared, dedicated *Listing) {
			shared.Objects = shared.Objects[2:]
			dedicated.Objects = dedicated.Objects[2:]
		}, ErrNoBaseBackup},
		{"a dedicated catalogue whose newest base backup is older", func(shared, _ *Listing) {
			shared.Objects = append(shared.Objects,
				Object{Key: "wedding-db-20260909/base/20260920T030000/backup.info", Size: 1, ETag: "n1", LastModified: beforeRun},
				Object{Key: "wedding-db-20260909/base/20260920T030000/data.tar.gz", Size: 1, ETag: "n2", LastModified: beforeRun})
		}, ErrNotCovered},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shared, dedicated := sharedCatalogue(), dedicatedCatalogue()
			tt.mutate(&shared, &dedicated)
			if _, err := EvaluateCoverage(shared, dedicated); !errors.Is(err, tt.want) {
				t.Fatalf("EvaluateCoverage() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestMultipartKeysNamesSharedKeysAnETagCannotProve(t *testing.T) {
	shared := []Object{
		{Key: baseInfo, ETag: "a1"},
		{Key: baseData, ETag: "b2-6"},
		{Key: olderWAL, ETag: "c3"},
		{Key: newerWAL, ETag: "d4-2"},
	}
	dedicated := []Object{
		{Key: baseInfo, ETag: "a1"},
		{Key: baseData, ETag: "ff"},
		{Key: olderWAL, ETag: "c3-2"},
		{Key: postSwitch, ETag: "g7-2"},
	}
	keys, err := MultipartKeys(shared, dedicated)
	if err != nil {
		t.Fatalf("MultipartKeys() = %v", err)
	}
	if got, want := strings.Join(keys, ","), baseData+","+olderWAL; got != want {
		t.Fatalf("MultipartKeys() = %q, want %q", got, want)
	}
}

func TestApplySums(t *testing.T) {
	objects := sourceListing()
	objects[1].SHA256 = ""
	if err := ApplySums(objects, strings.NewReader(baseData+"\t"+dataDigest+"\n\n")); err != nil {
		t.Fatalf("ApplySums() = %v, want nil", err)
	}
	if objects[1].SHA256 != dataDigest {
		t.Fatalf("SHA256 = %q, want %q", objects[1].SHA256, dataDigest)
	}
	for name, sums := range map[string]string{
		"an unlisted key":    lateWAL + "\t" + dataDigest + "\n",
		"a repeated key":     baseData + "\t" + dataDigest + "\n" + baseData + "\t" + dataDigest + "\n",
		"a malformed digest": baseData + "\tnot-a-digest\n",
		"no separator":       baseData + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ApplySums(sourceListing(), strings.NewReader(sums)); !errors.Is(err, ErrMalformedSums) {
				t.Fatalf("ApplySums() = %v, want %v", err, ErrMalformedSums)
			}
		})
	}
}

func TestRunEvaluateCoverageEndToEnd(t *testing.T) {
	keys := []string{baseInfo, baseData, olderWAL}
	shared := writeListing(t, "shared", sourceLocation, "2026-10-05T09:00:00Z", keys...)
	dedicated := writeListing(t, "dedicated", destinationLocation, "2026-10-05T09:01:00Z", keys...)
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var keysOut bytes.Buffer
	if err := run([]string{"coverage-multipart-keys", "platform-backups", shared, dedicated}, &keysOut); err != nil {
		t.Fatalf("run(coverage-multipart-keys) = %v, want nil", err)
	}

	var out bytes.Buffer
	if err := run([]string{"evaluate-coverage", "platform-backups", shared, dedicated, empty, empty}, &out); err != nil {
		t.Fatalf("run(evaluate-coverage) = %v, want nil", err)
	}
	var summary CoverageSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatalf("summary is not JSON: %v", err)
	}
	if !summary.Covered || summary.SharedObjects != len(keys) {
		t.Fatalf("summary = %+v", summary)
	}

	// The listings are bound to their buckets, so swapping them, or naming the
	// dedicated bucket as the shared one, is refused.
	if err := run([]string{"evaluate-coverage", "platform-backups", dedicated, shared, empty, empty}, &out); !errors.Is(err, ErrListingLocation) {
		t.Fatalf("run(evaluate-coverage, swapped) = %v, want %v", err, ErrListingLocation)
	}
	if err := run([]string{"evaluate-coverage", destinationBucket, shared, dedicated, empty, empty}, &out); !errors.Is(err, ErrWrongSource) {
		t.Fatalf("run(evaluate-coverage, dedicated as shared) = %v, want %v", err, ErrWrongSource)
	}
	if err := run([]string{"evaluate-coverage", "platform-backups", shared, dedicated, empty, filepath.Join(t.TempDir(), "absent")}, &out); err == nil {
		t.Fatal("run(evaluate-coverage, missing sums) = nil, want an error")
	}
}
