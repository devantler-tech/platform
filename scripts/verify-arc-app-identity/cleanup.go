package main

import (
	"context"
	"time"
)

// Cleanup shares the first cancellation's budget across all stages. During
// ordinary completion each stage retains its own timeout; cancellation also
// interrupts cleanup that has already started. Six seconds leaves margin
// before GitHub Actions escalates its initial interrupt after 7.5 seconds.
func cleanupContext(proof context.Context) (context.Context, func()) {
	cleanup, cancel := context.WithCancel(context.WithoutCancel(proof))
	finished := make(chan struct{})
	stopWatching := context.AfterFunc(proof, func() {
		defer close(finished)
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-cleanup.Done():
		}
	})
	return cleanup, func() {
		cancel()
		if !stopWatching() {
			<-finished
		}
	}
}
