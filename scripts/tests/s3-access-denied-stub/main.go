// Command s3-access-denied-stub answers every request the way R2 answers a
// bucket-scoped token that reaches for another bucket: 403 with the S3 error
// code AccessDenied. The Wedding backup denial test runs the pinned mc client
// against it, so the proof's refusal classifier is checked on that client's real
// output rather than on hand-written fixtures.
//
// Usage: s3-access-denied-stub <listen-address> <ready-file>
//
// The ready file is created once the listener is open, so a caller can wait for
// it instead of guessing how long startup takes.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

const accessDenied = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`

func refuse(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(accessDenied))
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: s3-access-denied-stub <listen-address> <ready-file>")
		os.Exit(2)
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
