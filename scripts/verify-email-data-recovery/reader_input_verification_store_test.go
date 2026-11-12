package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInputVerificationRequiresAnEntirePrivateOriginalWitness catches missing, foreign, ambiguous or linked expectations.
func TestInputVerificationRequiresAnEntirePrivateOriginalWitness(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "duplicate", "foreign-path", "version-two", "public-file", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if os.WriteFile(filepath.Join(dir, "token"), []byte("synthetic-independent-archive-token"), 0600) != nil {
				t.Fatal("private token fixture")
			}
			document, err := readerCustodyDocument(custodyBundle(t))
			if err != nil {
				t.Fatal(err)
			}
			version := 1
			if scenario == "version-two" {
				version = 2
			}
			value := object{"data": document, "metadata": object{"version": version, "created_time": "2026-01-01T00:00:00Z", "deletion_time": "", "destroyed": false}}
			path := "infrastructure/email/recovery/retained-reader-inputs"
			if scenario == "foreign-path" {
				path = "infrastructure/email/storage"
			}
			raw, err := json.Marshal(object{path: value})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "duplicate" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + "," + strings.TrimPrefix(string(raw), "{"))
			}
			file := filepath.Join(dir, "expected.json")
			if scenario != "missing" && os.WriteFile(file, raw, 0600) != nil {
				t.Fatal("private record fixture")
			}
			if scenario == "public-file" && os.Chmod(file, 0644) != nil {
				t.Fatal("fixture mode")
			}
			if scenario == "symlink" {
				if os.Rename(file, file+".real") != nil || os.Symlink(file+".real", file) != nil {
					t.Fatal("fixture link")
				}
			}
			store, err := readReaderInputVerificationStore(dir)
			if scenario == "valid" {
				if err != nil || store == nil || store.originalRecord == nil {
					t.Fatalf("valid complete private original witness refused: %v", err)
				}
				store.client.CloseIdleConnections()
			} else if err == nil || store != nil {
				t.Fatal("unqualified original witness admitted")
			}
		})
	}
	for _, path := range []string{"/tmp/email-input-native.ABCDEFGH", "/dev/shm/email-reader-inputs.ABCDEFGH", "/dev/shm/email-input-native.short", "/dev/shm/email-input-native.ABCDEFGH/../foreign"} {
		if store, err := loadReaderInputVerificationStore(func(string) string { return path }); err == nil || store != nil {
			t.Fatal("verification borrowed preparation or arbitrary workspace authority")
		}
	}
}

// TestInputVerificationAcceptsItsActualLinuxRunnerWorkspace catches drift between the operator and loader directory prefixes.
func TestInputVerificationAcceptsItsActualLinuxRunnerWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the exact memory workspace is verified on the supported Linux runner")
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	directory := "/dev/shm/email-input-native." + hex.EncodeToString(nonce[:])
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	document, err := readerCustodyDocument(custodyBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(object{"infrastructure/email/recovery/retained-reader-inputs": object{"data": document, "metadata": object{"version": 1, "created_time": "2026-01-01T00:00:00Z", "deletion_time": "", "destroyed": false}}})
	if err != nil || os.WriteFile(filepath.Join(directory, "expected.json"), expected, 0600) != nil || os.WriteFile(filepath.Join(directory, "token"), []byte("synthetic-independent-archive-token"), 0600) != nil {
		t.Fatal("private memory fixture")
	}
	store, err := loadReaderInputVerificationStore(func(string) string { return directory })
	if err != nil || store == nil || store.originalRecord == nil {
		t.Fatal("operator workspace was refused by its actual verification loader")
	}
	store.client.CloseIdleConnections()
}
