package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the compiled entrypoint. The only external executable available to that
// process is a non-networking git sentinel that refuses the initial source
// read. No real cluster, host client or operator credential can be reached.
func TestNativeSeedCLIAdmitsOnlyItsOwnFirstAttempt(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "migrate-longhorn-v2")
	build := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the native entrypoint: %v\n%s", err, output)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FIXTURE_MARKER\"\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cases := map[string][2]string{
		"": {}, "branch": {"GITHUB_REF", "refs/heads/candidate"}, "retry": {"GITHUB_RUN_ATTEMPT", "2"},
		"workflow":   {"GITHUB_WORKFLOW_REF", "devantler-tech/platform/.github/workflows/other.yaml@refs/heads/main"},
		"repository": {"GITHUB_REPOSITORY", "other/platform"}, "runner": {"RUNNER_ENVIRONMENT", "self-hosted"},
		"confirmation": {"LONGHORN_V2_CONFIRM", "benchmark-longhorn-storage"}, "node": {"LONGHORN_V2_NODE", "prod-worker-1"},
		"database": {"LONGHORN_V2_DATABASE", "wedding-db"}, "source": {"GITHUB_SHA", "abbreviated"}, "run": {"GITHUB_RUN_ID", "not-a-run"},
	}
	for fault, override := range cases {
		t.Run(fault, func(t *testing.T) {
			env := nativeSeedDispatch()
			if fault != "" {
				env[override[0]] = override[1]
			}
			marker := filepath.Join(t.TempDir(), "called")
			cmd := exec.CommandContext(t.Context(), binary)
			cmd.Env = []string{"PATH=" + bin, "HOME=" + dir, "KUBECONFIG=" + filepath.Join(dir, "absent"), "FIXTURE_MARKER=" + marker}
			for key, val := range env {
				cmd.Env = append(cmd.Env, key+"="+val)
			}
			output, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(output), "SOURCE_CLEANED") {
				t.Fatal("unqualified compiled operation reported success:", string(output))
			}
			called, readErr := os.ReadFile(marker)
			if fault == "" {
				if readErr != nil || string(called) != "ls-remote --exit-code https://github.com/devantler-tech/platform.git refs/heads/main\n" ||
					!strings.Contains(string(output), "Disposable canary source held:") {
					t.Fatal("valid source grant did not enter the bounded seed procedure:", readErr, string(output), string(called))
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("unadmitted compiled operation invoked an external command:", fault, readErr, string(called))
			}
		})
	}
}
