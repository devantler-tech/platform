package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"github.com/emersion/go-smtp"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureSettings = `{"listenAddress":"127.0.0.1:2465","sender":"invitations@example.invalid","senderName":"Example Business","clientUser":"application-fixture","upstreamAddress":"127.0.0.1:2587","upstreamServerName":"smtp.example.invalid","upstreamUser":"upstream-fixture"}`

func fixtureDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	server, _ := fixtureTLS(t)
	private, err := x509.MarshalPKCS8PrivateKey(server.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"settings.json": []byte(fixtureSettings), "client-password": []byte("fictional-application-password-0123456789"), "upstream-password": []byte("fictional-upstream-password-0123456789"),
		"server.crt":      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificates[0].Certificate[0]}),
		"server.key":      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}),
		"upstream-ca.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificates[0].Certificate[1]}),
	}
	for name, raw := range files {
		if err = os.WriteFile(filepath.Join(directory, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestConfigurationAndCheckNeverStartListener(t *testing.T) {
	dir := fixtureDirectory(t)
	cfg, err := loadConfiguration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Settings.Sender != "invitations@example.invalid" {
		t.Fatal("configuration not read")
	}
	var output bytes.Buffer
	if status := run(context.Background(), []string{"check", "--config-dir", dir}, &output); status != 0 {
		t.Fatal(output.String())
	}
	for _, args := range [][]string{nil, {"serve"}, {"serve", "--config-dir", ""}, {"unknown", "--config-dir", dir}, {"serve", "--config-dir", dir, "extra"}} {
		if status := run(context.Background(), args, &output); status == 0 {
			t.Fatal("invalid invocation accepted")
		}
	}
}

func TestConfigurationRefusesUnresolvedAndUnsafeValues(t *testing.T) {
	mutations := map[string]func(*configuration){
		"missing client password": func(c *configuration) { c.ClientPassword = "" },
		"weak password":           func(c *configuration) { c.ClientPassword = "short" },
		"reused password":         func(c *configuration) { c.ClientPassword = c.UpstreamPassword },
		"reused user":             func(c *configuration) { c.Settings.ClientUser = c.Settings.UpstreamUser },
		"substitution":            func(c *configuration) { c.ClientPassword = "${UNRESOLVED_APPLICATION_PASSWORD_123456789}" },
		"template":                func(c *configuration) { c.Settings.SenderName = "{{missing.name}}" },
		"insecure trust":          func(c *configuration) { c.UpstreamTLS.InsecureSkipVerify = true },
		"no trust":                func(c *configuration) { c.UpstreamTLS.RootCAs = nil },
		"different server name":   func(c *configuration) { c.UpstreamTLS.ServerName = "other.example.invalid" },
		"no key":                  func(c *configuration) { c.ServerTLS = nil },
		"hostname listener":       func(c *configuration) { c.Settings.ListenAddress = "localhost:2465" },
		"privileged port":         func(c *configuration) { c.Settings.ListenAddress = "127.0.0.1:465" },
		"bad upstream":            func(c *configuration) { c.Settings.UpstreamAddress = "smtp.example.invalid:0" },
		"mailbox group":           func(c *configuration) { c.Settings.Sender = "group: invitations@example.invalid;" },
		"header injection":        func(c *configuration) { c.Settings.SenderName = "Example\r\nFrom: other@example.invalid" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg, _, _ := fixtureConfiguration(t)
			cfg.UpstreamTLS = cfg.UpstreamTLS.Clone()
			mutate(&cfg)
			if _, err := newGateway(cfg, nil); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestPrivateConfigurationFileRefusals(t *testing.T) {
	for _, name := range []string{"missing", "world readable", "group writable", "executable", "symlink", "directory", "oversize", "invalid trust", "trust prefix", "duplicate", "unknown", "trailing", "null", "wrong type"} {
		t.Run(name, func(t *testing.T) {
			dir := fixtureDirectory(t)
			target := filepath.Join(dir, "settings.json")
			switch name {
			case "missing":
				_ = os.Remove(target)
			case "world readable":
				_ = os.Chmod(target, 0o644)
			case "group writable":
				_ = os.Chmod(target, 0o620)
			case "executable":
				_ = os.Chmod(target, 0o700)
			case "symlink":
				_ = os.Remove(target)
				if err := os.Symlink(filepath.Join(fixtureDirectory(t), "settings.json"), target); err != nil {
					t.Fatal(err)
				}
			case "directory":
				_ = os.Remove(target)
				_ = os.Mkdir(target, 0o700)
			case "oversize":
				_ = os.WriteFile(target, []byte(strings.Repeat(" ", 4097)), 0o600)
			case "invalid trust":
				_ = os.WriteFile(filepath.Join(dir, "upstream-ca.pem"), []byte("not a certificate"), 0o600)
			case "trust prefix":
				p := filepath.Join(dir, "upstream-ca.pem")
				raw, _ := os.ReadFile(p)
				_ = os.WriteFile(p, append([]byte("unexpected prefix\n"), raw...), 0o600)
			case "duplicate":
				_ = os.WriteFile(target, []byte(strings.TrimSuffix(fixtureSettings, "}")+`,"sender":"other@example.invalid"}`), 0o600)
			case "unknown":
				_ = os.WriteFile(target, []byte(strings.TrimSuffix(fixtureSettings, "}")+`,"ignored":"private-canary"}`), 0o600)
			case "trailing":
				_ = os.WriteFile(target, []byte(fixtureSettings+` {}`), 0o600)
			case "null":
				_ = os.WriteFile(target, []byte(strings.TrimSuffix(fixtureSettings, "}")+`,"replyTo":null}`), 0o600)
			case "wrong type":
				_ = os.WriteFile(target, []byte(strings.TrimSuffix(fixtureSettings, "}")+`,"replyTo":[]}`), 0o600)
			}
			if _, err := loadConfiguration(dir); err == nil {
				t.Fatal("unsafe private inputs accepted")
			}
			var output bytes.Buffer
			if status := run(context.Background(), []string{"check", "--config-dir", dir}, &output); status == 0 {
				t.Fatal("check accepted unsafe inputs")
			}
			if strings.Contains(output.String(), dir) || strings.Contains(output.String(), "private-canary") {
				t.Fatal("private input escaped in diagnostic")
			}
		})
	}
}

func TestProjectedConfigurationPinsOneGeneration(t *testing.T) {
	root := t.TempDir()
	generation := fixtureDirectory(t)
	// A normal Kubernetes projection uses an internal relative generation link.
	for _, name := range []string{"settings.json", "client-password", "upstream-password", "server.crt", "server.key", "upstream-ca.pem"} {
		raw, err := os.ReadFile(filepath.Join(generation, name))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "..generation")
		if err = os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(target, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(filepath.Join("..data", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("..generation", filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(root); err != nil {
		t.Fatalf("valid projected generation rejected: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(generation, filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(root); err == nil {
		t.Fatal("escaping generation accepted")
	}
}

func TestServeExitsWithoutLeavingAListener(t *testing.T) {
	dir := fixtureDirectory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if status := run(ctx, []string{"serve", "--config-dir", dir}, &output); status != 0 {
		t.Fatalf("shutdown failed: %s", output.String())
	}
	// A second invocation on the same address proves the first released it.
	if status := run(ctx, []string{"serve", "--config-dir", dir}, &output); status != 0 {
		t.Fatalf("listener leaked: %s", output.String())
	}
}

func TestShutdownGraceEndsWithActiveConnectionClosed(t *testing.T) {
	dir := fixtureDirectory(t)
	raw, err := os.ReadFile(filepath.Join(dir, "upstream-ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := trustCertificates(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve", "--config-dir", dir}, io.Discard) }()
	var client *smtp.Client
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client, err = smtp.DialTLS("127.0.0.1:2465", &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "smtp.example.invalid"})
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	cancel()
	select {
	case status := <-done:
		if status != 0 {
			t.Fatal("shutdown failed")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown exceeded grace")
	}
	if err = client.Noop(); err == nil {
		t.Fatal("active connection survived shutdown")
	}
}
