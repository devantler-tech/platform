package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowModeGuardAndDispatchStaySeparate(t *testing.T) {
	workflow, err := readYAMLFile(filepath.Join("..", "..", ".github", "workflows", "verify-arc-app-identity.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	job := object(object(workflow["jobs"])["identity"])
	steps, _ := job["steps"].([]any)
	var guard, dispatch string
	for _, entry := range steps {
		step := object(entry)
		switch stringValue(step["name"]) {
		case "Require protected invocation":
			guard = stringValue(step["run"])
		case "Verify selected prerequisite without activation":
			dispatch = stringValue(step["run"])
		}
	}
	if guard == "" || !strings.Contains(dispatch, "--verify") || !strings.Contains(dispatch, "--transport") {
		t.Fatal("workflow guard or mode dispatch step not found")
	}
	for _, tc := range []struct {
		mode, confirm, ref, attempt, argument string
		allowed                               bool
	}{
		{"identity", "verify-arc-app-identity", "refs/heads/main", "1", "--verify", true},
		{"transport", "verify-arc-app-transport", "refs/heads/main", "1", "--transport", true},
		{"transport", "verify-arc-app-identity", "refs/heads/main", "1", "", false},
		{"identity", "verify-arc-app-transport", "refs/heads/main", "1", "", false},
		{"other", "verify-arc-app-transport", "refs/heads/main", "1", "", false},
		{"transport", "verify-arc-app-transport", "refs/heads/codex/test", "1", "", false},
		{"transport", "verify-arc-app-transport", "refs/heads/main", "2", "", false},
		{"transport; touch injected", "verify-arc-app-transport", "refs/heads/main", "1", "", false},
	} {
		t.Run(tc.mode+"/"+tc.confirm+"/"+tc.attempt+"/"+tc.ref, func(t *testing.T) {
			root := t.TempDir()
			binary := filepath.Join(root, "verify-arc-app-identity")
			if err := os.WriteFile(binary, []byte("#!/bin/bash\nprintf '%s\\n' \"$@\" >\"${RUNNER_TEMP}/arguments\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("bash", "-c", guard+"\n"+dispatch)
			command.Dir = root
			command.Env = append(os.Environ(), "MODE="+tc.mode, "CONFIRM="+tc.confirm, "DISPATCH_REF="+tc.ref, "GITHUB_RUN_ATTEMPT="+tc.attempt, "RUNNER_TEMP="+root)
			err := command.Run()
			arguments, readErr := os.ReadFile(filepath.Join(root, "arguments"))
			if tc.allowed {
				if err != nil || readErr != nil || string(arguments) != tc.argument+"\n" {
					t.Fatalf("mode dispatch failed: err=%v, arguments=%q", err, arguments)
				}
			} else if err == nil || !os.IsNotExist(readErr) {
				t.Fatal("invalid protected mode reached the verifier")
			}
		})
	}
}
