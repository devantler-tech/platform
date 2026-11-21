package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// The image builder extracts the executable and exercises those exact bytes.
// The regular source tests still run when no packaged executable is selected.
func TestPackagedExecutable(t *testing.T) {
	binary := os.Getenv("MAIL_SUBMISSION_BINARY")
	if binary == "" {
		t.Skip("packaged executable not selected")
	}
	directory := fixtureDirectory(t)
	cfg, err := loadConfiguration(directory)
	if err != nil {
		t.Fatal(err)
	}
	upstream, peer := fixturePeer(t, cfg.ServerTLS)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	var settingsDocument map[string]string
	if err = json.Unmarshal([]byte(fixtureSettings), &settingsDocument); err != nil {
		t.Fatal(err)
	}
	settingsDocument["listenAddress"] = address
	settingsDocument["upstreamAddress"] = upstream
	raw, err := json.Marshal(settingsDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	check := func(valid bool) {
		t.Helper()
		output, checkErr := exec.CommandContext(ctx, binary, "check", "--config-dir", directory).CombinedOutput()
		if valid && (checkErr != nil || string(output) != "Submission configuration valid; no listener started\n") {
			t.Fatalf("packaged configuration check failed: %v: %s", checkErr, output)
		}
		if !valid && (checkErr == nil || string(output) != "Submission configuration refused\n") {
			t.Fatalf("packaged configuration refusal failed: %v: %s", checkErr, output)
		}
	}
	check(true)
	password := filepath.Join(directory, "client-password")
	if err = os.Chmod(password, 0o644); err != nil {
		t.Fatal(err)
	}
	check(false)
	if err = os.Chmod(password, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs lockedLog
	command := exec.CommandContext(ctx, binary, "serve", "--config-dir", directory)
	command.Stdout, command.Stderr = &logs, &logs
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = command.Process.Kill()
			<-finished
		}
	})
	trust := cfg.UpstreamTLS.Clone()
	var client *smtp.Client
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		client, err = smtp.DialTLS(address, trust)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("packaged listener unavailable: %v: %s", err, logs.String())
	}
	t.Cleanup(func() { _ = client.Close() })
	if err = client.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, cfg.ClientPassword)); err != nil {
		t.Fatal(err)
	}
	if err = client.Mail("other@example.invalid", nil); err == nil {
		t.Fatal("packaged executable accepted an alternative sender")
	}
	if err = client.Reset(); err != nil {
		t.Fatal(err)
	}
	message := []byte("From: Example Business <invitations@example.invalid>\r\nTo: client@example.invalid\r\nSubject: Fictional distribution check\r\n\r\npackaged-message-canary\r\n")
	if err = client.SendMail(cfg.Settings.Sender, []string{"client@example.invalid"}, bytes.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	select {
	case received := <-peer.messages:
		if received.from != cfg.Settings.Sender || len(received.to) != 1 || received.to[0] != "client@example.invalid" || !bytes.Equal(received.body, message) {
			t.Fatal("packaged executable changed the message or envelope")
		}
	case <-ctx.Done():
		t.Fatal("packaged executable did not submit the message")
	}
	badTrust := trust.Clone()
	badTrust.ServerName = "other.example.invalid"
	if unexpected, dialErr := smtp.DialTLS(address, badTrust); dialErr == nil {
		_ = unexpected.Close()
		t.Fatal("packaged executable accepted the wrong TLS server name")
	}
	wrongAuth, err := smtp.DialTLS(address, trust)
	if err != nil {
		t.Fatal(err)
	}
	if err = wrongAuth.Auth(sasl.NewPlainClient("", cfg.Settings.ClientUser, "wrong-password")); err == nil {
		_ = wrongAuth.Close()
		t.Fatal("packaged executable accepted the wrong password")
	}
	_ = wrongAuth.Close()
	if err = command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-finished:
		stopped = true
		if err != nil {
			t.Fatalf("packaged executable did not stop cleanly: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("packaged executable did not stop before deadline")
	}
	for _, private := range []string{cfg.ClientPassword, cfg.UpstreamPassword, "client@example.invalid", "packaged-message-canary"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("packaged executable logged private material")
		}
	}
}
