package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPinnedImageShellProbeRequiresTheOriginalStoredContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe")
	run := func(phase string) (string, error) {
		pod := podManifest("email-storage-canary-123", phase, "123")
		argv := items(child(pod, "spec"), "containers")[0]["command"].([]any)
		command := strings.ReplaceAll(argv[3].(string), "/data/probe", strconv.Quote(path))
		result, err := exec.Command("/bin/sh", "-eu", "-c", command).CombinedOutput()
		return string(result), err
	}
	for _, phase := range []string{"write", "read", "recovery"} {
		out, err := run(phase)
		if err != nil || out != "EMAIL_VOLUME_PROBE=PASS phase="+phase+"\n" {
			t.Fatalf("ordinary storage probe failed: %q", out)
		}
	}
	if os.WriteFile(path, []byte("different contents"), 0600) != nil {
		t.Fatal("fixture")
	}
	if out, err := run("read"); err == nil || strings.Contains(out, "PASS") {
		t.Fatal("corrupt stored content produced a successful probe")
	}
}
