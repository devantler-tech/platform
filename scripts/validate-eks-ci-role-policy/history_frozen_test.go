package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// frozenHistoryDigests holds the only accepted contents of approved-surface-history.md: main when
// the freeze was enforced, and main once #5693, queued ahead of the freeze, lands its entry.
var frozenHistoryDigests = map[string]bool{
	"db8edf334e7c14d2a7030f95fafe9b7e0d1884960c4c413328dbf95faf61ec12": true,
	"c94a75da1d3c36128cbfef040b30f091002b2c06e5ccca0cd2a3138df6a0d1a7": true,
}

// TestApprovedSurfaceHistoryStaysFrozen keeps approvals out of the shared history file. Every PR
// appending at its end conflicts with every other one and drops out of the merge queue (#5703).
func TestApprovedSurfaceHistoryStaysFrozen(t *testing.T) {
	contents, err := os.ReadFile("approved-surface-history.md")
	if err != nil {
		t.Fatalf("read approved-surface-history.md: %v", err)
	}
	digest := sha256.Sum256(contents)
	if got := hex.EncodeToString(digest[:]); !frozenHistoryDigests[got] {
		t.Fatalf("approved-surface-history.md changed (sha256 %s): it is frozen, so put the "+
			"evidence for a ledger change in the pull request and restore this file", got)
	}
}
