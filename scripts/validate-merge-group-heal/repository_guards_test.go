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

type repositoryGuardStep struct {
	Name     string            `yaml:"name"`
	If       string            `yaml:"if"`
	Run      string            `yaml:"run"`
	Continue any               `yaml:"continue-on-error"`
	Env      map[string]string `yaml:"env"`
}

type repositoryGuardJob struct {
	If          string                `yaml:"if"`
	Needs       []string              `yaml:"needs"`
	Continue    any                   `yaml:"continue-on-error"`
	Permissions map[string]string     `yaml:"permissions"`
	Steps       []repositoryGuardStep `yaml:"steps"`
}

type repositoryGuardWorkflow struct {
	Jobs map[string]repositoryGuardJob `yaml:"jobs"`
}

// readRepositoryGuardWorkflow examines the actual workflow consumed by Actions.
func readRepositoryGuardWorkflow(t *testing.T) repositoryGuardWorkflow {
	t.Helper()
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow repositoryGuardWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

// repositoryGuardContract rejects dependencies that serialize classification or
// allow deployment and required checks to bypass unconditional repository guards.
func repositoryGuardContract(t *testing.T, workflow repositoryGuardWorkflow) {
	t.Helper()
	guards, ok := workflow.Jobs["validate-repository-guards"]
	if !ok {
		t.Fatal("missing independent repository guards")
	}
	if guards.If != "" || len(guards.Needs) != 0 || guards.Continue != nil {
		t.Fatal("repository guards must run independently and unconditionally")
	}
	if len(guards.Permissions) != 1 || guards.Permissions["contents"] != "read" {
		t.Fatal("repository guards need only read-only checkout access")
	}
	for _, job := range []string{"deploy-prod", "ci-required-checks"} {
		found := false
		for _, dependency := range workflow.Jobs[job].Needs {
			found = found || dependency == "validate-repository-guards"
		}
		if !found {
			t.Fatalf("%s can bypass repository guards", job)
		}
	}
	for _, dependency := range workflow.Jobs["changes"].Needs {
		if dependency == "validate-repository-guards" {
			t.Fatal("path classification must not wait for repository guards")
		}
	}
	if workflow.Jobs["ci-required-checks"].If != "always()" {
		t.Fatal("required-check evaluation must run after unsuccessful dependencies")
	}
}

// TestRepositoryGuardsIndependent verifies the execution boundary, not a copied
// inventory of filenames or the formatting of the workflow.
func TestRepositoryGuardsIndependent(t *testing.T) {
	workflow := readRepositoryGuardWorkflow(t)
	repositoryGuardContract(t, workflow)
	for _, step := range workflow.Jobs["validate-repository-guards"].Steps {
		if step.If != "" || step.Continue != nil {
			t.Fatalf("repository guard %q may not skip or suppress a failure", step.Name)
		}
		if strings.Contains(step.Run, "needs.changes") || strings.Contains(step.If, "needs.changes") {
			t.Fatalf("repository guard %q depends on classification", step.Name)
		}
	}
}

// TestRepositoryGuardsRequiredResult executes the workflow's real result guard.
// The generic aggregator tolerates legitimate skips elsewhere; this job may not.
func TestRepositoryGuardsRequiredResult(t *testing.T) {
	workflow := readRepositoryGuardWorkflow(t)
	repositoryGuardContract(t, workflow)
	var guard *repositoryGuardStep
	for _, step := range workflow.Jobs["ci-required-checks"].Steps {
		if step.Env["REPOSITORY_GUARDS_RESULT"] != "${{ needs.validate-repository-guards.result }}" {
			continue
		}
		if guard != nil {
			t.Fatal("duplicate repository result guard")
		}
		resultGuard := step
		guard = &resultGuard
	}
	if guard == nil || guard.If != "" || guard.Continue != nil || strings.TrimSpace(guard.Run) == "" {
		t.Fatal("missing unconditional repository result guard")
	}
	for _, result := range []string{"success", "failure", "cancelled", "skipped", "", "unknown"} {
		t.Run(result, func(t *testing.T) {
			command := exec.Command("bash", "-e", "-o", "pipefail", "-c", guard.Run)
			command.Env = append(os.Environ(), "REPOSITORY_GUARDS_RESULT="+result)
			output, err := command.CombinedOutput()
			if result == "success" && err != nil {
				t.Fatalf("successful repository guards rejected: %v\n%s", err, output)
			}
			if result != "success" && err == nil {
				t.Fatalf("accepted incomplete repository guards: %q", result)
			}
		})
	}
}

// TestChangeDetectionAvoidsUnneededToolDownloads exercises detection's actual
// shell steps while tool-download boundaries fail and queue capture stays usable.
func TestChangeDetectionAvoidsUnneededToolDownloads(t *testing.T) {
	workflow := readRepositoryGuardWorkflow(t)
	for _, event := range []string{"pull_request", "merge_group"} {
		t.Run(event, func(t *testing.T) {
			work := t.TempDir()
			bin := filepath.Join(work, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{"curl", "kubectl", "sudo"} {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nprintf 'unneeded tool call\\n' >> \"$NETWORK_LOG\"\nexit 71\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			capture := "#!/bin/sh\nset -eu\n[ \"$*\" = 'run ./scripts/verify-merge-group-queued capture' ] || exit 72\nprintf '%s\\n' \"$*\" >> \"$CAPTURE_LOG\"\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(capture), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, step := range workflow.Jobs["changes"].Steps {
				if strings.TrimSpace(step.Run) == "" {
					continue
				}
				if step.If != "" {
					if step.If != "github.event_name == 'merge_group'" {
						t.Fatalf("cannot evaluate detection condition %q", step.If)
					}
					if event != "merge_group" {
						continue
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				command := exec.CommandContext(ctx, "bash", "-e", "-o", "pipefail", "-c", step.Run)
				command.Dir = filepath.Join("..", "..")
				command.Env = []string{
					"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
					"HOME=" + work,
					"RUNNER_TEMP=" + work,
					"GITHUB_OUTPUT=" + filepath.Join(work, "outputs"),
					"NETWORK_LOG=" + filepath.Join(work, "network"),
					"CAPTURE_LOG=" + filepath.Join(work, "capture"),
				}
				for key, value := range step.Env {
					switch key {
					case "PATH", "HOME", "RUNNER_TEMP", "GITHUB_OUTPUT", "NETWORK_LOG", "CAPTURE_LOG":
						t.Fatalf("detection overrides protected fixture environment %q", key)
					}
					command.Env = append(command.Env, key+"="+value)
				}
				output, err := command.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("detection step %q requires unnecessary tooling: %v\n%s", step.Name, err, output)
				}
			}
			if _, err := os.Stat(filepath.Join(work, "network")); !os.IsNotExist(err) {
				t.Fatalf("detection reached an unnecessary tool boundary: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(work, "capture"))
			if event == "merge_group" {
				if err != nil || string(got) != "run ./scripts/verify-merge-group-queued capture\n" {
					t.Fatalf("merge-group detection did not capture its queue receipt: %q, %v", got, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("pull-request detection must not capture a queue entry: %q, %v", got, err)
			}
		})
	}
}
