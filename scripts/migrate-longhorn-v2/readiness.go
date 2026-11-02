package main

import (
	"context"
	"errors"
	"time"
)

// readinessPoll spaces waitReady's checks.
var readinessPoll = 5 * time.Second

// errReadinessTerminal marks a state further polling cannot change.
var errReadinessTerminal = errors.New("required readiness can no longer be reached")

func waitReady(ctx context.Context, check func(context.Context) error) error {
	return waitReadyWithin(ctx, 15*time.Minute, check)
}

func waitReadyWithin(ctx context.Context, limit time.Duration, check func(context.Context) error) error {
	deadline, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ends, _ := deadline.Deadline()
	hasExpired := func() bool { return deadline.Err() != nil || !time.Now().Before(ends) }
	expired := func() error {
		cause := deadline.Err()
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		return errors.Join(errors.New("required readiness was not proven within the bounded recovery window"), cause)
	}
	for {
		if hasExpired() {
			return expired()
		}
		err := check(deadline)
		if hasExpired() {
			return expired()
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errReadinessTerminal) {
			return err
		}
		timer := time.NewTimer(min(readinessPoll, time.Until(ends)))
		select {
		case <-deadline.Done():
			timer.Stop()
			return expired()
		case <-timer.C:
		}
	}
}

func annotationPatch(n object, annotationValue string) []object {
	changes := []object{}
	if value(n, "metadata", "annotations") == nil {
		changes = append(changes, object{"op": "add", "path": "/metadata/annotations", "value": object{}})
	}
	return append(changes, object{"op": "add", "path": "/metadata/annotations/storage.platform.devantler.tech~1longhorn-v2-preparation", "value": annotationValue})
}
