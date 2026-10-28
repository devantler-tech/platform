package cleanup_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

const reader = `#!/bin/bash
set -euo pipefail
printf 'attempt\n' >> "$FIXTURE_CALLS"
case "$*" in
  '--context admin@prod --request-timeout=20s -n flux-system get secret variables-cluster -o json')
    case "$FIXTURE_CASE" in
      unavailable) echo 'fixture-private-value' >&2; exit 1 ;;
      malformed) echo 'fixture-private-value'; exit 0 ;;
      wrong-original) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"another","namespace":"flux-system"},"data":{}}'; exit 0 ;;
      obsolete-present) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-cluster","namespace":"flux-system"},"data":{"alertmanager_heartbeat_url":"fixture-private-value"}}'; exit 0 ;;
    esac
    echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-cluster","namespace":"flux-system"},"data":{"another":"c3ludGhldGlj"}}'
    ;;
  '--context admin@prod --request-timeout=20s -n flux-system get secret variables-heartbeat -o json')
    case "$FIXTURE_CASE" in
      empty-replacement) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-heartbeat","namespace":"flux-system"},"data":{"alertmanager_heartbeat_url":""}}' ;;
      wrong-namespace) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-heartbeat","namespace":"default"},"data":{"alertmanager_heartbeat_url":"c3ludGhldGlj"}}' ;;
      extra-field) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-heartbeat","namespace":"flux-system"},"data":{"alertmanager_heartbeat_url":"c3ludGhldGlj","another":"c3ludGhldGlj"}}' ;;
      *) echo '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"variables-heartbeat","namespace":"flux-system"},"data":{"alertmanager_heartbeat_url":"c3ludGhldGlj"}}' ;;
    esac
    ;;
  *) echo 'fixture-private-value'; exit 99 ;;
esac
`

func TestRuntimeReceiptFailsClosedWithoutDisclosingInputs(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		calls int
	}{
		{"success", 0, 2}, {"inherited-trace", 0, 2},
		{"wrong-ref", 2, 0}, {"wrong-event", 2, 0}, {"wrong-confirmation", 2, 0},
		{"missing-tool", 2, 0}, {"unavailable", 2, 1}, {"malformed", 2, 1},
		{"wrong-original", 2, 1}, {"obsolete-present", 2, 1},
		{"empty-replacement", 2, 2}, {"wrong-namespace", 2, 2}, {"extra-field", 2, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			private := t.TempDir()
			bin := filepath.Join(private, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(reader), 0700); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(private, "calls")
			if err := os.WriteFile(calls, nil, 0600); err != nil {
				t.Fatal(err)
			}
			path := bin + string(os.PathListSeparator) + os.Getenv("PATH")
			ref, event, confirmation := "refs/heads/main", "workflow_dispatch", "verify-heartbeat-cleanup"
			switch tc.name {
			case "wrong-ref":
				ref = "refs/heads/unreviewed"
			case "wrong-event":
				event = "pull_request"
			case "wrong-confirmation":
				confirmation = "verify-heartbeat-cleanup\"; exit 0; #"
			case "missing-tool":
				path = bin
			}
			args := []string{filepath.Join(root(), "scripts/verify-heartbeat-cleanup.sh")}
			if tc.name == "inherited-trace" {
				args = append([]string{"-x"}, args...)
			}
			cmd := exec.Command("/bin/bash", args...)
			cmd.Env = []string{"PATH=" + path, "GITHUB_REF=" + ref, "GITHUB_EVENT_NAME=" + event,
				"HEARTBEAT_CLEANUP_CONFIRM=" + confirmation, "FIXTURE_CASE=" + tc.name, "FIXTURE_CALLS=" + calls}
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code {
				t.Fatalf("expected exit %d, got %d: %s", tc.code, code, out)
			}
			verdict := "HEARTBEAT_CLEANUP=UNVERIFIED\n"
			if tc.code == 0 {
				verdict = "HEARTBEAT_CLEANUP=VERIFIED\n"
			}
			visible := strings.TrimPrefix(string(out), "+ set +x\n")
			if visible != verdict || strings.Contains(string(out), "fixture-private-value") || strings.Contains(string(out), "c3ludGhldGlj") {
				t.Fatalf("missing fixed verdict or private input reached output: %s", out)
			}
			data, err := os.ReadFile(calls)
			if err != nil || strings.Count(string(data), "attempt\n") != tc.calls {
				t.Fatalf("unexpected read attempts: %q, %v", data, err)
			}
		})
	}
}

