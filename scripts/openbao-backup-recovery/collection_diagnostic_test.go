package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCollectionDiagnosticRefusesPrivateErrorContent(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{stageError("snapshot-download"), "snapshot-download"},
		{stageError("recovery-key-download"), "recovery-key-download"},
		{stageError("PRIVATE_SYNTHETIC_VALUE"), "collection"},
		{stageError("snapshot-download\nPRIVATE_SYNTHETIC_VALUE"), "collection"},
		{errors.New("PRIVATE_SYNTHETIC_VALUE"), "collection"},
	} {
		got := inputFailureStage(tc.err)
		if got != tc.want || strings.Contains(got, "PRIVATE_SYNTHETIC") {
			t.Fatal("diagnostic did not preserve the closed stage vocabulary")
		}
	}
}
