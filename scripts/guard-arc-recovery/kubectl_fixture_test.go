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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const maxKubectlDownload = int64(128 << 20)

type kubectlPin struct {
	version, checksum string
}

func kubectlFixturePin(authorization []nativeCIStep, platform string) (kubectlPin, error) {
	var pin kubectlPin
	for _, step := range authorization {
		if step.Env["KUBECTL_VERSION"] != "" && step.Env["KUBECTL_SHA256"] != "" {
			pin = kubectlPin{step.Env["KUBECTL_VERSION"], step.Env["KUBECTL_SHA256"]}
			break
		}
	}
	checksum, err := hex.DecodeString(pin.checksum)
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(pin.version) || err != nil || len(checksum) != sha256.Size {
		return kubectlPin{}, errors.New("production authorization declares no valid kubectl version/SHA256 pair")
	}
	switch platform {
	case "linux/amd64":
		return pin, nil
	case "darwin/arm64":
		if pin.version == "v1.36.2" {
			// https://dl.k8s.io/release/v1.36.2/bin/darwin/arm64/kubectl.sha256
			pin.checksum = "4408c85c83fd3a31adaa555bdf3c7a6c81f74b19449a9060ba31ab91926f023d"
			return pin, nil
		}
	}
	return kubectlPin{}, fmt.Errorf("no approved kubectl checksum for %s on %s", pin.version, platform)
}

// Prepare the same approved client independently of runner-image tools. Setup
// has its own bound; the proxy's existing readiness deadline starts afterward.
func privateNativeKubectl(t *testing.T) string {
	t.Helper()
	_, authorization := nativeCISteps(t)
	platform := runtime.GOOS + "/" + runtime.GOARCH
	pin, err := kubectlFixturePin(authorization, platform)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	binary, err := filepath.Abs(filepath.Join(directory, "kubectl"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	url := "https://dl.k8s.io/release/" + pin.version + "/bin/" + platform + "/kubectl"
	if err := downloadPinnedKubectl(ctx, &http.Client{}, url, pin.checksum, binary); err != nil {
		t.Fatalf("prepare approved private kubectl: %v", err)
	}
	command := exec.CommandContext(ctx, binary, "version", "--client=true", "--output=json")
	command.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(directory, "unused-kubeconfig"))
	command.WaitDelay = 2 * time.Second
	data, err := command.Output()
	if err != nil {
		t.Fatalf("initialize approved private kubectl: %v", err)
	}
	var version struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		t.Fatalf("decode private kubectl client version: %v", err)
	}
	if version.ClientVersion.GitVersion != pin.version {
		t.Fatalf("private kubectl reported version %q, want %s", version.ClientVersion.GitVersion, pin.version)
	}
	t.Logf("initialized SHA256-verified kubectl %s for %s", pin.version, platform)
	return binary
}

func downloadPinnedKubectl(ctx context.Context, client *http.Client, url, checksum, destination string) (err error) {
	expected, err := hex.DecodeString(checksum)
	if err != nil || len(expected) != sha256.Size || !filepath.IsAbs(destination) {
		return errors.New("kubectl fixture requires an absolute destination and SHA256 pin")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("prepare kubectl download: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download kubectl: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxKubectlDownload {
		return errors.New("kubectl download has an unsuccessful status or excessive size")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create private kubectl: %w", err)
	}
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(response.Body, maxKubectlDownload+1))
	if err != nil {
		return fmt.Errorf("read kubectl download: %w", err)
	}
	if written > maxKubectlDownload || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), checksum) {
		return errors.New("kubectl download exceeds its size bound or does not match the SHA256 pin")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close verified kubectl: %w", err)
	}
	if err := os.Chmod(destination, 0700); err != nil {
		return fmt.Errorf("make verified kubectl executable: %w", err)
	}
	return nil
}