func TestWorkflowRestrictsTheProductionRead(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root(), ".github/workflows/verify-heartbeat-cleanup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if !safeWorkflow(workflow) {
		t.Fatal("workflow must guard main-only manual dispatch before checkout and production credentials")
	}
	jobs := workflow["jobs"].(map[string]any)
	steps := jobs["verify"].(map[string]any)["steps"].([]any)
	guard := steps[0].(map[string]any)["run"].(string)
	for _, invalid := range []string{"", "ref", "event", "confirmation"} {
		ref, event, confirmation := "refs/heads/main", "workflow_dispatch", "verify-heartbeat-cleanup"
		switch invalid {
		case "ref":
			ref = "refs/heads/unreviewed"
		case "event":
			event = "pull_request"
		case "confirmation":
			confirmation = "verify-heartbeat-cleanup\"; exit 0; #"
		}
		cmd := exec.Command("/bin/bash", "-c", guard)
		cmd.Env = []string{"GITHUB_REF=" + ref, "GITHUB_EVENT_NAME=" + event, "CONFIRM=" + confirmation}
		if out, err := cmd.CombinedOutput(); (err == nil) != (invalid == "") {
			t.Fatalf("inline guard %q: %v: %s", invalid, err, out)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(w map[string]any) { w["permissions"] = map[string]any{"contents": "write"} },
		func(w map[string]any) { w["on"].(map[string]any)["push"] = map[string]any{} },
		func(w map[string]any) { w["env"] = map[string]any{"KUBE_CONFIG": "premature"} },
		func(w map[string]any) { w["jobs"].(map[string]any)["verify"].(map[string]any)["steps"] = steps[1:] },
		func(w map[string]any) {
			w["jobs"].(map[string]any)["verify"].(map[string]any)["steps"].([]any)[1].(map[string]any)["with"].(map[string]any)["token"] = "provider-key"
		},
	} {
		encoded, err := json.Marshal(workflow)
		if err != nil {
			t.Fatal(err)
		}
		var mutated map[string]any
		if err := json.Unmarshal(encoded, &mutated); err != nil {
			t.Fatal(err)
		}
		mutate(mutated)
		if safeWorkflow(mutated) {
			t.Fatal("unsafe workflow mutation accepted")
		}
	}
}

func safeWorkflow(w map[string]any) bool {
	encoded, err := json.Marshal(w)
	if err != nil {
		return false
	}
	text := string(encoded)
	if strings.Count(text, "${{ secrets.KUBE_CONFIG }}") != 1 || strings.Count(text, "${{ secrets.HCLOUD_TOKEN }}") != 1 {
		return false
	}
	if p, ok := w["permissions"].(map[string]any); !ok || len(p) != 0 || w["env"] != nil || w["defaults"] != nil {
		return false
	}
	on, ok := w["on"].(map[string]any)
	if !ok || len(on) != 1 || on["workflow_dispatch"] == nil {
		return false
	}
	dispatch, ok := on["workflow_dispatch"].(map[string]any)
	if !ok {
		return false
	}
	inputs, ok := dispatch["inputs"].(map[string]any)
	if !ok || len(inputs) != 1 {
		return false
	}
	confirmation, ok := inputs["confirm"].(map[string]any)
	if !ok || confirmation["required"] != true || confirmation["type"] != "string" {
		return false
	}
	jobs, ok := w["jobs"].(map[string]any)
	if !ok || len(jobs) != 1 {
		return false
	}
	job, ok := jobs["verify"].(map[string]any)
	if !ok || job["environment"] != "prod" || job["runs-on"] != "ubuntu-latest" || job["env"] != nil || job["defaults"] != nil || job["if"] != nil || job["continue-on-error"] != nil {
		return false
	}
	switch job["timeout-minutes"] {
	case 5, float64(5):
	default:
		return false
	}
	permission, ok := job["permissions"].(map[string]any)
	if !ok || len(permission) != 1 || permission["contents"] != "read" {
		return false
	}
	steps, ok := job["steps"].([]any)
	if !ok || len(steps) != 4 {
		return false
	}
	for i, item := range steps {
		step, ok := item.(map[string]any)
		if !ok || step["if"] != nil || step["continue-on-error"] != nil {
			return false
		}
		if i < 3 {
			value, err := json.Marshal(step)
			if err != nil || strings.Contains(string(value), "secrets.") {
				return false
			}
		}
	}
	guard, ok := steps[0].(map[string]any)["run"].(string)
	if !ok || strings.Contains(guard, "${{") || !strings.Contains(guard, "GITHUB_EVENT_NAME") || !strings.Contains(guard, "GITHUB_REF") || !strings.Contains(guard, "verify-heartbeat-cleanup") {
		return false
	}
	guardEnv, ok := steps[0].(map[string]any)["env"].(map[string]any)
	if !ok || len(guardEnv) != 1 || guardEnv["CONFIRM"] != "${{ inputs.confirm }}" {
		return false
	}
	proof := steps[3].(map[string]any)
	proofEnv, ok := proof["env"].(map[string]any)
	if !ok || len(proofEnv) != 3 || proofEnv["KUBE_CONFIG"] != "${{ secrets.KUBE_CONFIG }}" || proofEnv["HCLOUD_TOKEN"] != "${{ secrets.HCLOUD_TOKEN }}" || proofEnv["HEARTBEAT_CLEANUP_CONFIRM"] != "${{ inputs.confirm }}" {
		return false
	}
	proofRun, ok := proof["run"].(string)
	if !ok || strings.Contains(proofRun, "${{") || !strings.HasSuffix(strings.TrimSpace(proofRun), "bash scripts/verify-heartbeat-cleanup.sh") {
		return false
	}
	checkout := steps[1].(map[string]any)
	with, ok := checkout["with"].(map[string]any)
	return ok && len(with) == 2 && checkout["uses"] == "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1" && with["ref"] == "${{ github.sha }}" && with["persist-credentials"] == false
}
