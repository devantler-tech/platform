package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"
)

type historyLoginBackend struct {
	backend.Backend
	logins atomic.Int32
}

func (b *historyLoginBackend) Login(info *imap.ConnInfo, username, password string) (backend.User, error) {
	b.logins.Add(1)
	return b.Backend.Login(info, username, password)
}

type historyWireLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (w *historyWireLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.Write(p)
}

func historyTestServer(t *testing.T) (string, *x509.CertPool, *historyLoginBackend, *historyWireLog) {
	t.Helper()
	certificate := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	configuration := certificate.TLS.Clone()
	pool := x509.NewCertPool()
	pool.AddCert(certificate.Certificate())
	certificate.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", configuration)
	if err != nil {
		t.Fatal(err)
	}
	b := &historyLoginBackend{Backend: memory.New()}
	u, err := b.Backend.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = u.CreateMailbox("Empty"); err != nil {
		t.Fatal(err)
	}
	s := server.New(b)
	s.ErrorLog = log.New(io.Discard, "", 0)
	wire := &historyWireLog{}
	s.Debug = wire
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	t.Cleanup(func() { _ = s.Close(); _ = listener.Close(); <-done })
	return listener.Addr().String(), pool, b, wire
}

func TestHistorySnapshotUsesVerifiedTLSAndOnlyReadOnlyMailboxCommands(t *testing.T) {
	address, roots, b, wire := historyTestServer(t)
	got, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", roots)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["INBOX"] != (historyFolder{messages: 1, uidNext: 7, uidValidity: 1}) || got["Empty"] != (historyFolder{messages: 0, uidNext: 1, uidValidity: 1}) {
		t.Fatalf("wrong independent folder inventory: %#v", got)
	}
	if b.logins.Load() != 1 {
		t.Fatal("the fixture was not authenticated")
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	commands := strings.ToUpper(wire.text.String())
	if len(regexp.MustCompile(`(?m)^[^ \r\n]+ EXAMINE `).FindAllString(commands, -1)) != 2 {
		t.Fatal("folder reads did not use EXAMINE")
	}
	for _, forbidden := range []string{" SELECT ", " CLOSE", " STORE ", " APPEND ", " EXPUNGE", " DELETE ", " SUBSCRIBE ", " FETCH "} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("observer sent a write or body-read command: %s", forbidden)
		}
	}
}

func TestHistorySnapshotRefusesWrongCAOrHostnameBeforeAuthentication(t *testing.T) {
	for _, wrong := range []string{"ca", "hostname"} {
		t.Run(wrong, func(t *testing.T) {
			address, roots, b, _ := historyTestServer(t)
			name := "example.com"
			if wrong == "ca" {
				roots = x509.NewCertPool()
			} else {
				name = "wrong.example.invalid"
			}
			if _, err := readHistorySnapshot(context.Background(), address, name, "username", "password", roots); err == nil {
				t.Fatal("unverified TLS accepted")
			}
			if b.logins.Load() != 0 {
				t.Fatal("credentials were sent before TLS authentication")
			}
		})
	}
}

func TestHistorySnapshotHonorsDeadlineOnSilentServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			<-done
		}
	}()
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := readHistorySnapshot(ctx, listener.Addr().String(), "example.com", "username", "password", nil); err == nil {
		t.Fatal("silent peer accepted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("snapshot ignored its deadline")
	}
}
