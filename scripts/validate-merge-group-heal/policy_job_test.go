package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestPolicyJobRegressionRejectsSchedulingBypasses runs the real shell regression
// against the workflow and unsafe mutations, not a copy of its admission logic.
func TestPolicyJobRegressionRejectsSchedulingBypasses(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../tests/test-policy-ci-job.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "preserved scheduling"},
		{name: "missing job", mutate: func(jobs map[string]any) { delete(jobs, "validate-cluster-policies") }},
		{name: "missing classification dependency", mutate: func(jobs map[string]any) {
			delete(jobs["validate-cluster-policies"].(map[string]any), "needs")
		}},
		{name: "serialized policy family", mutate: func(jobs map[string]any) {
			jobs["validate-cluster-policies"].(map[string]any)["needs"] = []string{"changes", "validate"}
		}},
		{name: "unconditional policy family", mutate: func(jobs map[string]any) {
			delete(jobs["validate-cluster-policies"].(map[string]any), "if")
		}},
		{name: "wrong path selection", mutate: func(jobs map[string]any) {
			jobs["validate-cluster-policies"].(map[string]any)["if"] = "github.event_name == 'pull_request' && needs.changes.outputs.talos == 'true'"
		}},
		{name: "wrong event selection", mutate: func(jobs map[string]any) {
			jobs["validate-cluster-policies"].(map[string]any)["if"] = "github.event_name == 'merge_group'"
		}},
		{name: "disabled policy family", mutate: func(jobs map[string]any) {
			jobs["validate-cluster-policies"].(map[string]any)["if"] = "false"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var candidate map[string]any
			if err := yaml.Unmarshal(workflow, &candidate); err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(candidate["jobs"].(map[string]any))
			}
			data, err := yaml.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			scriptPath := filepath.Join(root, "scripts", "tests", "test-policy-ci-job.sh")
			workflowPath := filepath.Join(root, ".github", "workflows", "ci.yaml")
			for path, content := range map[string][]byte{scriptPath: script, workflowPath: data} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "bash", scriptPath)
			command.WaitDelay = 2 * time.Second
			output, err := command.CombinedOutput()
			if tc.mutate == nil {
				if err != nil {
					t.Fatalf("preserved policy scheduling rejected: %v\n%s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), "FAIL: independent policy job scheduling changed") {
				t.Fatalf("unsafe scheduling did not fail at its admission boundary: error=%v\n%s", err, output)
			}
		})
	}
}
