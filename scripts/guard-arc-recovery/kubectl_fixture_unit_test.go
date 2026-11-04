package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fixtureABCChecksum = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func TestKubectlFixtureRefusesUnknownOrUnapprovedPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, version, checksum, platform string
	}{
		{"missing approval", "", "", "linux/amd64"},
		{"unsupported platform", "v1.36.2", fixtureABCChecksum, "windows/amd64"},
		{"Darwin version without approved checksum", "v1.36.3", fixtureABCChecksum, "darwin/arm64"},
		{"invalid version", "v1.36.2/other", fixtureABCChecksum, "linux/amd64"},
		{"invalid checksum", "v1.36.2", "not a checksum", "linux/amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []nativeCIStep{{Env: map[string]string{"KUBECTL_VERSION": tc.version, "KUBECTL_SHA256": tc.checksum}}}
			if _, err := kubectlFixturePin(steps, tc.platform); err == nil {
				t.Fatal("accepted a native tool without an approved version/platform checksum pair")
			}
		})
	}
}

func TestKubectlDownloadPublishesVerifiedPrivateExecutable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "kubectl")
	if err := downloadPinnedKubectl(t.Context(), server.Client(), server.URL, fixtureABCChecksum, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "abc" {
		t.Fatalf("verified download missing or changed: %v", err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("download is not a private executable: %v", err)
	}
}

func TestKubectlDownloadRejectsUnverifiedOrFailedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"wrong checksum", "unverified bytes", http.StatusOK},
		{"HTTP failure with matching bytes", "abc", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			destination := filepath.Join(t.TempDir(), "kubectl")
			if err := downloadPinnedKubectl(t.Context(), server.Client(), server.URL, fixtureABCChecksum, destination); err == nil {
				t.Fatal("accepted an unverified executable")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed download left an executable or partial file: %v", err)
			}
		})
	}
}

func TestKubectlDownloadDoesNotReplaceExistingFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(destination, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := downloadPinnedKubectl(t.Context(), server.Client(), server.URL, fixtureABCChecksum, destination); err == nil {
		t.Fatal("replaced an existing file")
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "retained" {
		t.Fatalf("existing file changed: %v", err)
	}
}

func TestKubectlDownloadHonorsSetupDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	destination := filepath.Join(t.TempDir(), "kubectl")
	if err := downloadPinnedKubectl(ctx, server.Client(), server.URL, fixtureABCChecksum, destination); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("download did not preserve its setup deadline: %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled download left a file: %v", err)
	}
}
