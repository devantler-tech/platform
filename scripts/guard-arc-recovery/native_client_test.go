package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Required organization coverage runs go test directly, without the native
// regression job's installation step. Qualify the dependency at its consumer so
// both entrypoints exercise the same approved client, never the runner's PATH.
func nativeKubectl(t *testing.T) string {
	t.Helper()
	const version = "v1.36.2"
	pins := map[string]string{
		"linux/amd64":  "1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82",
		"darwin/arm64": "4408c85c83fd3a31adaa555bdf3c7a6c81f74b19449a9060ba31ab91926f023d",
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	digest, ok := pins[platform]
	if !ok {
		t.Fatalf("no approved native kubectl for %s", platform)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 5 {
				return errors.New("native kubectl download redirect refused")
			}
			return nil
		},
	}
	path := filepath.Join(t.TempDir(), "kubectl")
	url := "https://dl.k8s.io/release/" + version + "/bin/" + platform + "/kubectl"
	if err := downloadNativeKubectl(ctx, client, url, digest, path); err != nil {
		t.Fatalf("qualify native kubectl: %v", err)
	}
	// Complete first-use client initialization before the proxy's own unchanged
	// ten-second deadline, and record the qualified version in verbose test output.
	startup, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	data, err := exec.CommandContext(startup, path, "version", "--client", "--output=json").Output()
	if err != nil {
		t.Fatalf("approved native kubectl client initialization failed: %v", err)
	}
	var result struct {
		ClientVersion struct{ GitVersion string } `json:"clientVersion"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.ClientVersion.GitVersion != version {
		t.Fatal("approved native kubectl reported an unexpected client version")
	}
	t.Logf("native proxy client=%s platform=%s sha256=%s", version, platform, digest)
	return path
}

func downloadNativeKubectl(ctx context.Context, client *http.Client, url, digest, destination string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("invalid native kubectl download request")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("native kubectl download failed")
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, errors.New("native kubectl response cleanup failed"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("native kubectl download status %d", response.StatusCode)
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".kubectl-download-")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, errors.New("native kubectl temporary download cleanup failed"))
		}
	}()
	const maxBytes = 80 << 20
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || size == 0 || size > maxBytes {
		return errors.New("native kubectl download incomplete or too large")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("native kubectl checksum differs from reviewed pin")
	}
	if err := os.Chmod(file.Name(), 0700); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

func TestNativeKubectlDownloadRejectsUnverifiedBytes(t *testing.T) {
	const body = "synthetic native client bytes"
	// Hand-checked SHA-256 of the literal fixture, independent of the downloader.
	const checksum = "d8ac7208be2eb55edb3b7c667ad310ac91db5859d2908e912e973ca71f249823"
	for _, tc := range []struct {
		name, digest, body string
		status             int
	}{
		{"changed bytes", checksum, body + "changed", http.StatusOK},
		{"wrong checksum", strings.Repeat("0", 64), body, http.StatusOK},
		{"missing body", checksum, "", http.StatusOK},
		{"failed response", checksum, body, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "kubectl")
			if err := downloadNativeKubectl(context.Background(), server.Client(), server.URL, tc.digest, path); err == nil {
				t.Fatal("published an unverified native client")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("unverified native client remained available")
			}
		})
	}
}

func TestNativeKubectlDownloadPublishesOnlyVerifiedClient(t *testing.T) {
	const body = "synthetic native client bytes"
	const checksum = "d8ac7208be2eb55edb3b7c667ad310ac91db5859d2908e912e973ca71f249823"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "kubectl")
	if err := downloadNativeKubectl(context.Background(), server.Client(), server.URL, checksum, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != body {
		t.Fatal("qualified native client bytes changed")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("qualified native client is not privately executable")
	}
}
