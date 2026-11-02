package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Invalid dispatches must finish before even reading a Kubernetes resource.
func TestDriverRefusesUnsafeDispatch(t *testing.T) {
	if _, err := os.ReadFile("main.go"); err != nil {
		t.Fatalf("the guarded production probe driver is missing: %v", err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "probe")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build driver: %v %s", err, output)
	}
	marker := filepath.Join(dir, "kubernetes-was-called")
	fake := "#!/bin/sh\nprintf called > \"$PROBE_TEST_MARKER\"\nexit 91\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, value string }{
		{"GITHUB_REPOSITORY", "other/project"},
		{"GITHUB_ACTOR", "other"},
		{"GITHUB_REF", "refs/heads/candidate"},
		{"GITHUB_EVENT_NAME", "push"},
		{"GITHUB_RUN_ATTEMPT", "2"},
		{"GITHUB_RUN_ID", "invalid"},
		{"GITHUB_SHA", "invalid"},
		{"EMAIL_PROVIDER_CONFIRM", "wrong"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			values := map[string]string{
				"GITHUB_REPOSITORY": "devantler-tech/platform", "GITHUB_ACTOR": "devantler",
				"GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch",
				"GITHUB_RUN_ATTEMPT": "1", "GITHUB_RUN_ID": "123456789", "GITHUB_SHA": strings.Repeat("a", 40),
				"EMAIL_PROVIDER_CONFIRM": "verify-email-provider-smtp",
			}
			values[tc.key] = tc.value
			cmd := exec.Command(binary)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "PROBE_TEST_MARKER=" + marker}
			for key, value := range values {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), "EMAIL_PROVIDER_SMTP=UNVERIFIED stage=dispatch") {
				t.Fatalf("unsafe dispatch did not refuse: %v %s", err, output)
			}
			if _, err = os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("unsafe dispatch reached Kubernetes")
			}
		})
	}
}
