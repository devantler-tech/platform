package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAPIRefusesExternalOriginsRedirectsAndIncompleteHealth protects recovery keys.
func TestAPIRefusesExternalOriginsRedirectsAndIncompleteHealth(t *testing.T) {
	for _, origin := range []string{"https://example.invalid", "http://localhost:8200", "http://127.0.0.1:8200/path", "http://user@127.0.0.1:8200", "http://127.0.0.1:8200?redirect=yes"} {
		if _, err := newAPI(origin); err == nil {
			t.Fatal("unsafe recovery API origin accepted")
		}
	}
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Store(true); w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	a, err := newAPI(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := a.request(context.Background(), http.MethodGet, "/v1/sys/health", "synthetic-only", nil, 4096)
	if err != nil || status != 307 || followed.Load() {
		t.Fatal("redirect could expose the recovery token")
	}
	for _, body := range []string{`{}`, `{"initialized":true,"sealed":false}`, `{"initialized":true,"sealed":false,"version":"2.5.3"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client, err := newAPI(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.health(context.Background())
		server.Close()
		if err == nil {
			t.Fatal("incomplete or wrong-version health accepted")
		}
	}
}

// TestSandboxWrapperBindsCleanupAndFiltersPrivateOutput exercises lost start replies.
func TestSandboxWrapperBindsCleanupAndFiltersPrivateOutput(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("actual Linux wrapper runs in CI")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	for _, test := range []string{"lost-start-reply", "foreign-owner", "unreadable-cleanup", "private-output"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			// testing.TempDir's numbered child uses 0777 before the host umask;
			// satisfy the production handoff's explicit 0700 ownership boundary.
			if os.Chmod(dir, 0700) != nil {
				t.Fatal("cannot make the synthetic handoff private")
			}
			bin := filepath.Join(dir, "bin")
			if os.Mkdir(bin, 0700) != nil {
				t.Fatal("cannot prepare fixture tools")
			}
			verifier := filepath.Join(dir, "verifier")
			if os.WriteFile(verifier, []byte("#!/bin/sh\nprintf '%s\\n' '"+serverImage+"'\n"), 0700) != nil {
				t.Fatal("cannot prepare fixture verifier")
			}
			fake := `#!/bin/bash
set -euo pipefail
case "$1" in
run)
  printf '%s\n' reached > "$FIXTURE_DIR/reached"
  while [[ $# -gt 0 ]]; do
    if [[ $1 == --label ]]; then printf '%s' "${2#*=}" > "$FIXTURE_DIR/nonce"; fi
    shift
  done
  if [[ $FIXTURE_CASE == private-output ]]; then printf '%s\n' 'PRIVATE_SYNTHETIC_PAYLOAD'; exit 0; fi
  printf '%s\n' 'PRIVATE_SYNTHETIC_ERROR' >&2
  exit 1;;
container)
  [[ $FIXTURE_CASE != unreadable-cleanup ]] || exit 1
  printf '%064d\n' 0;;
inspect)
  if [[ $FIXTURE_CASE == foreign-owner ]]; then printf '%s\n' foreign; else cat "$FIXTURE_DIR/nonce"; fi;;
rm) printf '%s\n' removed > "$FIXTURE_DIR/removed";;
*) exit 91;;
esac
`
			if os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0700) != nil {
				t.Fatal("cannot prepare synthetic Docker boundary")
			}
			command := exec.Command("bash", filepath.Join(root, "scripts/run-openbao-backup-sandbox.sh"), "verify", dir, verifier)
			command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE_DIR="+dir, "FIXTURE_CASE="+test)
			out, err := command.CombinedOutput()
			if _, err := os.Stat(filepath.Join(dir, "reached")); err != nil {
				t.Fatal("fixture did not reach the intended Docker boundary")
			}
			if err == nil || strings.Contains(string(out), "PRIVATE_SYNTHETIC") || strings.Contains(string(out), "=PASS") {
				t.Fatalf("unsafe wrapper verdict: %v %s", err, out)
			}
			_, removed := os.Stat(filepath.Join(dir, "removed"))
			if test == "lost-start-reply" || test == "private-output" {
				if removed != nil {
					t.Fatal("owned container was not cleaned after ambiguous start or output")
				}
			} else {
				if removed == nil || !strings.Contains(string(out), "OPENBAO_BACKUP_CLEANUP=UNVERIFIED") {
					t.Fatal("cleanup mutated an unverified owner or reported it clean")
				}
			}
		})
	}
}
