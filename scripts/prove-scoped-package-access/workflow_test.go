package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This credentialed experiment must never become a PR-triggered job or inherit
// the App installation's write permissions when its workflow is edited.
func TestWorkflowKeepsTheProofManualAndTheCandidateReadOnly(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/prove-scoped-package-access.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 1 {
		t.Fatal("proof must have one manual trigger")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("proof must require manual dispatch")
	}
	if len(workflow.Permissions) != 0 {
		t.Fatal("root token permissions must be empty")
	}
	if len(workflow.Jobs) != 1 {
		t.Fatal("additional credentialed jobs require review of this boundary")
	}
	job := workflow.Jobs["prove"]
	if job.If != "github.ref == 'refs/heads/main'" {
		t.Fatal("proof must be restricted to reviewed main")
	}
	if len(job.Permissions) != 1 || job.Permissions["contents"] != "read" {
		t.Fatal("workflow token must only read contents")
	}
	mints := 0
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Uses, "actions/create-github-app-token@") {
			continue
		}
		mints++
		if err := candidateRepositoryScope(step.With); err != nil {
			t.Fatal(err)
		}
		if step.With["permission-packages"] != "read" {
			t.Fatal("candidate needs explicit packages read only")
		}
		for key := range step.With {
			if strings.HasPrefix(key, "permission-") && key != "permission-packages" {
				t.Fatalf("unexpected App grant %s", key)
			}
		}
		if step.With["skip-token-revoke"] == "true" {
			t.Fatal("candidate must be revoked by the action's cleanup")
		}
	}
	if mints != 1 {
		t.Fatal("exactly one candidate token is required")
	}
}

// The pinned action defaults an omitted owner to GITHUB_REPOSITORY_OWNER while
// preserving its explicit repository list. An owner input also triggers the
// singular/plural input bug in zizmor 1.25.0; never work around it by dropping
// the repository list or granting a different owner.
func candidateRepositoryScope(inputs map[string]string) error {
	if _, present := inputs["owner"]; present {
		return errors.New("candidate must use the current repository owner default")
	}
	if inputs["repositories"] != "wedding-app" {
		return errors.New("candidate repository scope widened or absent")
	}
	return nil
}

func TestCandidateScopeRefusesMissingOrBroaderRepositorySelection(t *testing.T) {
	for name, inputs := range map[string]map[string]string{
		"missing repositories": {},
		"empty repositories":   {"repositories": ""},
		"extra repository":     {"repositories": "wedding-app,platform"},
		"different owner":      {"owner": "other-owner", "repositories": "wedding-app"},
		"empty owner input":    {"owner": "", "repositories": "wedding-app"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := candidateRepositoryScope(inputs); err == nil {
				t.Fatal("unsafe candidate scope was accepted")
			}
		})
	}
}
