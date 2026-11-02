package backupcatalogue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHistoricalMetadataCountRetainsUnknownProducerAndPrivateProvenance(t *testing.T) {
	ctx, repository, _ := repositoryFixture(t, false)
	catalogue, err := readCatalogue(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	// The genuine encrypted fixture has an unrecognized historical source and
	// no current Velero requester tags. It must remain a candidate, not vanish.
	if catalogue.MetadataEndingBefore(time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)) != 1 ||
		catalogue.MetadataEndingBefore(time.Date(2026, 6, 16, 9, 21, 54, 0, time.UTC)) != 0 {
		t.Fatal("historical metadata was lost or assigned to the wrong interval")
	}
	data, err := json.Marshal(catalogue)
	if err != nil || strings.Contains(string(data), "fixture-host") || strings.Contains(string(data), "/fixture") {
		t.Fatal("private provenance escaped")
	}
}
