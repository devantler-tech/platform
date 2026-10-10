package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestRejectedWriteReportsOnlyABoundedReason keeps private command failures
// diagnosable without turning their arbitrary stderr into public evidence.
func TestRejectedWriteReportsOnlyABoundedReason(t *testing.T) {
	const secret = "fixture-private-token-and-address"
	exitError := func(stderr string) error {
		return &exec.ExitError{Stderr: []byte(stderr)}
	}
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{"forbidden", "SERVER_FORBIDDEN", exitError("Error from server (Forbidden): " + secret)},
		{"conflict", "SERVER_CONFLICT", exitError("Warning: ignored warning\nError from server (Conflict): " + secret)},
		{"invalid", "SERVER_INVALID", exitError("Error from server (Invalid): " + secret)},
		// kubectl formats the generic HTTP 422 from a rejected JSON-patch test
		// specially. This literal was reproduced with the installed v1.36.2.
		{"native invalid", "SERVER_INVALID", exitError("The request is invalid: the server rejected our request due to an error in our request\n")},
		{"native invalid with warning", "SERVER_INVALID", exitError("Warning: " + secret + "\nThe request is invalid: the server rejected our request due to an error in our request\n")},
		{"native invalid lookalike", "UNKNOWN", exitError(secret + ": The request is invalid: the server rejected our request due to an error in our request")},
		{"native invalid extended", "UNKNOWN", exitError("The request is invalid: the server rejected our request due to an error in our request " + secret)},
		{"native invalid ambiguous", "UNKNOWN", exitError("The request is invalid: the server rejected our request due to an error in our request\nError from server (Forbidden): " + secret)},
		{"native invalid oversized", "UNKNOWN", exitError("The request is invalid: the server rejected our request due to an error in our request\n" + strings.Repeat(secret, 1024))},
		{"bad request", "SERVER_BAD_REQUEST", exitError("Error from server (BadRequest): " + secret)},
		{"unauthorized", "SERVER_UNAUTHORIZED", exitError("Error from server (Unauthorized): " + secret)},
		{"internal", "SERVER_INTERNAL", exitError("Error from server (InternalError): " + secret)},
		{"unavailable", "SERVER_UNAVAILABLE", exitError("Error from server (ServiceUnavailable): " + secret)},
		{"server timeout", "SERVER_TIMEOUT", exitError("Error from server (Timeout): " + secret)},
		{"deadline", "CONTEXT_DEADLINE", context.DeadlineExceeded},
		{"cancelled", "CONTEXT_CANCELLED", context.Canceled},
		{"unknown server reason", "UNKNOWN", exitError("Error from server (" + secret + "): refused")},
		{"transport", "UNKNOWN", exitError("Unable to connect to server: " + secret)},
		{"lookalike", "UNKNOWN", exitError(secret + ": Error from server (Forbidden): refused")},
		{"arbitrary error", "UNKNOWN", errors.New(secret)},
		{"ambiguous", "UNKNOWN", exitError("Error from server (Forbidden): " + secret + "\nError from server (Conflict): refused")},
		{"oversized", "UNKNOWN", exitError("Error from server (Forbidden): " + strings.Repeat(secret, 1024))},
		{"empty", "UNKNOWN", exitError("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := client{command: func(context.Context, []string, []byte) ([]byte, error) {
				calls++
				return nil, tc.err
			}}
			err := c.write(context.Background(), []string{"patch", "cluster.postgresql.cnpg.io"}, []object{})
			want := "conditional patch failed (reason=" + tc.want + "); no retry or cleanup write"
			if err == nil || err.Error() != want || calls != 1 {
				t.Fatalf("reason=%s calls=%d; expected one stopped write", tc.want, calls)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("private command error escaped the boundary")
			}
		})
	}
}

// TestCancelledCommandRetainsTheContextReason models exec returning only a
// killed-process error. The caller still knows its own deadline was exceeded.
func TestCancelledCommandRetainsTheContextReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := client{command: func(context.Context, []string, []byte) ([]byte, error) {
		return nil, &exec.ExitError{Stderr: []byte("fixture-private-token")}
	}}
	err := c.write(ctx, []string{"delete"}, object{})
	if err == nil || err.Error() != "conditional delete failed (reason=CONTEXT_CANCELLED); no retry or cleanup write" {
		t.Fatal("cancelled command lost its bounded context reason")
	}
}
