package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../.."
const metricsScript = "scripts/ksail-arc-job-metrics.sh"

func TestJobMetricsEntrypointRejectsAnAlternateCgroup(t *testing.T) {
	script := filepath.Join(repoRoot, metricsScript)
	output, err := exec.Command("bash", script, t.TempDir()).CombinedOutput()
	if err == nil || strings.Contains(string(output), "KSail ARC whole-job cgroup:") {
		t.Fatalf("accepted a substituted production cgroup: %s", output)
	}
}

func TestWholeJobCgroupMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name, limit, peak, events string
		ok                        bool
	}{
		{"complete", "15032385536", "9876543210", "low 0\nhigh 0\nmax 3\noom 0\noom_kill 0\noom_group_kill 0\n", true},
		{"missing limit", "", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"unbounded", "max", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"wrong cgroup", "17179869184", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"missing peak", "15032385536", "", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"zero peak", "15032385536", "0", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"exceeded", "15032385536", "15032385537", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"malformed peak", "15032385536", "1\n2", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"noncanonical peak", "15032385536", "001", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"overflow", "15032385536", "99999999999999999999999", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"missing events", "15032385536", "10", "", false},
		{"missing OOM", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom_kill 0\noom_group_kill 0\n", false},
		{"duplicate event", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 0\noom 0\noom_kill 0\noom_group_kill 0\n", false},
		{"unknown event", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\nsecret 0\n", false},
		{"malformed event", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom nope\noom_kill 0\noom_group_kill 0\n", false},
		{"extra event data", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 0 extra\noom_kill 0\noom_group_kill 0\n", false},
		{"OOM", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 1\noom_kill 0\noom_group_kill 0\n", false},
		{"OOM kill", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 1\noom_group_kill 0\n", false},
		{"OOM group kill", "15032385536", "10", "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\noom_group_kill 1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, value := range map[string]string{"memory.max": tc.limit, "memory.peak": tc.peak, "memory.events": tc.events} {
				if value != "" {
					if err := os.WriteFile(filepath.Join(root, name), []byte(strings.TrimSuffix(value, "\n")+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			script, err := filepath.Abs(filepath.Join(repoRoot, metricsScript))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", `source "$1"; arc_job_metrics "$2"`, "metrics-test", script, root)
			cmd.Env = append(os.Environ(), "ACTIONS_RUNNER_INPUT_JITCONFIG=must-not-be-read", "SECRET_CANARY=must-not-be-read")
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("success=%t, want %t: %s", err == nil, tc.ok, output)
			}
			if strings.Contains(string(output), "must-not-be-read") {
				t.Fatal("measurement exposed environment data")
			}
			if !tc.ok {
				if strings.Contains(string(output), "KSail ARC whole-job cgroup:") {
					t.Fatal("failed measurement emitted a success receipt")
				}
				return
			}
			var got struct {
				SchemaVersion   int    `json:"schemaVersion"`
				MemoryMaxBytes  uint64 `json:"memoryMaxBytes"`
				MemoryPeakBytes uint64 `json:"memoryPeakBytes"`
				LimitEvents     uint64 `json:"limitEvents"`
				OOMEvents       uint64 `json:"oomEvents"`
				OOMKills        uint64 `json:"oomKills"`
				OOMGroupKills   uint64 `json:"oomGroupKills"`
			}
			decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(string(output), "KSail ARC whole-job cgroup: ")))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != 1 || got.MemoryMaxBytes != 15032385536 || got.MemoryPeakBytes != 9876543210 ||
				got.LimitEvents != 3 || got.OOMEvents != 0 || got.OOMKills != 0 || got.OOMGroupKills != 0 {
				t.Fatalf("wrong measurement: %+v", got)
			}
		})
	}
}
