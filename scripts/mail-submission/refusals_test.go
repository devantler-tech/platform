package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/time/rate"
)

const validMessage = "From: Example Business <invitations@example.invalid>\r\nTo: client@example.invalid\r\nSubject: Fictional invitation\r\n\r\nfixture-message-canary\r\n"

func authenticatedClient(t *testing.T, address string, trust *tls.Config, cfg configuration) *smtp.Client {
	t.Helper()
	client, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestNativeMessageRefusals(t *testing.T) {
	from := "From: Example Business <invitations@example.invalid>\r\n"
	cases := map[string]string{
		"other author":            strings.Replace(validMessage, "invitations@example.invalid", "someone@example.invalid", 1),
		"other display name":      strings.Replace(validMessage, "Example Business", "Fictional Impersonator", 1),
		"missing author":          strings.TrimPrefix(validMessage, from),
		"duplicate author":        from + validMessage,
		"case duplicate":          "fRoM: invitations@example.invalid\r\n" + validMessage,
		"multiple authors":        strings.Replace(validMessage, from, "From: invitations@example.invalid, someone@example.invalid\r\n", 1),
		"group author":            strings.Replace(validMessage, from, "From: Trusted: invitations@example.invalid;\r\n", 1),
		"folded extra author":     strings.Replace(validMessage, from, "From: invitations@example.invalid,\r\n someone@example.invalid\r\n", 1),
		"sender":                  "Sender: someone@example.invalid\r\n" + validMessage,
		"resent author":           "Resent-From: someone@example.invalid\r\n" + validMessage,
		"return path":             "Return-Path: someone@example.invalid\r\n" + validMessage,
		"reply to":                "Reply-To: someone@example.invalid\r\n" + validMessage,
		"missing header boundary": from + "Subject: fixture\r\n",
		"header bound":            from + "X-Fixture: a\r\n" + strings.Repeat(" aaaaaaaaaaaaaaaaaaaa\r\n", 3200) + "\r\nbody\r\n",
		"message bound":           validMessage + strings.Repeat("x\r\n", maxMessageBytes/3),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			logs := &lockedLog{}
			client := authenticatedClient(t, fixtureGateway(t, cfg, logs), trust, cfg)
			if err := client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, strings.NewReader(body)); err == nil {
				t.Fatal("invalid message accepted")
			}
			select {
			case <-peer.messages:
				t.Fatal("invalid message reached upstream")
			default:
			}
			for _, canary := range []string{cfg.ClientPassword, cfg.UpstreamPassword, "fixture-message-canary", "client@example.invalid"} {
				if strings.Contains(logs.String(), canary) {
					t.Fatal("private protocol data logged")
				}
			}
		})
	}
}

func TestNativeAuthenticationAndEnvelopeRefusals(t *testing.T) {
	for _, name := range []string{"anonymous", "wrong password", "wrong user", "delegated identity", "wrong envelope", "second recipient", "attempt limit"} {
		t.Run(name, func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			client, err := smtp.DialTLS(fixtureGateway(t, cfg, nil), trust)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			switch name {
			case "anonymous":
				err = client.Mail(cfg.Settings.Sender, nil)
			case "wrong password":
				err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, "wrong-password"))
			case "wrong user":
				err = client.Auth(sasl.NewPlainClient("", "other-user", cfg.ClientPassword))
			case "delegated identity":
				err = client.Auth(sasl.NewPlainClient("delegation", cfg.Settings.ClientUser, cfg.ClientPassword))
			case "attempt limit":
				for range maxAuthAttempts {
					if e := client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, "wrong")); e == nil {
						t.Fatal("bad authentication accepted")
					}
				}
				err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword))
			default:
				if e := client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); e != nil {
					t.Fatal(e)
				}
				if name == "wrong envelope" {
					err = client.Mail("someone@example.invalid", nil)
				} else {
					if e := client.Mail(cfg.Settings.Sender, nil); e != nil {
						t.Fatal(e)
					}
					if e := client.Rcpt("client@example.invalid", nil); e != nil {
						t.Fatal(e)
					}
					err = client.Rcpt("another@example.invalid", nil)
				}
			}
			if err == nil {
				t.Fatal("unauthorized protocol command accepted")
			}
			select {
			case <-peer.messages:
				t.Fatal("unauthorized message forwarded")
			default:
			}
		})
	}
}

