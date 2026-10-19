package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func rgdJobBoundary(jobs map[string]fluxBridgeJob) error {
	job, exists := jobs["validate-rgd-templates"]
	if !exists {
		return fmt.Errorf("missing independent PR nested-template validation")
	}
	if strings.Join(strings.Fields(job.If), " ") != "github.event_name == 'pull_request' && needs.changes.outputs.k8s == 'true'" ||
		!slices.Equal(job.Needs, []string{"changes"}) || job.Continue != nil {
		return fmt.Errorf("nested-template selection or independence changed")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		return fmt.Errorf("nested-template validation must be read-only")
	}
	if slices.Contains(jobs["validate"].Needs, "validate-rgd-templates") ||
		slices.Contains(jobs["deploy-prod"].Needs, "validate-rgd-templates") {
		return fmt.Errorf("PR scan serializes manifests or blocks merge-group deployment")
	}
	required := jobs["ci-required-checks"]
	if required.If != "always()" || !slices.Contains(required.Needs, "validate-rgd-templates") {
		return fmt.Errorf("required evaluation can bypass nested-template validation")
	}
	return nil
}

func TestRGDJobIndependent(t *testing.T) {
	if err := rgdJobBoundary(readFluxBridgeJobs(t)); err != nil {
		t.Fatal(err)
	}
}

func TestRGDJobBoundaryRejectsBypasses(t *testing.T) {
	for _, name := range []string{"missing", "serialized", "write", "suppressed", "unselected", "manifest dependency", "deploy dependency", "required dependency", "required condition"} {
		t.Run(name, func(t *testing.T) {
			jobs := readFluxBridgeJobs(t)
			if err := rgdJobBoundary(jobs); err != nil {
				t.Fatal(err)
			}
			job := jobs["validate-rgd-templates"]
			switch name {
			case "missing":
				delete(jobs, "validate-rgd-templates")
			case "serialized":
				job.Needs = append(job.Needs, "validate")
			case "write":
				job.Permissions = map[string]string{"contents": "write"}
			case "suppressed":
				job.Continue = true
			case "unselected":
				job.If = "false"
			case "manifest dependency", "deploy dependency":
				key := "validate"
				if name == "deploy dependency" {
					key = "deploy-prod"
				}
				dependent := jobs[key]
				dependent.Needs = append(dependent.Needs, "validate-rgd-templates")
				jobs[key] = dependent
			case "required dependency":
				required := jobs["ci-required-checks"]
				required.Needs = slices.DeleteFunc(required.Needs, func(need string) bool { return need == "validate-rgd-templates" })
				jobs["ci-required-checks"] = required
			case "required condition":
				required := jobs["ci-required-checks"]
				required.If = "success()"
				jobs["ci-required-checks"] = required
			}
			if name != "missing" {
				jobs["validate-rgd-templates"] = job
			}
			if err := rgdJobBoundary(jobs); err == nil {
				t.Fatal("accepted unsafe nested-template graph")
			}
		})
	}
}

func TestRGDJobRequiredResult(t *testing.T) {
	jobs := readFluxBridgeJobs(t)
	var guard *fluxBridgeStep
	for _, step := range jobs["ci-required-checks"].Steps {
		if step.Env["RGD_RESULT"] == "${{ needs.validate-rgd-templates.result }}" {
			if guard != nil {
				t.Fatal("duplicate nested-template result guard")
			}
			copy := step
			guard = &copy
		}
	}
	if guard == nil || guard.If != "" || guard.Continue != nil ||
		guard.Env["EVENT_NAME"] != "${{ github.event_name }}" ||
		guard.Env["K8S_CHANGED"] != "${{ needs.changes.outputs.k8s }}" {
		t.Fatal("missing unconditional nested-template guard or real selection bindings")
	}
	for _, selection := range []struct{ event, k8s, want string }{
		{"pull_request", "true", "success"}, {"pull_request", "false", "skipped"},
		{"pull_request", "", ""}, {"pull_request", "unknown", ""},
		{"merge_group", "true", "skipped"}, {"merge_group", "", "skipped"},
		{"push", "true", ""}, {"", "true", ""},
	} {
		for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
			t.Run(selection.event+"/"+selection.k8s+"/"+result, func(t *testing.T) {
				command := exec.Command("bash", "-e", "-o", "pipefail", "-c", guard.Run)
				command.Env = append(os.Environ(), "EVENT_NAME="+selection.event,
					"K8S_CHANGED="+selection.k8s, "RGD_RESULT="+result)
				output, err := command.CombinedOutput()
				wantPass := selection.want != "" && result == selection.want
				if (err == nil) != wantPass {
					t.Fatalf("admitted=%t, want=%t: %v\n%s", err == nil, wantPass, err, output)
				}
			})
		}
	}
}
