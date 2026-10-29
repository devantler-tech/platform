package main

import (
	"testing"
	"time"
)

func TestCanonicalRecoveryUsesRecentIndependentBackupsAfterCustodyChanges(t *testing.T) {
	previous := credentialPaths
	t.Cleanup(func() { credentialPaths = previous })
	_, credentialPaths, _ = credentialMode("collect-email")
	now := time.Now().UTC()
	start, end, err := freshRecoveryWindow(nil, now)
	if err != nil || !start.Equal(now.Add(-24*time.Hour)) || !end.Equal(now.Add(5*time.Minute)) {
		t.Fatal("canonical recovery is still bound to the obsolete one-time copy snapshot")
	}
	if _, _, err := freshRecoveryWindow([]byte("undeclared-window"), now); err == nil {
		t.Fatal("canonical recovery accepted an externally supplied evidence window")
	}
	if _, _, err := freshRecoveryWindow(nil, time.Time{}); err == nil {
		t.Fatal("canonical recovery accepted an absent current clock")
	}
	_, credentialPaths, _ = credentialMode("collect")
	if _, _, err := freshRecoveryWindow(nil, now); err == nil {
		t.Fatal("legacy copy recovery lost its specific completed-job prerequisite")
	}
}
