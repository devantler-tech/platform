package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPinnedServerRestoresSnapshotAndRejectsInvalidRecovery exercises actual Raft state.
func TestPinnedServerRestoresSnapshotAndRejectsInvalidRecovery(t *testing.T) {
	testPinnedRecovery(t, "")
}

func TestPinnedServerRestoresCanonicalEmailCredentials(t *testing.T) {
	testPinnedRecovery(t, "-email")
}

func TestPinnedServerRestoresEmailStorageKeyAndAccountCredentials(t *testing.T) {
	testPinnedRecovery(t, "-email-storage")
}

func testPinnedRecovery(t *testing.T, suffix string) {
	t.Helper()
	if os.Getenv("OPENBAO_BACKUP_RECOVERY_NATIVE_TEST") != "1" {
		t.Skip("actual pinned server proof runs in Linux CI")
	}
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Fatal("native proof requires the non-root Linux CI host")
	}
	if out, err := exec.Command("findmnt", "-n", "-o", "FSTYPE", "--target", "/dev/shm").Output(); err != nil || strings.TrimSpace(string(out)) != "tmpfs" {
		t.Fatal("native fixture must remain in memory")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	binary := filepath.Join(t.TempDir(), "verifier")
	build := exec.Command("go", "build", "-o", binary, "./scripts/openbao-backup-recovery")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("native verifier build failed: %v %s", err, out)
	}
	fixture, err := os.MkdirTemp("/dev/shm", "openbao-recovery-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fixture) })
	run := func(mode, dir string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 270*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", filepath.Join(root, "scripts/run-openbao-backup-sandbox.sh"), mode, dir, binary)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("fixture"+suffix, fixture); err != nil || strings.TrimSpace(out) != "OPENBAO_BACKUP_FIXTURE=PASS" {
		t.Fatalf("native synthetic snapshot creation failed: %v %s", err, out)
	}
	rootToken, err := privateInput(filepath.Join(fixture, "root-token"), 16384)
	if err != nil {
		t.Fatal("missing synthetic root credential")
	}
	unsealKey, err := privateInput(filepath.Join(fixture, "unseal-key"), 16384)
	if err != nil {
		t.Fatal("missing synthetic recovery credential")
	}
	object := credentialSecret(t, string(rootToken), string(unsealKey))
	archive := credentialArchive(t, credentialArchiveEntry{name: legacyKeyPath, body: object}, credentialArchiveEntry{name: preferredKeyPath, body: object})
	clear(rootToken)
	clear(unsealKey)
	clear(object)
	defer clear(archive)
	// Every restore now obtains its keys through the independent archive path.
	// Removing the originals prevents a healthy fixture from hiding a live-key fallback.
	for _, name := range []string{"root-token", "unseal-key"} {
		if os.Remove(filepath.Join(fixture, name)) != nil {
			t.Fatal("could not remove original synthetic credentials")
		}
	}
	for _, test := range []string{"matching", "corrupt-snapshot", "different-record", "wrong-unseal-key", "wrong-custody-scope"} {
		t.Run(test, func(t *testing.T) {
			dir, err := os.MkdirTemp("/dev/shm", "openbao-recovery-case-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for _, name := range []string{"expected.json", "snapshot.snap"} {
				b, err := privateInput(filepath.Join(fixture, name), maxSnapshotBytes)
				if err != nil || writePrivate(filepath.Join(dir, name), b) != nil {
					t.Fatal("could not prepare private synthetic handoff")
				}
			}
			recovered, err := readCredentialArchive(archive)
			if err != nil {
				t.Fatal("could not recover independent synthetic credentials")
			}
			if test == "wrong-unseal-key" {
				badObject := credentialSecret(t, recovered.Root, strings.Repeat("A", 43)+"=")
				badArchive := credentialArchive(t, credentialArchiveEntry{name: legacyKeyPath, body: badObject}, credentialArchiveEntry{name: preferredKeyPath, body: badObject})
				recovered, err = readCredentialArchive(badArchive)
				clear(badObject)
				clear(badArchive)
				if err != nil {
					t.Fatal("wrong-key archive did not reach native cryptographic verification")
				}
			}
			for name, value := range map[string]string{"root-token": recovered.Root, "unseal-key": recovered.Unseal} {
				if writePrivate(filepath.Join(dir, name), []byte(value)) != nil {
					t.Fatal("could not prepare independently recovered synthetic credential")
				}
			}
			switch test {
			case "corrupt-snapshot":
				b, err := os.ReadFile(filepath.Join(dir, "snapshot.snap"))
				if err != nil || len(b) == 0 {
					t.Fatal("missing synthetic snapshot")
				}
				b[0] ^= 0xff
				if os.WriteFile(filepath.Join(dir, "snapshot.snap"), b, 0600) != nil {
					t.Fatal("could not corrupt synthetic snapshot")
				}
			case "different-record":
				b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
				var records map[string]json.RawMessage
				if err != nil || json.Unmarshal(b, &records) != nil {
					t.Fatal("invalid synthetic records")
				}
				if suffix == "-email-storage" {
					// Preserve all three account expectations and alter the actual
					// storage key, so native restoration reaches key comparison.
					records[storageCredentialPath] = json.RawMessage(`{"passphrase":"` + strings.Repeat("d", 64) + `"}`)
				} else {
					path := "apps/private-mail/maintainer"
					if suffix != "" {
						path = "infrastructure/email/maintainer"
					}
					records[path] = json.RawMessage(`{"username":"changed-synthetic-user","password":"` + strings.Repeat("d", 48) + `","retainedField":"synthetic-fixture"}`)
				}
				b, err = json.Marshal(records)
				if err != nil || os.WriteFile(filepath.Join(dir, "expected.json"), b, 0600) != nil {
					t.Fatal("could not change synthetic expected record")
				}
			case "wrong-custody-scope":
				b, err := os.ReadFile(filepath.Join(dir, "expected.json"))
				var records map[string]json.RawMessage
				if err != nil || json.Unmarshal(b, &records) != nil {
					t.Fatal("invalid synthetic records")
				}
				wrongScope := map[string]json.RawMessage{}
				for key, value := range records {
					if suffix != "" {
						key = strings.Replace(key, "infrastructure/email/", "apps/private-mail/", 1)
					} else {
						key = strings.Replace(key, "apps/private-mail/", "infrastructure/email/", 1)
					}
					wrongScope[key] = value
				}
				b, err = json.Marshal(wrongScope)
				if err != nil || os.WriteFile(filepath.Join(dir, "expected.json"), b, 0600) != nil {
					t.Fatal("could not change synthetic custody scope")
				}
			}
			out, err := run("verify"+suffix, dir)
			if test == "matching" {
				count := 3
				if suffix == "-email-storage" {
					count = 4
				}
				if err != nil || strings.TrimSpace(out) != fmt.Sprintf("OPENBAO_BACKUP_RESTORE=PASS records=%d version=2.6.3", count) {
					t.Fatalf("actual snapshot restoration not proven: %v %s", err, out)
				}
			} else {
				stage := map[string]string{"corrupt-snapshot": "restore", "different-record": "records", "wrong-unseal-key": "unseal", "wrong-custody-scope": "inputs"}[test]
				if err == nil || strings.TrimSpace(out) != "OPENBAO_BACKUP_RESTORE=FAIL stage="+stage {
					t.Fatalf("native refusal did not prove its intended failure: %v %s", err, out)
				}
			}
		})
	}
}
