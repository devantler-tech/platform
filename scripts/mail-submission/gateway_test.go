package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func fixtureTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fictional SMTP fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"smtp.example.invalid"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der, caDER}, PrivateKey: pair.PrivateKey}}}, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "smtp.example.invalid"}
}

type peerMessage struct {
	from string
	to   []string
	body []byte
}
type peerBackend struct {
	messages     chan peerMessage
	reject       atomic.Bool
	drop         atomic.Bool
	authAttempts atomic.Int64
}

func (p *peerBackend) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	return &peerSession{peer: p, conn: conn}, nil
}

type peerSession struct {
	peer          *peerBackend
	conn          *smtp.Conn
	authenticated bool
	from          string
	to            []string
}

func (p *peerSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (p *peerSession) Auth(_ string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, user, password string) error {
		p.peer.authAttempts.Add(1)
		if identity != "" || user != "upstream-fixture" || password != "fictional-upstream-password-0123456789" {
			return smtp.ErrAuthFailed
		}
		p.authenticated = true
		return nil
	}), nil
}
func (p *peerSession) Mail(from string, _ *smtp.MailOptions) error {
	if !p.authenticated {
		return smtp.ErrAuthRequired
	}
	p.from = from
	return nil
}
func (p *peerSession) Rcpt(to string, _ *smtp.RcptOptions) error { p.to = append(p.to, to); return nil }
func (p *peerSession) Data(r io.Reader) error {
	if p.peer.reject.Load() {
		return refusal(550, "private-upstream-canary")
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	p.peer.messages <- peerMessage{p.from, append([]string(nil), p.to...), body}
	if p.peer.drop.Load() {
		_ = p.conn.Close()
		return io.EOF
	}
	return nil
}
func (p *peerSession) Reset()        { p.from = ""; p.to = nil }
func (p *peerSession) Logout() error { return nil }

func fixturePeer(t *testing.T, serverTLS *tls.Config) (string, *peerBackend) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &peerBackend{messages: make(chan peerMessage, 20)}
	server := smtp.NewServer(peer)
	server.Domain = "smtp.example.invalid"
	server.TLSConfig = serverTLS
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), peer
}

func fixtureConfiguration(t *testing.T) (configuration, *peerBackend, *tls.Config) {
	t.Helper()
	serverTLS, clientTLS := fixtureTLS(t)
	address, peer := fixturePeer(t, serverTLS)
	return configuration{Settings: settings{ListenAddress: "127.0.0.1:2465", Sender: "invitations@example.invalid", SenderName: "Example Business", ClientUser: "application-fixture", UpstreamAddress: address, UpstreamServerName: "smtp.example.invalid", UpstreamUser: "upstream-fixture"}, ClientPassword: "fictional-application-password-0123456789", UpstreamPassword: "fictional-upstream-password-0123456789", ServerTLS: serverTLS, UpstreamTLS: clientTLS}, peer, clientTLS
}

func fixtureGateway(t *testing.T, cfg configuration, logs io.Writer, configure ...func(*gateway)) string {
	t.Helper()
	g, err := newGateway(cfg, logs)
	if err != nil {
		t.Fatalf("valid submission configuration refused: %v", err)
	}
	for _, change := range configure {
		change(g)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.serve(listener) }()
	t.Cleanup(func() { _ = g.server.Close() })
	return listener.Addr().String()
}

func TestNativeSubmissionPreservesMessage(t *testing.T) {
	cfg, peer, trust := fixtureConfiguration(t)
	var logs bytes.Buffer
	address := fixtureGateway(t, cfg, &logs)
	client, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err != nil {
		t.Fatal(err)
	}
	message := []byte("From: Example Business <invitations@example.invalid>\r\nTo: client@example.invalid\r\nSubject: Fictional invitation\r\n\r\nfixture-message-canary\r\n")
	if err = client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, bytes.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-peer.messages:
		if got.from != cfg.Settings.Sender || len(got.to) != 1 || got.to[0] != "client@example.invalid" || !bytes.Equal(got.body, message) {
			t.Fatal("message or envelope changed")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive submission")
	}
}

type lockedLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }
