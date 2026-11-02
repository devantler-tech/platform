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
	pending := func(context.Context) error { return errors.New("not yet") }
	if err := waitReadyWithin(context.Background(), 10*time.Millisecond, pending); err == nil {
		t.Fatal("a state never reached was accepted")
	}
	checks := 0
	terminal := func(context.Context) error { checks++; return errReadinessTerminal }
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

func TestExpiredReadinessCannotStartACallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	checks := 0
	if err := waitReadyWithin(ctx, time.Minute, func(context.Context) error { checks++; return nil }); err == nil || checks != 0 {
		t.Fatal("expired recovery authorization started or accepted a callback")
	}
}

func TestReadinessCannotAcceptSuccessAfterItsDeadline(t *testing.T) {
	if err := waitReadyWithin(context.Background(), time.Millisecond, func(context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	}); err == nil {
		t.Fatal("late callback success escaped the readiness deadline")
	}
}

func TestReadinessPassesItsDeadlineIntoCallbacks(t *testing.T) {
	outer, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	latest := time.Now().Add(11 * time.Second)
	err := waitReadyWithin(outer, 10*time.Second, func(bounded context.Context) error {
		deadline, ok := bounded.Deadline()
		if !ok || deadline.After(latest) || bounded == outer {
			t.Fatal("callback retained the unbounded outer authorization")
		}
		return nil
	})
	if err != nil || outer.Err() != nil {
		t.Fatal("bounded callback changed its parent context")
	}
}
