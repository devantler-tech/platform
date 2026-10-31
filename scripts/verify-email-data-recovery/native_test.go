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
		if marker != nil && marker.identity.email != "" {
			logNativeMessageShape(t, ctx, client, marker.identity)
		}
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

// Failure diagnostics reveal only comparisons and types from synthetic data.
// No message content, identifier or credential is written to CI output.
func logNativeMessageShape(t *testing.T, ctx context.Context, client *mailboxClient, expected messageIdentity) {
	t.Helper()
	result, err := client.call(ctx, "Email/get", object{"ids": []any{expected.email}, "properties": []any{"id", "blobId", "messageId", "mailboxIds", "textBody", "bodyValues"}, "fetchTextBodyValues": true})
	if err != nil {
		return
	}
	messages := items(result, "list")
	if len(messages) != 1 {
		t.Logf("NATIVE_MESSAGE_SHAPE count=%d state_present=%t", len(messages), text(result, "state") != "")
		return
	}
	m := messages[0]
	ids, _ := m["messageId"].([]any)
	idMatches := len(ids) == 1 && ids[0] == expected.messageID
	parts := items(m, "textBody")
	if len(parts) != 1 {
		t.Logf("NATIVE_MESSAGE_SHAPE text_parts=%d", len(parts))
		return
	}
	body := child(child(m, "bodyValues"), text(parts[0], "partId"))
	t.Logf("NATIVE_MESSAGE_SHAPE id_match=%t blob_match=%t message_id_match=%t mailbox_match=%t body_match=%t truncated_type=%T encoding_type=%T state_present=%t", text(m, "id") == expected.email, text(m, "blobId") == expected.blob, idMatches, child(m, "mailboxIds")[expected.mailbox] == true, strings.TrimRight(text(body, "value"), "\r\n") == expected.body, body["isTruncated"], body["isEncodingProblem"], text(result, "state") != "")
}
