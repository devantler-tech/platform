package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCheckEndpointIsAPlainClusterOrigin refuses endpoints that would send the
// administrator credential somewhere other than the management listener.
func TestCheckEndpointIsAPlainClusterOrigin(t *testing.T) {
	if !validCheckEndpoint("http://private-mail-management.private-mail.svc:8080") {
		t.Fatal("cluster management endpoint refused")
	}
	for _, raw := range []string{
		"", "private-mail:8080", "https://private-mail:8080", "http://private-mail", "http://:8080",
		"http://private-mail:8080/", "http://private-mail:8080/jmap", "http://user@private-mail:8080",
		"http://private-mail:8080?x=1", "http://private-mail:8080?", "http://private-mail:8080#x",
	} {
		if validCheckEndpoint(raw) {
			t.Fatalf("endpoint %q accepted", raw)
		}
	}
}

// TestCheckNeverReportsAMatchWithoutReadingTheServer keeps a refused or
// unreachable server from looking like a clean result.
func TestCheckNeverReportsAMatchWithoutReadingTheServer(t *testing.T) {
	input, _ := relayInputs(t)
	var mu sync.Mutex
	users := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		mu.Lock()
		users[user] = true
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	for name, args := range map[string][]string{
		"refused":         {"--input-dir", input, "--url", server.URL},
		"unreachable":     {"--input-dir", input, "--url", "http://127.0.0.1:9"},
		"no endpoint":     {"--input-dir", input},
		"relative inputs": {"--input-dir", "inputs", "--url", server.URL},
		"extra argument":  {"--input-dir", input, "--url", server.URL, "apply"},
	} {
		var log bytes.Buffer
		if status := command(append([]string{"check"}, args...), &log); status != checkUnknown {
			t.Fatalf("%s: status %d, want %d", name, status, checkUnknown)
		}
		if strings.Contains(log.String(), "matches") || strings.Contains(log.String(), strings.Repeat("A", 40)) {
			t.Fatalf("%s: reported a match or echoed a credential", name)
		}
	}
	// The serving process has no recovery administrator; the check signs in as
	// the managed administrator's address.
	if len(users) != 1 || !users["operator@example.test"] {
		t.Fatalf("check signed in as %v", users)
	}
}
