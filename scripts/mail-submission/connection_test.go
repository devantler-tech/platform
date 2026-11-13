package main

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func TestAbsoluteConnectionDeadlineCannotBeExtended(t *testing.T) {
	for _, requested := range []time.Time{{}, time.Now().Add(time.Hour)} {
		t.Run(requested.String(), func(t *testing.T) {
			server, client := net.Pipe()
			defer func() { _ = server.Close() }()
			defer func() { _ = client.Close() }()
			bounded := &deadlineConn{Conn: server, expires: time.Now().Add(30 * time.Millisecond)}
			if err := bounded.SetReadDeadline(requested); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err := bounded.Read(make([]byte, 1))
			timeout, ok := err.(net.Error)
			if !ok || !timeout.Timeout() {
				t.Fatalf("absolute deadline did not fail closed: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("absolute deadline extended")
			}
		})
	}
}

func TestConnectionSlotsBoundUnauthenticatedPeers(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, _ := fixtureTLS(t)
	listener := newTLSListener(raw, serverTLS)
	defer func() { _ = listener.Close() }()
	var accepted []net.Conn
	for range maxConnections {
		client, e := net.Dial("tcp", raw.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = client.Close() }()
		server, e := listener.Accept()
		if e != nil {
			t.Fatal(e)
		}
		accepted = append(accepted, server)
		defer func() { _ = server.Close() }()
	}
	next := make(chan net.Conn, 1)
	errs := make(chan error, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			errs <- e
			return
		}
		next <- conn
	}()
	extra, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = extra.Close() }()
	select {
	case conn := <-next:
		_ = conn.Close()
		t.Fatal("connection limit exceeded")
	case err = <-errs:
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	_ = accepted[0].Close()
	select {
	case conn := <-next:
		_ = conn.Close()
	case err = <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("closed connection did not release slot")
	}
}

func TestNativeReceivingTLSAndRestartedCredential(t *testing.T) {
	cfg, _, trust := fixtureConfiguration(t)
	address := fixtureGateway(t, cfg, nil)
	old := trust.Clone()
	old.MinVersion = tls.VersionTLS10
	old.MaxVersion = tls.VersionTLS11
	if client, err := smtp.DialTLS(address, old); err == nil {
		_ = client.Close()
		t.Fatal("obsolete receiving TLS accepted")
	}
	_, wrong := fixtureTLS(t)
	if client, err := smtp.DialTLS(address, wrong); err == nil {
		_ = client.Close()
		t.Fatal("receiving trust not verified")
	}
	original := cfg.ClientPassword
	cfg.ClientPassword = "fictional-rotated-password-01234567890123456789"
	restarted := fixtureGateway(t, cfg, nil)
	client, err := smtp.DialTLS(restarted, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, original)); err == nil {
		t.Fatal("retired credential accepted after restart")
	}
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err != nil {
		t.Fatal(err)
	}
}
