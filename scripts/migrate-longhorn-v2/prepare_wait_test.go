package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestWaitReadyStopsOnATerminalStateAndRetriesOthers(t *testing.T) {
	poll := readinessPoll
	readinessPoll = time.Millisecond
	t.Cleanup(func() { readinessPoll = poll })

	checks := 0
	err := waitReady(context.Background(), func() error {
		checks++
		return fmt.Errorf("benchmark consumer failed: %w", errReadinessTerminal)
	})
	if !errors.Is(err, errReadinessTerminal) || checks != 1 {
		t.Fatalf("terminal state retried: checks=%d err=%v", checks, err)
	}

	checks = 0
	if err = waitReady(context.Background(), func() error {
		checks++
		if checks < 3 {
			return errors.New("not yet")
		}
		return nil
	}); err != nil || checks != 3 {
		t.Fatalf("recoverable state not retried: checks=%d err=%v", checks, err)
	}
}

func TestReviewedWorkflowPathOnlyNamesTheProcedureOnMain(t *testing.T) {
	const workflow = ".github/workflows/prepare-longhorn-v2-node.yaml"
	for _, path := range []string{workflow, workflow + "@main", workflow + "@refs/heads/main"} {
		if !reviewedWorkflowPath(path) {
			t.Fatalf("%s refused", path)
		}
	}
	for _, path := range []string{"", workflow + "@feature", workflow + "@refs/heads/feature", workflow + "@refs/tags/main", ".github/workflows/other.yaml@main"} {
		if reviewedWorkflowPath(path) {
			t.Fatalf("%q accepted", path)
		}
	}
}
