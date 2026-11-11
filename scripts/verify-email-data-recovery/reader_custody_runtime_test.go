package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderCustodyTokenRequiresPrivateRegularLocalFiles(t *testing.T) {
	for _, failure := range []string{"", "public-directory", "public-file", "file-symlink", "directory-symlink", "empty", "oversized", "newline"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			if os.Chmod(dir, 0700) != nil {
				t.Fatal("private fixture directory")
			}
			value := "fixture-reader-custody-token"
			switch failure {
			case "empty":
				value = ""
			case "oversized":
				value = strings.Repeat("x", 257)
			case "newline":
				value += "\n"
			}
			path := filepath.Join(dir, "token")
			if os.WriteFile(path, []byte(value), 0600) != nil {
				t.Fatal("private fixture credential")
			}
			switch failure {
			case "public-directory":
				if os.Chmod(dir, 0755) != nil {
					t.Fatal("fixture mode")
				}
			case "public-file":
				if os.Chmod(path, 0644) != nil {
					t.Fatal("fixture mode")
				}
			case "file-symlink":
				if os.Rename(path, path+"-real") != nil || os.Symlink(path+"-real", path) != nil {
					t.Fatal("fixture symlink")
				}
			case "directory-symlink":
				link := dir + "-link"
				if os.Symlink(dir, link) != nil {
					t.Fatal("fixture directory symlink")
				}
				t.Cleanup(func() { _ = os.Remove(link) })
				dir = link
			}
			token, err := readReaderCustodyToken(dir)
			if failure == "" {
				if err != nil || token != value {
					t.Fatal("private bounded credential refused")
				}
			} else if err == nil || token != "" || strings.Contains(err.Error(), value) && value != "" {
				t.Fatal("unqualified credential accepted or exposed")
			}
		})
	}
}

func TestReaderCustodyTransportRefusesRemoteOrAmbiguousEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://127.0.0.1:38200", "http://example.invalid:38200", "http://localhost:38200", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:38200", "http://127.0.0.1:38200/path", "http://127.0.0.1:38200?", "http://127.0.0.1:38200#other"} {
		if store, err := newReaderCustodyStore(endpoint, "fixture-reader-custody-token"); err == nil || store != nil {
			t.Fatal("custody credential could reach an unqualified endpoint")
		}
	}
	if _, err := newReaderCustodyStore("http://127.0.0.1:38200", "token\r\nInjected: value"); err == nil {
		t.Fatal("malformed authentication accepted")
	}
}

func TestReaderCustodyCommandUsesTheSeparateProtectedDispatch(t *testing.T) {
	if os.Getenv("EMAIL_READER_CUSTODY_CHILD") == "1" {
		os.Args = []string{"verify-email-data-recovery", "--prepare-reader-inputs"}
		main()
		return
	}
	for _, confirmed := range []bool{false, true} {
		command := exec.Command(os.Args[0], "-test.run=^TestReaderCustodyCommandUsesTheSeparateProtectedDispatch$")
		command.Env = []string{"EMAIL_READER_CUSTODY_CHILD=1", "PATH=/nonexistent", "GOCOVERDIR=" + t.TempDir()}
		stage := "dispatch"
		if confirmed {
			for key, value := range readerCustodyDispatch() {
				command.Env = append(command.Env, key+"="+value)
			}
			command.Env = append(command.Env, "EMAIL_RETAINED_READER_UID=11111111-1111-2222-3333-444444444444", "EMAIL_RETAINED_CLAIM_UID=22222222-1111-2222-3333-444444444444")
			stage = "source"
		}
		output, err := command.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || !strings.Contains(string(output), "EMAIL_READER_INPUT_CUSTODY=UNVERIFIED stage="+stage+" restart=NOT_AUTHORIZED recovery=UNVERIFIED") || strings.Contains(string(output), "EMAIL_DATA_RECOVERY=PASS") {
			t.Fatalf("custody command lost its distinct dispatch boundary: %q", output)
		}
	}
}
