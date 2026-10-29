package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// The disposable native fixture uses example.test identities. Translate only
// authentication at the transport boundary; all production request encoding,
// response validation, content checks and cleanup run against the real server.
type fixtureIdentityTransport struct {
	base     http.RoundTripper
	password string
}

func (f fixtureIdentityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.SetBasicAuth("maintainer@example.test", f.password)
	return f.base.RoundTrip(cloned)
}
func TestNativeRecoveryMarkerLifecycle(t *testing.T) {
	if os.Getenv("EMAIL_RECOVERY_NATIVE_TEST") != "1" {
		t.Skip("requires the isolated native Email fixture")
	}
	data, err := os.ReadFile("/proof/inputs/maintainer-password")
	if err != nil || len(data) < 16 || len(data) > 4096 {
		t.Fatal("native fixture credential unavailable")
	}
	password := strings.TrimSpace(string(data))
	client, err := newMailboxClient("http://127.0.0.1:8080", password)
	if err != nil {
		t.Fatal("native fixture endpoint refused")
	}
	client.http.Transport = fixtureIdentityTransport{base: client.http.Transport, password: password}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = client.session(ctx); err != nil {
		t.Fatalf("native account session: %s", err)
	}
	nonce, err := randomRecoveryValue()
	if err != nil {
		t.Fatal("native nonce unavailable")
	}
	marker, err := client.createMarker(ctx, nonce)
	if err != nil {
		t.Fatalf("native marker import: %s", err)
	}
	if _, err = client.verifyMessage(ctx, marker.identity); err != nil {
		t.Fatalf("native marker read: %s", err)
	}
	if err = client.cleanupMarker(ctx, marker); err != nil {
		t.Fatalf("native marker cleanup: %s", err)
	}
	t.Log("EMAIL_RECOVERY_NATIVE_MARKER=PASS import=VERIFIED read=VERIFIED cleanup=VERIFIED")
}
