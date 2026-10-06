package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedMetricsManifestIsCurrent(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("../..", scriptPath))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := generate(script)
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(filepath.Join("../..", manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, expected) {
		t.Fatal("published hook differs from its source; run go run ./scripts/generate-ksail-arc-job-metrics -write")
	}
}

func TestMetricsManifestRejectsEmptyOrOversizedSource(t *testing.T) {
	for _, size := range []int{0, 65537} {
		if _, err := generate(make([]byte, size)); err == nil {
			t.Fatalf("accepted %d bytes", size)
		}
	}
}
