package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type fluxBridgeStep struct {
	Name     string            `yaml:"name"`
	If       string            `yaml:"if"`
	Run      string            `yaml:"run"`
	Continue any               `yaml:"continue-on-error"`
	Env      map[string]string `yaml:"env"`
}

type fluxBridgeJob struct {
	If          string            `yaml:"if"`
	Needs       []string          `yaml:"needs"`
	Continue    any               `yaml:"continue-on-error"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []fluxBridgeStep  `yaml:"steps"`
}

// readFluxBridgeJobs reads the workflow actually executed by Actions.
func readFluxBridgeJobs(t *testing.T) map[string]fluxBridgeJob {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]fluxBridgeJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs
}

// fluxBridgeBoundary keeps the original PR-only scope while removing the
// expensive family's ordering dependency on manifest validation.
func fluxBridgeBoundary(jobs map[string]fluxBridgeJob) error {
	bridge, exists := jobs["validate-flux-bridge"]
	if !exists {
		return fmt.Errorf("missing independent Flux bridge validation")
	}
	const selection = "github.event_name == 'pull_request' && (needs.changes.outputs.k8s == 'true' || needs.changes.outputs.bridge_validation == 'true')"
	if strings.Join(strings.Fields(bridge.If), " ") != selection ||
		len(bridge.Needs) != 1 || bridge.Needs[0] != "changes" || bridge.Continue != nil {
		return fmt.Errorf("bridge selection or independence changed")
	}
	if len(bridge.Permissions) != 1 || bridge.Permissions["contents"] != "read" {
		return fmt.Errorf("offline bridge validation must be read-only")
	}
	if strings.Join(strings.Fields(jobs["validate"].If), " ") != selection {
		return fmt.Errorf("manifest and bridge selection disagree")
	}
	for _, need := range jobs["validate"].Needs {
		if need == "validate-flux-bridge" {
			return fmt.Errorf("manifest validation waits for the bridge")
		}
	}
	for _, need := range jobs["deploy-prod"].Needs {
		if need == "validate-flux-bridge" {
			return fmt.Errorf("merge-group deployment depends on a skipped PR-only job")
		}
	}
	for _, need := range jobs["ci-required-checks"].Needs {
		if need == "validate-flux-bridge" {
			return nil
		}
	}
	return fmt.Errorf("required CI can bypass the bridge")
}

// TestFluxBridgeBoundaryRejectsBypasses exercises unsafe graph mutations against
// the real workflow, including the skipped-job production dependency trap.
func TestFluxBridgeBoundaryRejectsBypasses(t *testing.T) {
	jobs := readFluxBridgeJobs(t)
	for name, mutate := range map[string]func(map[string]fluxBridgeJob){
		"missing job": func(j map[string]fluxBridgeJob) { delete(j, "validate-flux-bridge") },
		"serialized bridge": func(j map[string]fluxBridgeJob) {
			job := j["validate-flux-bridge"]
			job.Needs = []string{"changes", "validate"}
			j["validate-flux-bridge"] = job
		},
		"serialized manifests": func(j map[string]fluxBridgeJob) {
			job := j["validate"]
			job.Needs = []string{"changes", "validate-flux-bridge"}
			j["validate"] = job
		},
		"conditional family lost": func(j map[string]fluxBridgeJob) {
			job := j["validate-flux-bridge"]
			job.If = "false"
			j["validate-flux-bridge"] = job
		},
		"bridge-only manifest coverage lost": func(j map[string]fluxBridgeJob) {
			job := j["validate"]
			job.If = "github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'"
			j["validate"] = job
		},
		"suppressed failure": func(j map[string]fluxBridgeJob) {
			job := j["validate-flux-bridge"]
			job.Continue = true
			j["validate-flux-bridge"] = job
		},
		"write authority": func(j map[string]fluxBridgeJob) {
			job := j["validate-flux-bridge"]
			job.Permissions = map[string]string{"contents": "write"}
			j["validate-flux-bridge"] = job
		},
		"deployment blocked on skipped job": func(j map[string]fluxBridgeJob) {
			job := j["deploy-prod"]
			job.Needs = append(append([]string(nil), job.Needs...), "validate-flux-bridge")
			j["deploy-prod"] = job
		},
		"required dependency removed": func(j map[string]fluxBridgeJob) {
			job := j["ci-required-checks"]
			job.Needs = []string{"changes", "validate"}
			j["ci-required-checks"] = job
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := make(map[string]fluxBridgeJob, len(jobs))
			for key, job := range jobs {
				candidate[key] = job
			}
			mutate(candidate)
			if err := fluxBridgeBoundary(candidate); err == nil {
				t.Fatal("accepted unsafe validation graph")
			}
		})
	}
}

// TestFluxBridgeIndependent checks the registered execution boundary and the
// complete package invocation, rather than a duplicated source-file inventory.
func TestFluxBridgeIndependent(t *testing.T) {
	jobs := readFluxBridgeJobs(t)
	if err := fluxBridgeBoundary(jobs); err != nil {
		t.Fatal(err)
	}
	const suite = "go test -timeout=20m ./scripts/tests/refresh-flux-ghcr-auth"
	invocations := 0
	for name, job := range jobs {
		for _, step := range job.Steps {
			for _, line := range strings.Split(step.Run, "\n") {
				if strings.TrimSpace(line) != suite {
					continue
				}
				invocations++
				if name != "validate-flux-bridge" || step.If != "" || step.Continue != nil {
					t.Fatal("bridge suite is serialized, conditional or failure-suppressed")
				}
			}
		}
	}
	if invocations != 1 {
		t.Fatalf("complete bridge invocations = %d, want exactly one", invocations)
	}
}

// TestFluxBridgeRequiredResult executes the actual final guard. Failure and
// missing evidence cannot masquerade as an intentional path/event exclusion.
func TestFluxBridgeRequiredResult(t *testing.T) {
	jobs := readFluxBridgeJobs(t)
	if err := fluxBridgeBoundary(jobs); err != nil {
		t.Fatal(err)
	}
	if jobs["ci-required-checks"].If != "always()" {
		t.Fatal("required evaluation must survive an unsuccessful dependency")
	}
	var guard *fluxBridgeStep
	for _, step := range jobs["ci-required-checks"].Steps {
		if step.Env["FLUX_BRIDGE_RESULT"] != "${{ needs.validate-flux-bridge.result }}" {
			continue
		}
		if guard != nil {
			t.Fatal("duplicate Flux bridge result guard")
		}
		copy := step
		guard = &copy
	}
	if guard == nil || guard.If != "" || guard.Continue != nil ||
		guard.Env["EVENT_NAME"] != "${{ github.event_name }}" ||
		guard.Env["K8S_CHANGED"] != "${{ needs.changes.outputs.k8s }}" ||
		guard.Env["BRIDGE_CHANGED"] != "${{ needs.changes.outputs.bridge_validation }}" {
		t.Fatal("missing unconditional result guard or real selection bindings")
	}
	for _, selection := range []struct {
		name, event, k8s, bridge string
		want                     string
	}{
		{"manifests", "pull_request", "true", "false", "success"},
		{"bridge-only", "pull_request", "false", "true", "success"},
		{"both", "pull_request", "true", "true", "success"},
		{"neither", "pull_request", "false", "false", "skipped"},
		{"merge-group", "merge_group", "true", "true", "skipped"},
		{"missing-manifests", "pull_request", "", "false", ""},
		{"missing-bridge", "pull_request", "true", "", ""},
		{"unknown-selection", "pull_request", "unknown", "true", ""},
		{"unknown-event", "push", "true", "true", ""},
	} {
		for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
			t.Run(selection.name+"/"+result, func(t *testing.T) {
				command := exec.Command("bash", "-e", "-o", "pipefail", "-c", guard.Run)
				command.Env = append(os.Environ(), "EVENT_NAME="+selection.event,
					"K8S_CHANGED="+selection.k8s, "BRIDGE_CHANGED="+selection.bridge,
					"FLUX_BRIDGE_RESULT="+result)
				output, err := command.CombinedOutput()
				wantPass := selection.want != "" && result == selection.want
				if (err == nil) != wantPass {
					t.Fatalf("result %q admitted=%t, want=%t: %v\n%s", result, err == nil, wantPass, err, output)
				}
			})
		}
	}
}
