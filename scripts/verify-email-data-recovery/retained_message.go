package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
)

var retainedDiagnosticName = regexp.MustCompile(`^Email recovery verification ([0-9a-f]{64})$`)

const retainedMailboxLimit = 256

type retainedMailboxTransport struct {
	base     http.RoundTripper
	endpoint string
	account  *string
}

// This capability guards the actual HTTP boundary, including calls through
// existing mailbox helpers that can otherwise import or destroy messages.
func (t *retainedMailboxTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	refuse := func() (*http.Response, error) { return nil, errors.New("retained-read-scope") }
	if r.URL == nil || t.base == nil {
		return refuse()
	}
	if r.Method == http.MethodGet && r.URL.String() == t.endpoint+"/jmap/session" && r.ContentLength == 0 && (r.Body == nil || r.Body == http.NoBody) {
		return t.base.RoundTrip(r)
	}
	if r.Method != http.MethodPost || r.URL.String() != t.endpoint+"/jmap" || r.Header.Get("Content-Type") != "application/json" || r.Body == nil || t.account == nil {
		return refuse()
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	_ = r.Body.Close()
	if err != nil || len(data) > 64<<10 || !retainedJMAPRead(data, *t.account) {
		return refuse()
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return t.base.RoundTrip(r)
}

func retainedMessageProperties() []any {
	return []any{"id", "blobId", "messageId", "mailboxIds", "textBody", "bodyValues"}
}

func retainedJMAPRead(data []byte, account string) bool {
	var envelope object
	if !mailboxIDPattern.MatchString(account) || json.Unmarshal(data, &envelope) != nil || len(envelope) != 2 || !reflect.DeepEqual(envelope["using"], []any{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}) {
		return false
	}
	calls, ok := envelope["methodCalls"].([]any)
	if !ok || len(calls) != 1 {
		return false
	}
	tuple, ok := calls[0].([]any)
	if !ok || len(tuple) != 3 || tuple[2] != "proof" {
		return false
	}
	args, ok := tuple[1].(map[string]any)
	if !ok || text(args, "accountId") != account {
		return false
	}
	switch tuple[0] {
	case "Mailbox/get":
		return len(args) == 2 && reflect.DeepEqual(args["properties"], []any{"id", "name", "isSubscribed"})
	case "Email/query":
		filter := child(args, "filter")
		return len(args) == 5 && len(filter) == 1 && mailboxIDPattern.MatchString(text(filter, "inMailbox")) && args["calculateTotal"] == true && args["position"] == float64(0) && args["limit"] == float64(2)
	case "Email/get":
		ids, ok := args["ids"].([]any)
		id := ""
		if ok && len(ids) == 1 {
			id, _ = ids[0].(string)
		}
		return len(args) == 4 && mailboxIDPattern.MatchString(id) && args["fetchTextBodyValues"] == true && reflect.DeepEqual(args["properties"], retainedMessageProperties())
	}
	return false
}

func newRetainedMailboxClient(endpoint, password string) (*mailboxClient, error) {
	c, err := newMailboxClient(endpoint, password)
	if err != nil {
		return nil, errors.New("retained-endpoint")
	}
	c.http.Transport = &retainedMailboxTransport{base: c.http.Transport, endpoint: c.endpoint, account: &c.account}
	return c, nil
}

// Matching retained content is current readback evidence only. The old random
// marker was not durably bound to its run, so this is not fresh-trial acceptance.
// Private client copies ensure even callers supplying ordinary mailbox clients
// cannot accidentally grant this path a mutating HTTP capability.
func verifyRetainedMessage(ctx context.Context, reader, source *mailboxClient) (messageIdentity, error) {
	// Report only stages chosen here or by readRetainedDiagnostic. Never
	// propagate client errors or values from either mailbox to workflow logs.
	fail := func(stage string) (messageIdentity, error) {
		return messageIdentity{}, errors.New("retained-" + stage)
	}
	if reader == nil || source == nil || reader.endpoint == source.endpoint {
		return fail("endpoints")
	}
	r, err := newRetainedMailboxClient(reader.endpoint, reader.password)
	if err != nil {
		return fail("reader-endpoint")
	}
	r.account = reader.account
	if err = r.session(ctx); err != nil {
		return fail("reader-session")
	}
	s, err := newRetainedMailboxClient(source.endpoint, source.password)
	if err != nil {
		return fail("source-endpoint")
	}
	if source.account != "" && source.account != r.account {
		return fail("source-account")
	}
	s.account = r.account
	if err = s.session(ctx); err != nil {
		return fail("source-session")
	}
	expected, stage := readRetainedDiagnostic(ctx, r)
	if stage != "" {
		return fail("reader-" + stage)
	}
	for _, read := range []struct {
		side   string
		client *mailboxClient
	}{{"source", s}, {"reader", r}, {"source", s}} {
		actual, stage := readRetainedDiagnostic(ctx, read.client)
		if stage != "" {
			return fail(read.side + "-" + stage)
		}
		if actual != expected {
			return fail(read.side + "-mismatch")
		}
	}
	return expected, nil
}

func retainedGetList(result object, limit int) ([]object, bool) {
	for key := range result {
		if key != "accountId" && key != "state" && key != "list" && key != "notFound" {
			return nil, false
		}
	}
	if state := text(result, "state"); state == "" || len(state) > 512 {
		return nil, false
	}
	if missing, present := result["notFound"]; present {
		ids, ok := missing.([]any)
		if !ok || len(ids) != 0 {
			return nil, false
		}
	}
	raw, ok := result["list"].([]any)
	list := items(result, "list")
	return list, ok && len(raw) == len(list) && len(list) <= limit
}

func retainedQueryID(result object) (string, bool) {
	for key := range result {
		switch key {
		case "accountId", "queryState", "canCalculateChanges", "position", "ids", "total", "limit":
		default:
			return "", false
		}
	}
	state := text(result, "queryState")
	ids, ok := result["ids"].([]any)
	if !ok || len(ids) != 1 || state == "" || len(state) > 512 || result["position"] != float64(0) || result["total"] != float64(1) {
		return "", false
	}
	if limit, present := result["limit"]; present {
		n, ok := limit.(float64)
		if !ok || n < 1 || n != float64(int64(n)) {
			return "", false
		}
	}
	id, ok := ids[0].(string)
	return id, ok && mailboxIDPattern.MatchString(id)
}

// The second result is a fixed failure stage suitable for workflow logs.
func readRetainedDiagnostic(ctx context.Context, c *mailboxClient) (messageIdentity, string) {
	result, err := c.call(ctx, "Mailbox/get", object{"properties": []any{"id", "name", "isSubscribed"}})
	if err != nil {
		return messageIdentity{}, "mailbox-list"
	}
	boxes, complete := retainedGetList(result, retainedMailboxLimit)
	if !complete {
		return messageIdentity{}, "mailbox-list"
	}
	var expected messageIdentity
	seen := map[string]bool{}
	for _, box := range boxes {
		id, name := text(box, "id"), text(box, "name")
		subscribed, valid := box["isSubscribed"].(bool)
		if !mailboxIDPattern.MatchString(id) || seen[id] || name == "" || !valid {
			return messageIdentity{}, "mailbox-list"
		}
		seen[id] = true
		match := retainedDiagnosticName.FindStringSubmatch(name)
		if len(match) == 0 {
			continue
		}
		if subscribed || expected.mailbox != "" {
			return messageIdentity{}, "mailbox-ambiguous"
		}
		expected.mailbox, expected.body = id, name
		expected.messageID = "email-recovery-" + match[1] + "@example.invalid"
	}
	if expected.mailbox == "" {
		return messageIdentity{}, "mailbox-missing"
	}
	result, err = c.call(ctx, "Email/query", object{"filter": object{"inMailbox": expected.mailbox}, "calculateTotal": true, "position": 0, "limit": 2})
	if err != nil {
		return messageIdentity{}, "email-query"
	}
	var completeID bool
	expected.email, completeID = retainedQueryID(result)
	if !completeID {
		return messageIdentity{}, "email-query"
	}
	result, err = c.call(ctx, "Email/get", object{"ids": []any{expected.email}, "properties": retainedMessageProperties(), "fetchTextBodyValues": true})
	if err != nil {
		return messageIdentity{}, "email-get"
	}
	messages, complete := retainedGetList(result, 1)
	if !complete || len(messages) != 1 {
		return messageIdentity{}, "email-get"
	}
	expected.blob = text(messages[0], "blobId")
	parts := items(messages[0], "textBody")
	if !mailboxIDPattern.MatchString(expected.blob) || len(parts) != 1 || !mailboxIDPattern.MatchString(text(parts[0], "partId")) || len(child(messages[0], "bodyValues")) != 1 || !proveMessage(messages[0], expected) {
		return messageIdentity{}, "email-content"
	}
	return expected, ""
}
