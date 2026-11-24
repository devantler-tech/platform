package main

import (
	"errors"
	"strings"
	"testing"
)

func TestNativeRestoreStopsWhenVeleroHasFinishedUnsuccessfully(t *testing.T) {
	for _, status := range []struct {
		phase    string
		terminal bool
	}{
		{"", false},
		{"New", false},
		{"InProgress", false},
		{"WaitingForPluginOperations", false},
		{"WaitingForPluginOperationsPartiallyFailed", false},
		{"Finalizing", false},
		{"Completed", false},
		{"FailedValidation", true},
		{"PartiallyFailed", true},
		{"Failed", true},
	} {
		t.Run(status.phase, func(t *testing.T) {
			restore := object{"status": object{"phase": status.phase, "failureReason": "private diagnostic"}}
			err := nativeRestoreTerminalFailure(restore)
			if status.terminal {
				if !errors.Is(err, errReadinessTerminal) || strings.Contains(err.Error(), "private diagnostic") {
					t.Fatalf("expected fixed terminal diagnosis, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected terminal status: %v", err)
			}
		})
	}
}
