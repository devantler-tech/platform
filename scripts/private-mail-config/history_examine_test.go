package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// scriptedIMAPServer answers a fixed conversation over TLS: one INBOX whose
// EXAMINE reply sends the given untagged lines and ends with the given tagged
// response code ("" for none). It records every command verb it receives.
func scriptedIMAPServer(t *testing.T, examineCode string, untagged string) (string, *x509.CertPool, func() []string) {
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
	var mu sync.Mutex
	var verbs []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		write("* OK [CAPABILITY IMAP4rev1] ready\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return
			}
			tag, verb := fields[0], strings.ToUpper(fields[1])
			mu.Lock()
			verbs = append(verbs, verb)
			mu.Unlock()
			switch verb {
			case "CAPABILITY":
				write("* CAPABILITY IMAP4rev1\r\n" + tag + " OK done\r\n")
			case "LOGIN":
				write(tag + " OK signed in\r\n")
			case "LIST":
				write("* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n" + tag + " OK listed\r\n")
			case "EXAMINE":
				reply := tag + " OK EXAMINE completed\r\n"
				if examineCode != "" {
					reply = tag + " OK [" + examineCode + "] EXAMINE completed\r\n"
				}
				write(untagged + reply)
			case "LOGOUT":
				write("* BYE\r\n" + tag + " OK bye\r\n")
				return
			default:
				write(tag + " BAD unexpected\r\n")
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String(), pool, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), verbs...)
	}
}

// examineStatus is the untagged part of a complete EXAMINE reply.
const examineStatus = "* 2 EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 5] next\r\n"

// assertOnlyReadCommands requires that the observer sent nothing but the
// commands it needs to read folder status: an allowlist, so a new verb (a UID
// command, MOVE, DELETE and so on) fails instead of slipping past a blocklist.
// On the source this is the guarantee, since its reply code is not checked.
func assertOnlyReadCommands(t *testing.T, verbs []string) {
	t.Helper()
	for _, verb := range verbs {
		switch verb {
		case "CAPABILITY", "LOGIN", "LIST", "EXAMINE", "LOGOUT":
		default:
			t.Fatalf("observer sent %s", verb)
		}
	}
}

// The source, iCloud, does not confirm EXAMINE with [READ-ONLY], so its
// observer accepts a reply without it. The destination's observer requires it.
func TestHistorySnapshotAcceptsAnExamineWithoutTheReadOnlyCodeOnlyFromTheSource(t *testing.T) {
	address, roots, _ := scriptedIMAPServer(t, "", examineStatus)
	if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", roots); err == nil || err.Error() != "read-only" {
		t.Fatalf("the destination observer accepted an EXAMINE without [READ-ONLY]: %v", err)
	}
	address, roots, _ = scriptedIMAPServer(t, "READ-ONLY", examineStatus)
	if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", roots); err != nil {
		t.Fatalf("the destination observer refused a confirmed EXAMINE as %v", err)
	}
	for _, code := range []string{"", "READ-ONLY"} {
		t.Run("code="+code, func(t *testing.T) {
			address, roots, verbs := scriptedIMAPServer(t, code, examineStatus)
			got, err := readHistorySnapshotWith(context.Background(), address, "example.com", "username", "password", roots, false)
			if err != nil {
				t.Fatalf("EXAMINE reply %q refused as %v", code, err)
			}
			if got["INBOX"] != (historyFolder{messages: 2, uidNext: 5, uidValidity: 9}) {
				t.Fatalf("wrong inventory %#v", got)
			}
			assertOnlyReadCommands(t, verbs())
		})
	}
}

// The destination must never claim [READ-WRITE] for an EXAMINE. The source,
// iCloud, does, so its observer reads the folder anyway and, as always, sends
// only read commands.
func TestHistorySnapshotReadWriteClaimRefusedOnlyWhereConfirmationIsRequired(t *testing.T) {
	for name, server := range map[string]struct{ code, untagged string }{
		"tagged":   {"READ-WRITE", examineStatus},
		"untagged": {"", examineStatus + "* OK [READ-WRITE] opened\r\n"},
		// A confirmed open does not excuse an untagged read-write claim.
		"untagged with a tagged confirmation": {"READ-ONLY", examineStatus + "* OK [READ-WRITE] opened\r\n"},
	} {
		t.Run(name+"/destination", func(t *testing.T) {
			address, roots, _ := scriptedIMAPServer(t, server.code, server.untagged)
			if _, err := readHistorySnapshot(context.Background(), address, "example.com", "username", "password", roots); err == nil || err.Error() != "read-only" {
				t.Fatalf("the destination observer accepted an EXAMINE claiming READ-WRITE: %v", err)
			}
		})
		t.Run(name+"/source", func(t *testing.T) {
			address, roots, verbs := scriptedIMAPServer(t, server.code, server.untagged)
			got, err := readHistorySnapshotWith(context.Background(), address, "example.com", "username", "password", roots, false)
			if err != nil {
				t.Fatalf("the source observer refused iCloud's reply code as %v", err)
			}
			if got["INBOX"] != (historyFolder{messages: 2, uidNext: 5, uidValidity: 9}) {
				t.Fatalf("wrong inventory %#v", got)
			}
			assertOnlyReadCommands(t, verbs())
		})
	}
}

// A folder whose message count is missing must not be read as empty: the copy
// would then be judged against zero messages and could never be shown short.
func TestHistorySnapshotRefusesAnExamineWithoutAMessageCount(t *testing.T) {
	for name, untagged := range map[string]string{
		"missing": "* 0 RECENT\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 5] next\r\n",
		// go-imap reads `* x EXISTS` as a response named X, not EXISTS.
		"not a count": "* x EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 5] next\r\n",
		// It reads `* EXISTS x` as EXISTS with a field that is not a number.
		"unparseable": "* EXISTS x\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 9] valid\r\n* OK [UIDNEXT 5] next\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			address, roots, _ := scriptedIMAPServer(t, "READ-ONLY", untagged)
			if _, err := readHistorySnapshotWith(context.Background(), address, "example.com", "username", "password", roots, false); err == nil || err.Error() != "examine" {
				t.Fatalf("an EXAMINE without a message count was reported as %v", err)
			}
		})
	}
}
