// Command s3-access-denied-stub answers every request the way R2 answers a
// bucket-scoped token that reaches for another bucket: 403 with the S3 error
// code AccessDenied. The Wedding backup denial test runs the pinned mc client
// against it, so the proof's refusal classifier is checked on that client's real
// output rather than on hand-written fixtures.
//
// Usage: s3-access-denied-stub <listen-address> <ready-file> <request-log>
//
// The ready file is created once the listener is open, so a caller can wait for
// it instead of guessing how long startup takes. Each request is appended to the
// request log as "<method> <path>", so a caller can tell which accesses actually
// reached the destination.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const accessDenied = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: s3-access-denied-stub <listen-address> <ready-file> <request-log>")
		os.Exit(2)
	}

	requests, err := os.OpenFile(os.Args[3], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var mu sync.Mutex
	refuse := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_, logErr := fmt.Fprintf(requests, "%s %s\n", r.Method, r.URL.Path)
		mu.Unlock()
		if logErr != nil {
			fmt.Fprintln(os.Stderr, logErr)
			os.Exit(1)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(accessDenied))
	}

	listener, err := net.Listen("tcp", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := os.WriteFile(os.Args[2], nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	server := &http.Server{
		Handler:           http.HandlerFunc(refuse),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintln(os.Stderr, server.Serve(listener))
	os.Exit(1)
}
