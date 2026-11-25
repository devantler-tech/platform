package main

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// A failed observation names its step, so an operator can tell a refused
// connection from a refused login without any reply or value in the log.
func TestHistorySnapshotNamesTheFailedStep(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", nil); err == nil || err.Error() != "connect" {
			t.Fatalf("refused connection reported as %v", err)
		}
	})
	t.Run("tls untrusted", func(t *testing.T) {
		address, _, b, _ := historyTestServer(t)
		if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", x509.NewCertPool()); err == nil || err.Error() != "tls" {
			t.Fatalf("untrusted certificate reported as %v", err)
		}
		if b.logins.Load() != 0 {
			t.Fatal("credentials were sent before TLS authentication")
		}
	})
	t.Run("tls wrong name", func(t *testing.T) {
		address, roots, _, _ := historyTestServer(t)
		if _, err := readHistorySnapshot(context.Background(), address, "wrong.example.invalid", "username", "password", roots); err == nil || err.Error() != "tls" {
			t.Fatalf("wrong host name reported as %v", err)
		}
	})
	t.Run("tls silent peer", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		done := make(chan struct{})
		defer close(done)
		go func() {
			if conn, err := listener.Accept(); err == nil {
				defer func() { _ = conn.Close() }()
				<-done
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := readHistorySnapshot(ctx, listener.Addr().String(), "example.com", "username", "password", nil); err == nil || err.Error() != "tls" {
			t.Fatalf("silent peer reported as %v", err)
		}
	})
	t.Run("login", func(t *testing.T) {
		address, roots, b, _ := historyTestServer(t)
		if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "wrong-password", roots); err == nil || err.Error() != "login" {
			t.Fatalf("refused login reported as %v", err)
		}
		if b.logins.Load() != 1 {
			t.Fatal("the login was not attempted exactly once")
		}
	})
}

// The receipt carries only a fixed vocabulary: a known stage, or a side with a
// known step. Any other error text is never echoed.
func TestHistoryReceiptStageVocabulary(t *testing.T) {
	for err, want := range map[string]string{
		"source":                      "source",
		"source-login":                "source-login",
		"source-tls":                  "source-tls",
		"destination-delimiter":       "destination-delimiter",
		"destination-uids":            "destination-uids",
		"source-change":               "source-change",
		"reconciliation":              "reconciliation",
		"source-private-reply-canary": "unknown",
		"login":                       "unknown",
		"private-server-error-canary": "unknown",
		"elsewhere-login":             "unknown",
	} {
		if got := historyStage(errors.New(err)); got != want {
			t.Fatalf("stage for %q = %q, want %q", err, got, want)
		}
	}
}

// An observation's step reaches the run's error only when it is on the fixed
// list; an unexpected error keeps the bare side.
func TestHistoryRunNamesTheObservationStep(t *testing.T) {
	v, err := loadHistoryInputs(historyInputs(t))
	if err != nil {
		t.Fatal(err)
	}
	execute := func(context.Context, []string, io.Writer) error {
		t.Fatal("nothing may be copied after a failed observation")
		return nil
	}
	for _, tc := range []struct{ side, step, want string }{
		{"source", "login", "source-login"},
		{"source", "private-server-error-canary", "source"},
		{"destination", "tls", "destination-tls"},
		{"destination", "private-server-error-canary", "destination"},
	} {
		observe := func(_ context.Context, side string, _ historyConfig) (historySnapshot, error) {
			if side == tc.side {
				return nil, errors.New(tc.step)
			}
			return historySnapshot{"INBOX": {messages: 1, uidNext: 2, uidValidity: 1}}, nil
		}
		if err := importHistory(context.Background(), v, observe, execute, io.Discard); err == nil || err.Error() != tc.want {
			t.Fatalf("%s %s reported as %v, want %s", tc.side, tc.step, err, tc.want)
		}
	}
}
