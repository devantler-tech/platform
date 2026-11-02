package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

func TestInventoryConfigCannotUseAmbientOrAlternateAuthentication(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}))
	fixture := func() *talosconfig.Config {
		return &talosconfig.Config{Context: "reviewed", Contexts: map[string]*talosconfig.Context{"reviewed": {Endpoints: []string{"192.0.2.2", "192.0.2.1"}, CA: ca, Crt: "protected-crt", Key: "protected-key"}}}
	}
	endpoint, err := inventoryEndpoint(fixture())
	if err != nil || endpoint != "192.0.2.1" {
		t.Fatal("explicit protected endpoint selection failed")
	}
	for _, change := range []func(*talosconfig.Config){
		func(c *talosconfig.Config) { c.Context = "" },
		func(c *talosconfig.Config) { c.Context = "missing" },
		func(c *talosconfig.Config) { c.Contexts[c.Context].Endpoints = nil },
		func(c *talosconfig.Config) {
			c.Contexts[c.Context].Endpoints = []string{"https://example.invalid/path"}
		},
		func(c *talosconfig.Config) { c.Contexts[c.Context].Endpoints = []string{"192.0.2.1", "192.0.2.1"} },
		func(c *talosconfig.Config) { c.Contexts[c.Context].Key = "" },
		func(c *talosconfig.Config) { c.Contexts[c.Context].CA = "\r\n" },
		func(c *talosconfig.Config) { c.Contexts[c.Context].CA = "Cg==" },
		func(c *talosconfig.Config) { c.Contexts[c.Context].CA = "not-base64" },
		func(c *talosconfig.Config) { c.Contexts[c.Context].ProxyURL = "http://example.invalid" },
		func(c *talosconfig.Config) {
			c.Contexts[c.Context].Auth.Basic = &talosconfig.Basic{Username: "alternate"}
		},
		func(c *talosconfig.Config) {
			c.Contexts[c.Context].Auth.SideroV1 = &talosconfig.SideroV1{Identity: "alternate"}
		},
	} {
		c := fixture()
		change(c)
		if _, err := inventoryEndpoint(c); err == nil {
			t.Fatal("unresolved or ambient configuration accepted")
		}
	}
}

func TestInventoryCommandCannotExecuteMutationOrSelectAnotherScope(t *testing.T) {
	for _, command := range [][]string{
		{"kubectl", "--context=admin@prod", "--request-timeout=20s", "delete", "nodes"},
		{"kubectl", "--context=other", "--request-timeout=20s", "get", "nodes", "--output=json", "--chunk-size=0"},
		{"kubectl", "--context=admin@prod", "--request-timeout=20s", "get", "secrets", "--output=json", "--chunk-size=0"},
		{"talosctl", "--nodes=10.0.0.2", "reset"},
		{"git", "ls-remote", "--exit-code", "https://example.invalid/private.git", "refs/heads/main"},
		{"sh", "-c", "true"},
	} {
		if _, err := inventoryReadCommand(context.Background(), nil, command[0], command[1:]...); err == nil {
			t.Fatal("inventory command escaped its fixed read-only scope")
		}
	}
	if _, err := inventoryReadCommand(context.Background(), []byte("mutation"), "kubectl", "--context=admin@prod", "--request-timeout=20s", "get", "nodes", "--output=json", "--chunk-size=0"); err == nil {
		t.Fatal("inventory command accepted a request body")
	}
}

func TestInventoryOutputBoundDoesNotAcceptTruncatedSuccess(t *testing.T) {
	w := inventoryOutput{limit: 4}
	if _, err := w.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("5")); err == nil || !w.exceeded || strings.Contains(w.buffer.String(), "5") {
		t.Fatal("overflow was accepted or retained")
	}
}
