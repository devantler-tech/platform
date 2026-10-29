package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The disabled mode must remain inert even when the deployment selects the
// remote journal or its explicit initialization command.
func TestDisabledRemoteJournalDoesNotReadDeploymentFacts(t *testing.T) {
	var output bytes.Buffer
	code := command([]string{"--journal-backend", "r2", "--r2-config-file", "/missing/private-canary", "--r2-credential-file", "/missing/private-canary", "--initialize-journal"}, forbiddenReader{}, &output, func(int64, []byte) (githubAPI, error) {
		t.Fatal("disabled remote mode constructed the GitHub publisher")
		return nil, errQuarantined
	})
	if code != 0 || !strings.Contains(output.String(), "disabled") || strings.Contains(output.String(), "private-canary") {
		t.Fatalf("disabled remote mode was not inert: code=%d output=%q", code, output.String())
	}
}

func r2PrivateFacts(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	config, credential := filepath.Join(dir, "r2.json"), filepath.Join(dir, "credential.json")
	if err := os.WriteFile(config, []byte(`{"accountId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","bucket":"fixture-journal","key":"alerts/state.json","instance":"fixture-instance"}`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"accessKeyId": "fixture-access-key", "secretAccessKey": strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credential, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return config, credential
}

func TestRemoteInitializationDoesNotNeedOrReadGitHubCredentials(t *testing.T) {
	config, credential := r2PrivateFacts(t)
	f := &r2HTTPFixture{}
	var output bytes.Buffer
	code := commandWithJournal([]string{"--enabled", "--journal-backend", "r2", "--r2-config-file", config, "--r2-credential-file", credential, "--initialize-journal"}, forbiddenReader{}, &output, func(int64, []byte) (githubAPI, error) {
		t.Fatal("initialization accessed the GitHub publisher")
		return nil, errQuarantined
	}, func(facts r2Settings, credential r2Credentials) (*r2JournalStore, error) {
		store, err := newR2JournalStore(facts, credential)
		if err == nil {
			attachR2Fixture(store, f)
		}
		return store, err
	})
	if code != 0 || f.puts != 1 || f.gets != 1 || f.unsafe {
		t.Fatalf("explicit initialization was not independent and verified: code=%d output=%q", code, output.String())
	}
}

func TestEnabledRemoteCommandUsesItsJournalThroughConfirmedDelivery(t *testing.T) {
	config, credential := r2PrivateFacts(t)
	key := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(key, testKey(t), 0400); err != nil {
		t.Fatal(err)
	}
	f := &r2HTTPFixture{}
	f.seed(initialR2Object)
	api := &fakeGitHub{}
	raw, err := json.Marshal(observation{Route: "cnpg-degraded", State: "firing", ObservedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	code := commandWithJournal([]string{"--enabled", "--journal-backend", "r2", "--r2-config-file", config, "--r2-credential-file", credential, "--key-file", key, "--installation-id", "17"}, bytes.NewReader(raw), &output, func(id int64, key []byte) (githubAPI, error) {
		if id != 17 || len(key) == 0 {
			t.Fatal("App identity facts lost")
		}
		return api, nil
	}, func(facts r2Settings, credential r2Credentials) (*r2JournalStore, error) {
		store, err := newR2JournalStore(facts, credential)
		if err == nil {
			attachR2Fixture(store, f)
		}
		return store, err
	})
	if code != 0 || api.writes != 1 || f.gets != 1 || f.puts != 3 || f.unsafe {
		t.Fatalf("remote command did not journal its verified receipt: code=%d output=%q", code, output.String())
	}
	var envelope struct {
		Journal journal `json:"journal"`
	}
	if json.Unmarshal(f.body, &envelope) != nil || len(envelope.Journal.Pending) != 0 || len(envelope.Journal.Receipts) != 1 {
		t.Fatal("remote CLI acknowledged unjournaled delivery")
	}
}

func TestInvalidRemoteInvocationAndInputReadNoCredentialOrProvider(t *testing.T) {
	for _, args := range [][]string{
		{"--enabled", "--journal-backend", "unknown"},
		{"--enabled", "--journal-backend", "r2", "--state-dir", "/local", "--r2-config-file", "/private-canary", "--r2-credential-file", "/private-canary"},
		{"--enabled", "--journal-backend", "local", "--state-dir", "/local", "--r2-config-file", "/private-canary"},
		{"--enabled", "--journal-backend", "r2", "--r2-config-file", "/private-canary", "--r2-credential-file", "/private-canary", "--initialize-journal", "--drain"},
	} {
		var output bytes.Buffer
		code := commandWithJournal(args, forbiddenReader{}, &output, func(int64, []byte) (githubAPI, error) {
			t.Fatal("invalid invocation constructed GitHub")
			return nil, errStore
		}, func(r2Settings, r2Credentials) (*r2JournalStore, error) {
			t.Fatal("invalid invocation constructed R2")
			return nil, errStore
		})
		if code != 2 || strings.Contains(output.String(), "private-canary") {
			t.Fatal("unsafe invocation accepted or echoed")
		}
	}
	var output bytes.Buffer
	code := commandWithJournal([]string{"--enabled", "--journal-backend", "r2", "--r2-config-file", "/missing/private-canary", "--r2-credential-file", "/missing/private-canary", "--key-file", "/missing/private-canary", "--installation-id", "17"}, strings.NewReader(`{"route":"private-canary"}`), &output, func(int64, []byte) (githubAPI, error) {
		t.Fatal("invalid observation constructed GitHub")
		return nil, errStore
	}, func(r2Settings, r2Credentials) (*r2JournalStore, error) {
		t.Fatal("invalid observation constructed R2")
		return nil, errStore
	})
	if code != 2 || !strings.Contains(output.String(), errInput.Error()) || strings.Contains(output.String(), "private-canary") {
		t.Fatal("invalid observation reached deployment loading or leaked")
	}
}

func TestR2CredentialsCannotInjectHeaderOrControlBytes(t *testing.T) {
	for _, value := range []string{"fixture-access-key\x00canary", "fixture-access-key\x1bcanary", "fixture-access-key\ncanary", "fixture-access-key canary"} {
		for _, secret := range []bool{false, true} {
			credential := r2Credentials{AccessKeyID: "fixture-access-key", SecretAccessKey: strings.Repeat("b", 64)}
			if secret {
				credential.SecretAccessKey = value
			} else {
				credential.AccessKeyID = value
			}
			if _, err := newR2JournalStore(r2Settings{AccountID: strings.Repeat("a", 32), Bucket: "fixture-journal", Key: "alerts/state.json", Instance: "fixture-instance"}, credential); err == nil {
				t.Fatal("malformed credential accepted")
			}
		}
	}
}

func TestR2PrivateFilesRefusePublicAmbiguousOrPartialFacts(t *testing.T) {
	for _, kind := range []string{"public-credential", "duplicate-setting", "unknown-setting", "partial-setting", "partial-credential", "oversized-credential"} {
		t.Run(kind, func(t *testing.T) {
			config, credential := r2PrivateFacts(t)
			switch kind {
			case "public-credential":
				if err := os.Chmod(credential, 0644); err != nil {
					t.Fatal(err)
				}
			case "duplicate-setting":
				raw, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte(`"instance":"fixture-instance"`), []byte(`"instance":"unapproved","instance":"fixture-instance"`), 1)
				if err := os.WriteFile(config, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "unknown-setting":
				raw, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				raw = bytes.Replace(raw, []byte(`"instance":`), []byte(`"destination":"private-canary","instance":`), 1)
				if err := os.WriteFile(config, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "partial-setting":
				if err := os.WriteFile(config, []byte(`{"accountId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "partial-credential":
				if err := os.WriteFile(credential, []byte(`{"accessKeyId":"private-canary"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized-credential":
				if err := os.WriteFile(credential, []byte(strings.Repeat(" ", 8193)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			code := commandWithJournal([]string{"--enabled", "--journal-backend", "r2", "--r2-config-file", config, "--r2-credential-file", credential, "--initialize-journal"}, forbiddenReader{}, &output, func(int64, []byte) (githubAPI, error) { t.Fatal("invalid facts accessed GitHub"); return nil, errStore }, func(r2Settings, r2Credentials) (*r2JournalStore, error) {
				t.Fatal("invalid files constructed a provider client")
				return nil, errStore
			})
			if code != 2 || strings.Contains(output.String(), "private-canary") {
				t.Fatal("private-file refusal failed or leaked")
			}
		})
	}
}

func TestR2PrivateInputRefusesANonRegularFileWithoutWaitingForAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var value r2Settings
		done <- privateR2JSON(path, &value, "accountId", "bucket", "key", "instance")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular input accepted")
		}
	case <-time.After(2 * time.Second):
		// Release the negative-control reader so a failing test never leaves a
		// blocked goroutine or FIFO handle behind.
		writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("negative-control reader did not terminate")
		}
		t.Fatal("nonregular credential input waited for a FIFO writer")
	}
}
