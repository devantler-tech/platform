package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

func observeHistory(ctx context.Context, side string, v historyConfig) (historySnapshot, error) {
	switch side {
	case "source":
		return readHistorySnapshot(ctx, "imap.mail.me.com:993", "imap.mail.me.com", v.username, v.sourcePassword, nil)
	case "destination":
		return readHistorySnapshot(ctx, "mail.devantler.tech:993", "mail.devantler.tech", historyDestination, v.destinationPassword, nil)
	default:
		return nil, errors.New("history-side")
	}
}

// readHistorySnapshot fails with one of the fixed step names in
// historyObservationSteps, so an operator can tell which step refused without
// any server reply, folder name or credential reaching the log.
func readHistorySnapshot(ctx context.Context, address, serverName, username, password string, roots *x509.CertPool) (historySnapshot, error) {
	fail := func(step string) (historySnapshot, error) { return nil, errors.New(step) }
	deadline := time.Now().Add(60 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fail("connect")
	}
	connection := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName, RootCAs: roots})
	defer func() { _ = connection.Close() }()
	if connection.SetDeadline(deadline) != nil {
		return fail("connect")
	}
	if connection.HandshakeContext(ctx) != nil {
		return fail("tls")
	}
	c, err := client.New(connection)
	if err != nil {
		return fail("greeting")
	}
	// Logout does not expunge. In particular, never send CLOSE after EXAMINE.
	defer func() { _ = c.Logout() }()
	if c.Login(username, password) != nil {
		return fail("login")
	}
	mailboxes := make(chan *imap.MailboxInfo, 1)
	listed := make(chan error, 1)
	go func() { listed <- c.List("", "*", mailboxes) }()
	var names []string
	bad, delimiter := false, false
	for box := range mailboxes {
		if box == nil {
			bad = true
			continue
		}
		noselect := false
		for _, attribute := range box.Attributes {
			if strings.EqualFold(attribute, imap.NoSelectAttr) {
				noselect = true
			}
		}
		if noselect {
			continue
		}
		// The copier uses exact names and this separator. Refuse a server whose
		// hierarchy would require an unreviewed mapping or a partial inventory.
		if box.Delimiter != "/" {
			delimiter = true
			continue
		}
		if box.Name == "" || len(box.Name) > 4096 || strings.ContainsAny(box.Name, "\r\n\x00") || len(names) >= 1000 {
			bad = true
			continue
		}
		names = append(names, box.Name)
	}
	if <-listed != nil {
		return fail("list")
	}
	if delimiter {
		return fail("delimiter")
	}
	if bad || len(names) == 0 {
		return fail("folders")
	}
	result := make(historySnapshot, len(names))
	for _, name := range names {
		if _, exists := result[name]; exists {
			return fail("folders")
		}
		status, err := c.Select(name, true)
		if err != nil {
			return fail("examine")
		}
		if !status.ReadOnly {
			return fail("read-only")
		}
		if status.UidNext == 0 || status.UidValidity == 0 {
			return fail("uids")
		}
		result[name] = historyFolder{messages: status.Messages, uidNext: status.UidNext, uidValidity: status.UidValidity}
	}
	if _, inbox := result["INBOX"]; !inbox {
		return fail("inbox")
	}
	return result, nil
}

// historyObservationSteps are the only step names a failed observation may
// report; anything else is reported without a step.
var historyObservationSteps = map[string]bool{
	"connect": true, "tls": true, "greeting": true, "login": true, "list": true,
	"delimiter": true, "folders": true, "examine": true, "read-only": true, "uids": true, "inbox": true,
}
