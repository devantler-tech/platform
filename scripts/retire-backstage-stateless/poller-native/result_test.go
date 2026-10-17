package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestWaitMarkerRequiresSuccessOrOnlyDeadlineErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "WAITED"},
		{"deadline", fmt.Errorf("termination: %w", context.DeadlineExceeded), "WAITED_TIMEOUT"},
		{"joined deadline", fmt.Errorf("native uninstall: %w", errors.Join(context.DeadlineExceeded)), "WAITED_TIMEOUT"},
		{"storage failure", errors.New("storage unavailable"), "HOLD_UNINSTALL"},
		{"mixed timeout and storage", errors.Join(context.DeadlineExceeded, errors.New("storage unavailable")), "HOLD_UNINSTALL"},
		{"cancelled", context.Canceled, "HOLD_UNINSTALL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultMarker(tc.err); got != tc.want {
				t.Fatalf("marker=%s want=%s", got, tc.want)
			}
		})
	}
}
