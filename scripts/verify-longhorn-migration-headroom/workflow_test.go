package main

import (
	"os"
	"os/exec"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDispatchConfirmationRefusesRerunAndOtherRefs(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/observe-longhorn-migration-headroom.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	steps := workflow.Jobs["observe"].Steps
	if len(steps) == 0 || steps[0].Run == "" {
		t.Fatal("missing pre-checkout dispatch authorization")
	}
	for _, tc := range []struct {
		name, ref, attempt, confirm string
		ok                          bool
	}{
		{"confirmed main", "refs/heads/main", "1", "observe-longhorn-migration-headroom", true},
		{"confirmed account reconciliation", "refs/heads/main", "1", "observe-longhorn-provider-account", true},
		{"foreign ref", "refs/heads/feature", "1", "observe-longhorn-migration-headroom", false},
		{"rerun", "refs/heads/main", "2", "observe-longhorn-migration-headroom", false},
		{"account reconciliation rerun", "refs/heads/main", "2", "observe-longhorn-provider-account", false},
		{"missing confirmation", "refs/heads/main", "1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", steps[0].Run)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DISPATCH_REF=" + tc.ref, "GITHUB_RUN_ATTEMPT=" + tc.attempt, "LONGHORN_HEADROOM_CONFIRM=" + tc.confirm}
			err := cmd.Run()
			if (err == nil) != tc.ok {
				t.Fatalf("dispatch authorization result=%v want pass=%v", err, tc.ok)
			}
		})
	}
}
