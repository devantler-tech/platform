package main

import (
	"context"
	"errors"
	"time"
)

// readinessPoll spaces waitReady's checks.
var readinessPoll = 5 * time.Second

func waitReady(ctx context.Context, check func() error) error {
	deadline, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	for {
		err := check()
		if err == nil {
			return nil
		}
		timer := time.NewTimer(readinessPoll)
		select {
		case <-deadline.Done():
			timer.Stop()
			return errors.New("required readiness was not proven within the bounded recovery window")
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
