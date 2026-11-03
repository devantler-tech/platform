package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestPublicationSelectsBuildToolChanges prevents a successful PR build from
// hiding checker-module or signing-tool changes until an unrelated release.
func TestPublicationSelectsBuildToolChanges(t *testing.T) {
	var workflow struct {
		On map[string]struct {
			Paths []string `yaml:"paths"`
		} `yaml:"on"`
	}
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-dex-security-image.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "push"} {
		for _, input := range []string{"go.mod", "go.sum", ".github/scripts/setup-supply-chain-tools.sh"} {
			t.Run(event+"/"+input, func(t *testing.T) {
				if !slices.Contains(workflow.On[event].Paths, input) {
					t.Fatalf("%s does not select a build with changed input %s", event, input)
				}
			})
		}
	}
}

// TestPublisherRetainsTheProducingAttempt runs the real artifact-naming step
// and resolves its declared transfer across two different attempt contexts.
func TestPublisherRetainsTheProducingAttempt(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Outputs map[string]string `yaml:"outputs"`
			Steps   []struct {
				ID, Uses, Run string
				With          map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "publish-dex-security-image.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, producer, consumer, want string }{
		{"publish-only retry", "1", "2", "dex-image-101-1"},
		{"full verification retry", "2", "2", "dex-image-101-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer := map[string]string{"github.run_id": "101", "github.run_attempt": tc.producer}
			consumer := map[string]string{"github.run_id": "101", "github.run_attempt": tc.consumer}
			verify := workflow.Jobs["verify"]
			for _, step := range verify.Steps {
				if step.ID != "artifact" {
					continue
				}
				output := filepath.Join(t.TempDir(), "output")
				command := exec.Command("bash", "-euo", "pipefail", "-c", step.Run)
				command.Env = append(os.Environ(), "GITHUB_RUN_ID=101", "GITHUB_RUN_ATTEMPT="+tc.producer, "GITHUB_OUTPUT="+output)
				if b, err := command.CombinedOutput(); err != nil {
					t.Fatalf("artifact naming step: %v: %s", err, b)
				}
				b, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					key, value, ok := strings.Cut(line, "=")
					if !ok {
						t.Fatal("invalid output from naming step")
					}
					producer["steps.artifact.outputs."+key] = value
				}
			}
			if expression := verify.Outputs["artifact_name"]; expression != "" {
				consumer["needs.verify.outputs.artifact_name"] = renderArtifactExpression(t, expression, producer)
			}
			uploaded, downloaded := "", ""
			for _, step := range verify.Steps {
				if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
					uploaded = renderArtifactExpression(t, step.With["name"], producer)
				}
			}
			for _, step := range workflow.Jobs["publish"].Steps {
				if strings.HasPrefix(step.Uses, "actions/download-artifact@") {
					downloaded = renderArtifactExpression(t, step.With["name"], consumer)
				}
			}
			if uploaded != tc.want || downloaded != tc.want {
				t.Fatalf("uploaded=%q downloaded=%q; both must retain %q", uploaded, downloaded, tc.want)
			}
		})
	}
}

// Only the context references used by this artifact handoff are modeled. A new
// expression cannot silently resolve to an empty selector.
func renderArtifactExpression(t *testing.T, expression string, values map[string]string) string {
	t.Helper()
	pattern := regexp.MustCompile(`\$\{\{\s*([^{}]+?)\s*\}\}`)
	return pattern.ReplaceAllStringFunc(expression, func(token string) string {
		key := strings.TrimSpace(pattern.FindStringSubmatch(token)[1])
		value, ok := values[key]
		if !ok {
			t.Fatalf("unbound artifact reference %q", key)
		}
		return value
	})
}
