package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExecutorDiscardsResponseWithoutNaturalEOF(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("scripts", 0700); err != nil {
		t.Fatal(err)
	}
	// The command exits with an ordinary denial, but its child retains the pipe.
	// Cmd.Wait alone can return ExitError(1) after forcibly closing that pipe.
	const script = "#!/bin/bash\nsleep 60 &\nprintf '%s' $! > child.pid\nprintf '%s' 'partial policy denial'\nexit 1\n"
	if err := os.WriteFile(filepath.Join("scripts", "verify-prod-recovery-source.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		raw, err := os.ReadFile("child.pid")
		if err != nil {
			t.Error(err)
			return
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil || pid <= 0 {
			t.Errorf("invalid owned child PID: %q", raw)
			return
		}
		child, err := os.FindProcess(pid)
		if err == nil {
			_ = child.Kill()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	raw, err := execute(ctx, "bash", []string{"./scripts/verify-prod-recovery-source.sh", strings.Repeat("a", 40)}, nil, nil)
	if err == nil || len(raw) != 0 {
		t.Fatalf("incomplete response retained: bytes=%d error=%v", len(raw), err)
	}
}

func TestExecutorRetainsCompleteExitOneResponse(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("scripts", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("scripts/verify-prod-recovery-source.sh", []byte("printf '%s' 'complete denial'\nexit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := execute(context.Background(), "bash", []string{"./scripts/verify-prod-recovery-source.sh", strings.Repeat("a", 40)}, nil, nil)
	if err == nil || string(raw) != "complete denial" {
		t.Fatalf("complete response lost: bytes=%d error=%v", len(raw), err)
	}
}

func TestTruncatedCommandOutputCannotSupplyAnAdmissionResult(t *testing.T) {
	var output boundedCommandOutput
	if _, err := output.Write([]byte("partial policy denial")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", 2*1024*1024+1))); err == nil {
		t.Fatal("oversized output was accepted")
	}
	if raw, truncated := output.result(); !truncated || len(raw) != 0 {
		t.Fatal("truncated command returned partial evidence")
	}
}

func TestExecutorAllowsOnlyFixedReadsAndServerDryRun(t *testing.T) {
	read := []string{"--context", "admin@prod", "--request-timeout=15s", "get", "--raw", "/api/v1/namespaces/kyverno/secrets/ghcr-auth"}
	if !commandAllowed("kubectl", read, nil, nil) {
		t.Fatal("fixed authoritative read rejected")
	}
	pod, _, err := controlPod("123", "unsigned")
	if err != nil {
		t.Fatal(err)
	}
	dry := []string{"--context", "admin@prod", "--request-timeout=45s", "create", "--dry-run=server", "--validate=strict", "--namespace", "default", "--filename", "-", "-o", "json"}
	if !commandAllowed("kubectl", dry, pod, nil) {
		t.Fatal("fixed server dry-run rejected")
	}
	for _, args := range [][]string{
		{"--context", "admin@prod", "create", "--namespace", "default", "-f", "-"},
		{"--context", "admin@prod", "--request-timeout=15s", "get", "--raw", "/api/v1/secrets"},
		{"--context", "foreign", "--request-timeout=15s", "get", "--raw", "/api/v1/namespaces/kyverno/secrets/ghcr-auth"},
	} {
		if commandAllowed("kubectl", args, pod, nil) {
			t.Fatal("out-of-scope command accepted")
		}
	}
	if _, err := execute(context.Background(), "bash", []string{"-c", "exit 0"}, nil, nil); err == nil {
		t.Fatal("executor ran an unapproved command")
	}
}

func TestRegistryPasswordNeverBecomesAnArgument(t *testing.T) {
	env := map[string]string{"REGISTRY_USERNAME": "probe", "REGISTRY_PASSWORD": "private-test-token", "TMPDIR": "/dev/shm/kyverno-cache-recovery.abc123"}
	if !commandAllowed("bash", []string{"./scripts/verify-image-unsigned.sh", "--image", unsignedImage}, nil, env) {
		t.Fatal("fixed in-memory registry precheck rejected")
	}
	if commandAllowed("bash", []string{"./scripts/verify-image-unsigned.sh", "--password", "private-test-token"}, nil, env) {
		t.Fatal("registry password argument accepted")
	}
	if !commandAllowed("bash", []string{"./scripts/verify-prod-recovery-source.sh", strings.Repeat("a", 40)}, nil, nil) {
		t.Fatal("fixed current-main guard rejected")
	}
}
