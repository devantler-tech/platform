package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const companyMailbox = "ned@devantler.tech"

var mailboxIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

type mailboxClient struct {
	account, endpoint, password string
	http                        *http.Client
}

func newMailboxClient(endpoint, password string) (*mailboxClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || password == "" {
		return nil, errors.New("mailbox-endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("mailbox-endpoint")
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &mailboxClient{endpoint: endpoint, password: password, http: client}, nil
}
func (c *mailboxClient) request(ctx context.Context, method, path, contentType string, input []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(input))
	if err != nil {
		return nil, errors.New("mailbox-request")
	}
	req.SetBasicAuth(companyMailbox, c.password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("mailbox-transport")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return nil, errors.New("mailbox-http")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("mailbox-response")
	}
	return data, nil
}
func (c *mailboxClient) call(ctx context.Context, method string, args object) (object, error) {
	switch method {
	case "Email/get", "Email/query", "Email/import", "Email/set", "Mailbox/get", "Mailbox/set":
	default:
		return nil, errors.New("mailbox-method")
	}
	if !mailboxIDPattern.MatchString(c.account) {
		return nil, errors.New("mailbox-account")
	}
	bound := object{"accountId": c.account}
	for key, value := range args {
		if key == "accountId" {
			return nil, errors.New("mailbox-account-override")
		}
		bound[key] = value
	}
	body, err := json.Marshal(object{"using": []any{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}, "methodCalls": []any{[]any{method, bound, "proof"}}})
	if err != nil {
		return nil, errors.New("mailbox-input")
	}
	data, err := c.request(ctx, http.MethodPost, "/jmap", "application/json", body)
	if err != nil {
		return nil, err
	}
	var envelope object
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errors.New("mailbox-json")
	}
	responses, ok := envelope["methodResponses"].([]any)
	if !ok || len(responses) != 1 {
		return nil, errors.New("mailbox-receipt")
	}
	tuple, ok := responses[0].([]any)
	if !ok || len(tuple) != 3 || tuple[0] != method || tuple[2] != "proof" {
		return nil, errors.New("mailbox-method-receipt")
	}
	result, ok := tuple[1].(map[string]any)
	if !ok || text(result, "accountId") != c.account {
		return nil, errors.New("mailbox-account-receipt")
	}
	return result, nil
}

type mailboxMarker struct {
	identity  messageIdentity
	name      string
	uncertain bool
}

