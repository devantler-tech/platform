package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitStopsAtItsLimitAndOnATerminalState(t *testing.T) {
	poll := readinessPoll
	readinessPoll = time.Millisecond
	t.Cleanup(func() { readinessPoll = poll })
	pending := func() error { return errors.New("not yet") }
	if err := waitReadyWithin(context.Background(), 10*time.Millisecond, pending); err == nil {
		t.Fatal("a state never reached was accepted")
	}
	checks := 0
	terminal := func() error { checks++; return errReadinessTerminal }
	if err := waitReadyWithin(context.Background(), time.Minute, terminal); !errors.Is(err, errReadinessTerminal) || checks != 1 {
		t.Fatalf("a terminal state was polled: %v after %d checks", err, checks)
	}
}

// The fio trials alone take at least twelve minutes, and the Pod's own deadline ends them,
// so the wait for its completion must outlast that deadline and the general readiness limit.
func TestBenchmarkWaitOutlastsItsPodDeadline(t *testing.T) {
	if benchmarkCompletionLimit <= benchmarkDeadlineSeconds*time.Second || benchmarkCompletionLimit <= 15*time.Minute {
		t.Fatal("the benchmark wait can end before its Pod does")
	}
	if benchmarkDeadlineSeconds < 18*40+120 {
		t.Fatal("the Pod deadline cannot hold eighteen 40-second trials and the preparation pass")
	}
}
