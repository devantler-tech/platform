package main

import (
	"io"
	"testing"
)

type rejectedOutput struct{}

func (rejectedOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRecoveryRefusesFailedOutput(t *testing.T) {
	env := dispatchEnvironment()
	code := runObservation(t.Context(), func(k string) string { return env[k] }, fixedReader(t, goodEvidence()), map[string]string{"managed-resources.tsv": "sample-v1"}, "reviewed-checksum", observationTime, rejectedOutput{})
	if code != 3 {
		t.Fatalf("unwritten recovery receipt must remain unknown: code=%d", code)
	}
}
