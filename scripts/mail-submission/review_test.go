package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/time/rate"
)

func TestConfigurationRefusesOutOfDateReceivingCertificate(t *testing.T) {
	for _, future := range []bool{false, true} {
		name := "expired"
		if future {
			name = "not yet valid"
		}
		t.Run(name, func(t *testing.T) {
			dir := fixtureDirectory(t)
			keyPEM, err := os.ReadFile(filepath.Join(dir, "server.key"))
			if err != nil {
				t.Fatal(err)
			}
			block, _ := pem.Decode(keyPEM)
			if block == nil {
				t.Fatal("fixture key missing")
			}
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			signer, ok := key.(crypto.Signer)
			if !ok {
				t.Fatal("fixture key is not a signer")
			}
			certPEM, err := os.ReadFile(filepath.Join(dir, "server.crt"))
			if err != nil {
				t.Fatal(err)
			}
			block, _ = pem.Decode(certPEM)
			if block == nil {
				t.Fatal("fixture certificate missing")
			}
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			leaf.NotBefore, leaf.NotAfter = now.Add(-2*time.Hour), now.Add(-time.Hour)
			if future {
				leaf.NotBefore, leaf.NotAfter = now.Add(time.Hour), now.Add(2*time.Hour)
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, signer.Public(), signer)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "server.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err = loadConfiguration(dir); err == nil {
				t.Error("out-of-date receiving certificate accepted")
			}
			var output bytes.Buffer
			if status := run(context.Background(), []string{"check", "--config-dir", dir}, &output); status == 0 {
				t.Error("check accepted out-of-date receiving certificate")
			}
		})
	}
}

// These bytes are valid on the SMTP wire, but not a three-field PLAIN response.
type malformedPlainClient struct{}

func (malformedPlainClient) Start() (string, []byte, error) {
	return sasl.Plain, []byte("not-a-plain-response"), nil
}
func (malformedPlainClient) Next([]byte) ([]byte, error) {
	return nil, errors.New("unexpected authentication challenge")
}

func TestMalformedPlainConsumesConnectionBudget(t *testing.T) {
	cfg, _, trust := fixtureConfiguration(t)
	address := fixtureGateway(t, cfg, nil)
	client, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	for range maxAuthAttempts {
		if err = client.Auth(malformedPlainClient{}); err == nil {
			t.Fatal("malformed PLAIN accepted")
		}
	}
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err == nil {
		t.Fatal("malformed PLAIN bypassed connection authentication budget")
	}
}

func TestMalformedPlainConsumesSharedBudget(t *testing.T) {
	cfg, _, trust := fixtureConfiguration(t)
	address := fixtureGateway(t, cfg, nil, func(g *gateway) {
		g.authenticationLimiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	})
	first, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if err = first.Auth(malformedPlainClient{}); err == nil {
		t.Fatal("malformed PLAIN accepted")
	}
	second, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if err = second.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err == nil {
		t.Fatal("malformed PLAIN bypassed shared authentication budget")
	}
}
