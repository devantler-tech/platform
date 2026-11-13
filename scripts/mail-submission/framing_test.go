package main

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/textproto"
	"testing"
	"time"
)

func framingClient(t *testing.T, address string, trust *tls.Config, cfg configuration) (*tls.Conn, *textproto.Conn) {
	t.Helper()
	conn, err := tls.Dial("tcp", address, trust)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	wire := textproto.NewConn(conn)
	if _, _, err = wire.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	plain := base64.StdEncoding.EncodeToString([]byte("\x00" + cfg.Settings.ClientUser + "\x00" + cfg.ClientPassword))
	for _, command := range []struct {
		text string
		code int
	}{
		{"EHLO fixture.example.invalid", 250},
		{"AUTH PLAIN " + plain, 235},
		{"MAIL FROM:<invitations@example.invalid>", 250},
		{"RCPT TO:<client@example.invalid>", 250},
	} {
		if err = wire.PrintfLine("%s", command.text); err != nil {
			t.Fatal(err)
		}
		if _, _, err = wire.ReadResponse(command.code); err != nil {
			t.Fatal(err)
		}
	}
	return conn, wire
}

func TestNativeHeaderFramingRefusals(t *testing.T) {
	for _, transport := range []string{"DATA", "BDAT"} {
		for _, ending := range []string{"\n", "\r"} {
			t.Run(fmt.Sprintf("%s_%q", transport, ending), func(t *testing.T) {
				cfg, peer, trust := fixtureConfiguration(t)
				_, wire := framingClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
				body := "Return-Path: invitations@example.invalid" + ending + validMessage
				if transport == "DATA" {
					if err := wire.PrintfLine("DATA"); err != nil {
						t.Fatal(err)
					}
					if _, _, err := wire.ReadResponse(354); err != nil {
						t.Fatal(err)
					}
				} else if err := wire.PrintfLine("BDAT %d LAST", len(body)); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(wire.W, body); err != nil {
					t.Fatal(err)
				}
				if transport == "DATA" {
					if _, err := io.WriteString(wire.W, ".\r\n"); err != nil {
						t.Fatal(err)
					}
				}
				if err := wire.W.Flush(); err != nil {
					t.Fatal(err)
				}
				if _, _, err := wire.ReadResponse(554); err != nil {
					t.Fatal("malformed header framing was not refused:", err)
				}
				select {
				case <-peer.messages:
					t.Fatal("malformed headers reached upstream")
				default:
				}
			})
		}
	}
}

func TestNativeBDATRequiresCompleteChunks(t *testing.T) {
	for _, last := range []bool{false, true} {
		t.Run(fmt.Sprintf("last_%t", last), func(t *testing.T) {
			cfg, peer, trust := fixtureConfiguration(t)
			conn, wire := framingClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
			suffix := ""
			if last {
				suffix = " LAST"
			}
			if err := wire.PrintfLine("BDAT %d%s", len(validMessage)+100, suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(wire.W, validMessage); err != nil {
				t.Fatal(err)
			}
			if err := wire.W.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := wire.ReadResponse(554); err != nil {
				t.Fatal("incomplete BDAT was not refused:", err)
			}
			select {
			case <-peer.messages:
				t.Fatal("truncated BDAT reached upstream")
			default:
			}
		})
	}
}

func TestNativeBDATPreservesCompleteMessage(t *testing.T) {
	cfg, peer, trust := fixtureConfiguration(t)
	_, wire := framingClient(t, fixtureGateway(t, cfg, nil), trust, cfg)
	// Header framing does not reject body line endings. The upstream DATA
	// writer retains its existing CRLF normalization.
	want := validMessage + "body with bare LF\r\nsecond line\r\n"
	message := "Return-Path: invitations@example.invalid\r\n" + validMessage + "body with bare LF\nsecond line\r\n"
	for index, chunk := range []string{message[:30], message[30:]} {
		suffix := ""
		if index == 1 {
			suffix = " LAST"
		}
		if err := wire.PrintfLine("BDAT %d%s", len(chunk), suffix); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(wire.W, chunk); err != nil {
			t.Fatal(err)
		}
		if err := wire.W.Flush(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := wire.ReadResponse(250); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-peer.messages:
		if string(got.body) != want {
			t.Fatal("valid chunked message lost its author or changed content")
		}
	case <-time.After(time.Second):
		t.Fatal("complete chunked message missing upstream")
	}
}