func (c *mailboxClient) createMarker(ctx context.Context, nonce string) (*mailboxMarker, error) {
	marker := &mailboxMarker{name: "Email recovery verification " + nonce}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(nonce) {
		return marker, errors.New("marker-nonce")
	}
	marker.identity.body = marker.name
	marker.identity.messageID = "email-recovery-" + nonce + "@example.invalid"
	created, err := c.call(ctx, "Mailbox/set", object{"create": object{"proof": object{"name": marker.name, "isSubscribed": false}}})
	if err != nil {
		marker.uncertain = true
		return marker, err
	}
	marker.identity.mailbox = text(child(child(created, "created"), "proof"), "id")
	if !mailboxIDPattern.MatchString(marker.identity.mailbox) {
		marker.uncertain = true
		return marker, errors.New("marker-mailbox")
	}
	message := "From: Email recovery diagnostic <email-recovery@example.invalid>\r\nTo: email-recovery@example.invalid\r\nSubject: Email recovery verification\r\nMessage-ID: <" + marker.identity.messageID + ">\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + marker.identity.body + "\r\n"
	data, err := c.request(ctx, http.MethodPost, "/jmap/upload/"+c.account+"/", "message/rfc822", []byte(message))
	if err != nil {
		return marker, err
	}
	var upload object
	if json.Unmarshal(data, &upload) != nil || text(upload, "accountId") != c.account || !mailboxIDPattern.MatchString(text(upload, "blobId")) {
		return marker, errors.New("marker-upload")
	}
	marker.identity.blob = text(upload, "blobId")
	imported, err := c.call(ctx, "Email/import", object{"emails": object{"proof": object{"blobId": marker.identity.blob, "mailboxIds": object{marker.identity.mailbox: true}, "keywords": object{"$seen": true}}}})
	if err != nil {
		marker.uncertain = true
		return marker, err
	}
	marker.identity.email = text(child(child(imported, "created"), "proof"), "id")
	if !mailboxIDPattern.MatchString(marker.identity.email) {
		marker.uncertain = true
		return marker, errors.New("marker-import")
	}
	if _, err = c.verifyMessage(ctx, marker.identity); err != nil {
		return marker, err
	}
	return marker, nil
}
func (c *mailboxClient) verifyMessage(ctx context.Context, expected messageIdentity) (string, error) {
	result, err := c.call(ctx, "Email/get", object{"ids": []any{expected.email}, "properties": []any{"id", "blobId", "messageId", "mailboxIds", "textBody", "bodyValues"}, "fetchTextBodyValues": true})
	if err != nil {
		return "", err
	}
	messages := items(result, "list")
	if len(messages) != 1 || !proveMessage(messages[0], expected) || text(result, "state") == "" {
		return "", errors.New("message-proof")
	}
	return text(result, "state"), nil
}
func (c *mailboxClient) queryMarkerBox(ctx context.Context, box string) ([]any, error) {
	result, err := c.call(ctx, "Email/query", object{"filter": object{"inMailbox": box}, "calculateTotal": true, "limit": 2})
	if err != nil {
		return nil, err
	}
	ids, ok := result["ids"].([]any)
	if !ok || result["total"] != float64(len(ids)) || len(ids) > 1 {
		return nil, errors.New("marker-foreign-content")
	}
	return ids, nil
}
func (c *mailboxClient) cleanupMarker(ctx context.Context, marker *mailboxMarker) error {
	if marker == nil || marker.uncertain || !mailboxIDPattern.MatchString(marker.identity.mailbox) || !mailboxIDPattern.MatchString(marker.identity.email) {
		return errors.New("marker-retained")
	}
	ids, err := c.queryMarkerBox(ctx, marker.identity.mailbox)
	if err != nil || len(ids) != 1 || ids[0] != marker.identity.email {
		return errors.New("marker-foreign-content")
	}
	state, err := c.verifyMessage(ctx, marker.identity)
	if err != nil {
		return err
	}
	result, err := c.call(ctx, "Email/set", object{"ifInState": state, "destroy": []any{marker.identity.email}})
	if err != nil {
		return err
	}
	destroyed, ok := result["destroyed"].([]any)
	if !ok || len(destroyed) != 1 || destroyed[0] != marker.identity.email {
		return errors.New("marker-delete-unverified")
	}
	ids, err = c.queryMarkerBox(ctx, marker.identity.mailbox)
	if err != nil || len(ids) != 0 {
		return errors.New("marker-box-not-empty")
	}
	result, err = c.call(ctx, "Mailbox/get", object{"ids": []any{marker.identity.mailbox}, "properties": []any{"id", "name", "isSubscribed"}})
	if err != nil {
		return err
	}
	boxes := items(result, "list")
	state = text(result, "state")
	if len(boxes) != 1 || text(boxes[0], "id") != marker.identity.mailbox || text(boxes[0], "name") != marker.name || boxes[0]["isSubscribed"] != false || state == "" {
		return errors.New("marker-box-changed")
	}
	result, err = c.call(ctx, "Mailbox/set", object{"ifInState": state, "destroy": []any{marker.identity.mailbox}, "onDestroyRemoveEmails": false})
	if err != nil {
		return err
	}
	destroyed, ok = result["destroyed"].([]any)
	if !ok || len(destroyed) != 1 || destroyed[0] != marker.identity.mailbox {
		return errors.New("marker-box-delete-unverified")
	}
	return nil
}
