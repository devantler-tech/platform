package contractprobeimage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPublishedProbeImage(t *testing.T) {
	t.Parallel()
	if os.Getenv("CONTRACT_PROBE_IMAGE_NATIVE_TEST") != "1" {
		t.Skip("native published-image proof is opt-in")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("the selected native proof requires Linux/amd64")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	declaration := filepath.Join("..", "..", "..", "k8s", "bases", "apps", "data-product-controller", "deployment-contract-probe.yaml")
	data := runNative(t, ctx, "read declared image", "yq", "-o=json", ".spec.template.spec.containers", declaration)
	var containers []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	}
	if err := json.Unmarshal(data, &containers); err != nil || len(containers) != 1 || containers[0].Name != "contract-probe" {
		t.Fatal("ambiguous probe declaration")
	}
	image := containers[0].Image
	approved := regexp.MustCompile(`^ghcr\.io/devantler-tech/(data-product-controller|khoist):[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}$`)
	if !approved.MatchString(image) {
		t.Fatal("probe image is not an approved immutable first-party reference")
	}

	scratch := t.TempDir()
	client := filepath.Join(scratch, "management-proof.test")
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", client, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated management client: %v\n%s", err, output)
	}
	if err := os.Chmod(client, 0o555); err != nil {
		t.Fatal("could not make the isolated client executable")
	}
	runNative(t, ctx, "pull declared image", "docker", "pull", "--platform", "linux/amd64", image)
	name := fmt.Sprintf("contract-probe-proof-%d-%d", os.Getpid(), time.Now().UnixNano())
	// Register before create: a partially completed Docker operation may still
	// leave this unique, test-owned container behind.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		output, err := exec.CommandContext(cleanupCtx, "docker", "rm", "-fv", name).CombinedOutput()
		if err != nil {
			t.Errorf("native probe cleanup was not confirmed: %v\n%s", err, output)
		}
	})
	runNative(t, ctx, "create isolated probe", "docker", "create", "--name", name,
		"--platform", "linux/amd64", "--network", "none", "--read-only", "--user", "65532:65532",
		"--cpus", "1", "--memory", "64m", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--mount", "type=bind,source="+client+",target=/management-proof.test,readonly",
		"--env", "CONTRACT_PROBE_URL=https://contract-probe.invalid/openapi.json",
		"--env", "CONTRACT_READINESS_ENABLED=true", "--entrypoint", "/contract-probe", image)
	runNative(t, ctx, "start isolated probe", "docker", "start", name)
	proof := runNative(t, ctx, "exercise published diagnostics", "docker", "exec",
		"--env", "CONTRACT_PROBE_IMAGE_INSIDE=1", name, "/management-proof.test",
		"-test.run", "^TestInsidePublishedProbe$", "-test.v", "-test.timeout", "30s")
	if !strings.Contains(string(proof), "--- PASS: TestInsidePublishedProbe") {
		t.Fatal("the native management proof did not execute and pass")
	}
	t.Log("published probe started without privileges or networking; one failed request and two passive metric scrapes passed")
}

// runNative keeps failures attributable to the fixed fixture stage. No platform
// credentials or workload response data are supplied to these commands.
func runNative(t *testing.T, ctx context.Context, stage, program string, args ...string) []byte {
	t.Helper()
	output, err := exec.CommandContext(ctx, program, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", stage, err, output)
	}
	return output
}

func TestInsidePublishedProbe(t *testing.T) {
	t.Parallel()
	if os.Getenv("CONTRACT_PROBE_IMAGE_INSIDE") != "1" {
		t.Skip("runs only inside the isolated published probe")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	startup, startupCancel := context.WithTimeout(ctx, 5*time.Second)
	defer startupCancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		connection, err := dialer.DialContext(startup, "tcp", "127.0.0.1:8081")
		if err == nil {
			_ = connection.Close()
			break
		}
		select {
		case <-startup.Done():
			t.Fatal("published management listener did not start")
		case <-ticker.C:
		}
	}
	if err := verifyManagement(ctx, "http://127.0.0.1:8081"); err != nil {
		// The checker returns only fixed errors, never metric/response contents.
		t.Fatal(strings.TrimSpace(err.Error()))
	}
}
