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
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
)

// errNoMessageCount reports an EXAMINE reply without a parseable `* N EXISTS`.
// RFC 3501 requires that line, and a folder without it would otherwise count as
// empty, so the copy would be judged against zero messages.
var errNoMessageCount = errors.New("examine reply has no message count")

// examineHandler collects an EXAMINE reply. The client tracks the message count
// (`* N EXISTS`) only for a mailbox it selected itself, so it is read here, and
// so is an untagged [READ-WRITE] code, which the client would otherwise drop.
type examineHandler struct {
	selected  *responses.Select
	mailbox   *imap.MailboxStatus
	counted   bool
	readWrite bool
}

func (h *examineHandler) Handle(resp imap.Resp) error {
	if status, ok := resp.(*imap.StatusResp); ok && status.Tag == "*" && status.Code == imap.CodeReadWrite {
		h.readWrite = true
	}
	if err := h.selected.Handle(resp); err != responses.ErrUnhandled {
		return err
	}
	if data, ok := resp.(*imap.DataResp); ok {
		if name, fields, ok := imap.ParseNamedResp(data); ok && name == "EXISTS" && len(fields) > 0 {
			if messages, err := imap.ParseNumber(fields[0]); err == nil {
				h.mailbox.Messages = messages
				h.counted = true
				return nil
			}
		}
	}
	return responses.ErrUnhandled
}

// examineFolder opens a folder with EXAMINE and returns its status, and whether
// the server claimed the open was read-write, in the tagged reply's code (which
// the client's Select reduces to a flag) or in an untagged one.
//
// client.Execute bypasses the client's state tracking, so the client never
// considers a mailbox selected: no selected-state call (Fetch, Check, Close)
// may follow, and none must, because the observer only reads folder status.
func examineFolder(c *client.Client, name string) (*imap.MailboxStatus, bool, error) {
	mailbox := &imap.MailboxStatus{Name: name, Items: make(map[imap.StatusItem]interface{})}
	handler := &examineHandler{selected: &responses.Select{Mailbox: mailbox}, mailbox: mailbox}
	status, err := c.Execute(&commands.Select{Mailbox: name, ReadOnly: true}, handler)
	if err != nil {
		return nil, false, err
	}
	if err := status.Err(); err != nil {
		return nil, false, err
	}
	if !handler.counted {
		return nil, false, errNoMessageCount
	}
	return mailbox, handler.readWrite || status.Code == imap.CodeReadWrite, nil
}

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
		status, readWrite, err := examineFolder(c, name)
		if err != nil {
			return fail("examine")
		}
		// EXAMINE never permits changes, whatever the reply says. RFC 3501
		// asks servers to confirm it with [READ-ONLY]; iCloud omits the code.
		// A server that explicitly claims [READ-WRITE] for an EXAMINE is
		// still refused. The same rule applies to the source and destination.
		if readWrite {
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
