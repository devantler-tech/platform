package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeZitadelRenderedMessage(t *testing.T) {
	encoded, err := os.ReadFile("testdata/zitadel-v4.20.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Upstream, SourceSHA, Message string }
	if err = json.Unmarshal(encoded, &fixture); err != nil || fixture.Upstream != "8afa5dd37a8e723efb9eace8d09fd5c1cccef694" || fixture.SourceSHA != "94715fb2a0262576a132f5e7f08353b9d12e7fee73df344c4ce241dcdb25c73a" {
		t.Fatal("native message provenance differs")
	}
	raw := []byte(fixture.Message)
	input, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || input.Header.Get("Return-Path") != "invitations@example.invalid" {
		t.Fatal("native message fixture lost the submitted return path")
	}
	cfg, peer, trust := fixtureConfiguration(t)
	client := authenticatedClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
	if err = client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, bytes.NewReader(raw)); err != nil {
		t.Fatal("native ZITADEL message refused:", err)
	}
	select {
	case delivered := <-peer.messages:
		message, err := mail.ReadMessage(bytes.NewReader(delivered.body))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(message.Body)
		if err != nil || !strings.Contains(string(body), "fixture-message-canary") {
			t.Fatal("native message body changed")
		}
		if len(message.Header["Return-Path"]) != 0 || delivered.from != cfg.Settings.Sender {
			t.Fatal("submitted trace header reached upstream or changed the enforced envelope")
		}
		if message.Header.Get("From") != input.Header.Get("From") || message.Header.Get("Subject") != input.Header.Get("Subject") {
			t.Fatal("native message author or subject changed")
		}
	case <-time.After(time.Second):
		t.Fatal("native message missing upstream")
	}
}

func TestSubmittedReturnPathRemovalPreservesOtherBytes(t *testing.T) {
	for _, field := range []string{
		"Return-Path: invitations@example.invalid\r\n",
		"rEtUrN-pAtH: <invitations@example.invalid>\r\n",
		"Return-Path:\r\n\tinvitations@example.invalid\r\n",
	} {
		original := "X-Fixture: unchanged\r\n\tfolded value\r\n" + validMessage + "Return-Path: body text is retained\r\n"
		for _, input := range []string{field + original, strings.Replace(original, "Subject:", field+"Subject:", 1)} {
			if got := stripSubmittedReturnPath([]byte(input)); !bytes.Equal(got, []byte(original)) {
				t.Fatal("removing a submitted trace field changed unrelated headers or body")
			}
		}
		if got := stripSubmittedReturnPath([]byte(original)); !bytes.Equal(got, []byte(original)) {
			t.Fatal("message without a submitted return path changed")
		}
	}
}
