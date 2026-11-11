package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func boundedMessage(size int) string {
	remaining := size - len(validMessage) - 2
	const line = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\n"
	return validMessage + strings.Repeat(line, remaining/len(line)) + strings.Repeat("x", remaining%len(line)) + "\r\n"
}

func TestNativeNewMailCannotReuseRecipient(t *testing.T) {
	for _, refusedSender := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused_sender_%t", refusedSender), func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			client := authenticatedClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
			if err := client.Mail(cfg.Settings.Sender, nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Rcpt("earlier@example.invalid", nil); err != nil {
				t.Fatal(err)
			}
			from := cfg.Settings.Sender
			if refusedSender {
				from = "refused@example.invalid"
			}
			if err := client.Mail(from, nil); (err != nil) != refusedSender {
				t.Fatalf("unexpected new MAIL result: %v", err)
			}
			writer, err := client.Data()
			if err == nil {
				_, _ = io.WriteString(writer, validMessage)
				err = writer.Close()
			}
			if err == nil {
				t.Fatal("new MAIL reused the earlier recipient without RCPT")
			}
			select {
			case <-peer.messages:
				t.Fatal("incomplete transaction reached upstream")
			default:
			}
			if err = client.Reset(); err != nil {
				t.Fatal(err)
			}
			if err = client.SendMail(cfg.Settings.Sender, []string{"current@example.invalid"}, strings.NewReader(validMessage)); err != nil {
				t.Fatal("valid transaction after RSET refused:", err)
			}
			select {
			case message := <-peer.messages:
				if len(message.to) != 1 || message.to[0] != "current@example.invalid" {
					t.Fatal("old recipient survived reset")
				}
			case <-time.After(time.Second):
				t.Fatal("valid transaction missing upstream")
			}
		})
	}
}

func TestNativeDataMessageBoundary(t *testing.T) {
	for _, size := range []int{maxMessageBytes - 1, maxMessageBytes, maxMessageBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			client := authenticatedClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
			body := boundedMessage(size)
			if len(body) != size {
				t.Fatal("fixture size differs")
			}
			// No SIZE declaration: exercise the actual DATA reader and application bound.
			if err := client.Mail(cfg.Settings.Sender, nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Rcpt("client@example.invalid", nil); err != nil {
				t.Fatal(err)
			}
			writer, err := client.Data()
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := io.WriteString(writer, body)
			closeErr := writer.Close()
			if size > maxMessageBytes {
				if writeErr == nil && closeErr == nil {
					t.Fatal("oversize DATA accepted")
				}
				select {
				case <-peer.messages:
					t.Fatal("oversize DATA reached upstream")
				default:
				}
				return
			}
			if writeErr != nil || closeErr != nil {
				t.Fatalf("in-bound DATA refused: write=%v close=%v", writeErr, closeErr)
			}
			select {
			case message := <-peer.messages:
				if !bytes.Equal(message.body, []byte(body)) {
					t.Fatal("in-bound DATA bytes changed")
				}
			case <-time.After(time.Second):
				t.Fatal("in-bound DATA missing upstream")
			}
		})
	}
}

func TestNativeDeclaredMessageBound(t *testing.T) {
	cfg, peer, trust := fixtureConfiguration(t)
	client := authenticatedClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
	if err := client.Mail(cfg.Settings.Sender, &smtp.MailOptions{Size: maxMessageBytes + 1}); err == nil {
		t.Fatal("declared oversize message accepted")
	}
	select {
	case <-peer.messages:
		t.Fatal("declared oversize message reached upstream")
	default:
	}
}