func TestNativeUpstreamRefusals(t *testing.T) {
	for _, name := range []string{"wrong CA", "wrong name", "missing STARTTLS", "unavailable", "permanent refusal", "lost acknowledgement"} {
		t.Run(name, func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			cfg.UpstreamTLS = cfg.UpstreamTLS.Clone()
			logs := &lockedLog{}
			switch name {
			case "wrong CA":
				_, other := fixtureTLS(t)
				cfg.UpstreamTLS.RootCAs = other.RootCAs
			case "wrong name":
				cfg.Settings.UpstreamServerName = "other.example.invalid"
				cfg.UpstreamTLS.ServerName = cfg.Settings.UpstreamServerName
			case "missing STARTTLS":
				cfg.Settings.UpstreamAddress, peer = fixturePeer(t, nil)
			case "unavailable":
				cfg.Settings.UpstreamAddress = "127.0.0.1:1"
			case "permanent refusal":
				peer.reject.Store(true)
			case "lost acknowledgement":
				peer.drop.Store(true)
			}
			client := authenticatedClient(t, fixtureGateway(t, cfg, logs), trust, cfg)
			err := client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, strings.NewReader(validMessage))
			var protocol *smtp.SMTPError
			if !errors.As(err, &protocol) {
				t.Fatalf("expected classified refusal, got %v", err)
			}
			expected := 451
			if name == "permanent refusal" {
				expected = 554
			}
			if protocol.Code != expected {
				t.Fatalf("refusal %d, want %d", protocol.Code, expected)
			}
			if strings.Contains(err.Error(), "private-upstream-canary") || strings.Contains(logs.String(), "private-upstream-canary") {
				t.Fatal("private upstream error escaped")
			}
			if name == "wrong CA" || name == "wrong name" || name == "missing STARTTLS" {
				if peer.authAttempts.Load() != 0 {
					t.Fatal("upstream received credentials before verified TLS")
				}
			}
			if name == "lost acknowledgement" {
				select {
				case <-peer.messages:
				default:
					t.Fatal("fixture did not lose a real final acknowledgement")
				}
			}
		})
	}
}

func TestApplicationCredentialCannotAuthenticateUpstream(t *testing.T) {
	cfg, _, trust := fixtureConfiguration(t)
	client, err := smtp.DialStartTLS(cfg.Settings.UpstreamAddress, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err == nil {
		t.Fatal("application credential bypasses submission boundary")
	}
}

func TestResetDoesNotResetSubmissionBudget(t *testing.T) {
	cfg, peer, trust := fixtureConfiguration(t)
	address := fixtureGateway(t, cfg, nil, func(g *gateway) { g.limiter = rate.NewLimiter(rate.Every(time.Hour), 1) })
	client := authenticatedClient(t, address, trust, cfg)
	if err := client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, strings.NewReader(validMessage)); err != nil {
		t.Fatal(err)
	}
	if err := client.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, strings.NewReader(validMessage)); err == nil {
		t.Fatal("reset bypasses submission budget")
	}
	<-peer.messages
	select {
	case <-peer.messages:
		t.Fatal("over-budget submission forwarded")
	default:
	}
}

func TestNativePermittedAuthorFormsPreserveBytes(t *testing.T) {
	for _, from := range []string{"From: invitations@example.invalid\r\n", "From: Example Business\r\n <invitations@example.invalid>\r\n"} {
		t.Run(from, func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			client := authenticatedClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
			body := strings.Replace(validMessage, "From: Example Business <invitations@example.invalid>\r\n", from, 1)
			if err := client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, strings.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			got := <-peer.messages
			if !bytes.Equal(got.body, []byte(body)) {
				t.Fatal("permitted message rewritten")
			}
		})
	}
}

func TestAuthenticationBudgetSurvivesNewConnections(t *testing.T) {
	cfg, _, trust := fixtureConfiguration(t)
	address := fixtureGateway(t, cfg, nil, func(g *gateway) { g.authenticationLimiter = rate.NewLimiter(rate.Every(time.Hour), 1) })
	first := authenticatedClient(t, address, trust, cfg)
	_ = first.Close()
	client, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err == nil {
		t.Fatal("new connection reset shared authentication budget")
	}
}
