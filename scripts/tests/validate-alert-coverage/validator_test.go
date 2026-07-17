package validatealertcoverage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var (
	repositoryRoot = findRepositoryRoot()
	validatorPath  = filepath.Join(repositoryRoot, "scripts", "validate-alert-coverage.sh")
	ciWorkflowPath = filepath.Join(repositoryRoot, ".github", "workflows", "ci.yaml")
)

var layers = []string{
	"k8s/clusters/prod",
	"k8s/providers/hetzner/bootstrap",
	"k8s/providers/hetzner/infrastructure/controllers",
	"k8s/providers/hetzner/infrastructure",
	"k8s/providers/hetzner/apps",
}

type validatorFixture struct {
	workspace string
	script    string
}

type commandResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func TestValidWildcardAlertCoversEveryRenderedNamespace(t *testing.T) {
	fixture := newValidatorFixture(t)

	result := fixture.runValidator(t, nil)

	if result.exitCode != 0 {
		t.Fatalf("validator exit code = %d, want 0; stderr = %q", result.exitCode, result.stderr)
	}
}

func TestMissingLayerFailsClosedInsteadOfBeingSkipped(t *testing.T) {
	fixture := newValidatorFixture(t)
	missingLayer := layers[len(layers)-1]
	if err := os.Remove(filepath.Join(fixture.workspace, missingLayer, "kustomization.yaml")); err != nil {
		t.Fatalf("remove missing-layer fixture: %v", err)
	}

	result := fixture.runValidator(t, nil)
	output := result.stdout + result.stderr

	if result.exitCode == 0 {
		t.Fatalf("validator unexpectedly succeeded; output = %q", output)
	}
	if !strings.Contains(output, missingLayer) {
		t.Fatalf("validator output = %q, want missing layer %q", output, missingLayer)
	}
}

func TestNamedEventSourceDoesNotCoverItsWholeNamespace(t *testing.T) {
	fixture := newValidatorFixture(t)
	fixture.writeAlert(t, false)

	result := fixture.runValidator(t, nil)
	output := result.stdout + result.stderr

	if result.exitCode == 0 {
		t.Fatalf("validator unexpectedly succeeded; output = %q", output)
	}
	if !strings.Contains(output, "does not watch every namespace") {
		t.Fatalf("validator output = %q, want uncovered-namespace diagnostic", output)
	}
}

func TestWatchedResourceWithoutNamespaceFailsClosed(t *testing.T) {
	tests := []struct {
		kind string
		name string
	}{
		{kind: "HelmRelease", name: "release-0"},
		{kind: "Kustomization", name: "layer-0"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			fixture := newValidatorFixture(t)
			resourcePath := filepath.Join(fixture.workspace, layers[0], "resources.yaml")
			resources, err := os.ReadFile(resourcePath)
			if err != nil {
				t.Fatalf("read resource fixture: %v", err)
			}

			namespaced := fmt.Sprintf(
				"kind: %s\nmetadata:\n  name: %s\n  namespace: flux-system",
				tt.kind,
				tt.name,
			)
			if !bytes.Contains(resources, []byte(namespaced)) {
				t.Fatalf("resource fixture does not contain %q", namespaced)
			}
			withoutNamespace := fmt.Sprintf("kind: %s\nmetadata:\n  name: %s", tt.kind, tt.name)
			resources = bytes.Replace(resources, []byte(namespaced), []byte(withoutNamespace), 1)
			writeFile(t, resourcePath, string(resources), 0o600)

			result := fixture.runValidator(t, nil)
			output := result.stdout + result.stderr

			if result.exitCode == 0 {
				t.Fatalf("validator unexpectedly succeeded; output = %q", output)
			}
			for _, want := range []string{"missing metadata.namespace", tt.kind, tt.name} {
				if !strings.Contains(output, want) {
					t.Fatalf("validator output = %q, want %q", output, want)
				}
			}
		})
	}
}

func TestYQDiagnosticsRemainVisibleOnQueryFailure(t *testing.T) {
	fixture := newValidatorFixture(t)
	binDir := filepath.Join(fixture.workspace, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create fixture bin directory: %v", err)
	}
	yqPath := filepath.Join(binDir, "yq")
	writeFile(
		t,
		yqPath,
		"#!/usr/bin/env bash\necho fixture-yq-query-diagnostic >&2\nexit 72\n",
		0o700,
	)

	result := fixture.runValidator(t, map[string]string{
		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	})

	if result.exitCode == 0 {
		t.Fatal("validator unexpectedly succeeded")
	}
	if !strings.Contains(result.stderr, "fixture-yq-query-diagnostic") {
		t.Fatalf("validator stderr = %q, want yq diagnostic", result.stderr)
	}
}

func TestCIRunsBehavioralRegressionsWhenTheyChange(t *testing.T) {
	workflow, err := os.ReadFile(ciWorkflowPath)
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}

	for _, want := range []string{
		"'scripts/tests/validate-alert-coverage/**'",
		"go test ./scripts/tests/validate-alert-coverage",
	} {
		if !bytes.Contains(workflow, []byte(want)) {
			t.Fatalf("CI workflow does not contain %q", want)
		}
	}
}

func findRepositoryRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate alert coverage test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func newValidatorFixture(t *testing.T) validatorFixture {
	t.Helper()

	workspace := t.TempDir()
	script := filepath.Join(workspace, "scripts", filepath.Base(validatorPath))
	if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
		t.Fatalf("create fixture scripts directory: %v", err)
	}
	validator, err := os.ReadFile(validatorPath)
	if err != nil {
		t.Fatalf("read validator: %v", err)
	}
	writeFile(t, script, string(validator), 0o700)

	fixture := validatorFixture{workspace: workspace, script: script}
	for index, layer := range layers {
		fixture.writeLayer(t, layer, index)
	}
	fixture.writeAlert(t, true)
	return fixture
}

func (fixture validatorFixture) writeLayer(t *testing.T, relativePath string, index int) {
	t.Helper()

	layer := filepath.Join(fixture.workspace, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(layer, 0o700); err != nil {
		t.Fatalf("create fixture layer: %v", err)
	}
	writeFile(t, filepath.Join(layer, "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - resources.yaml
`, 0o600)
	writeFile(t, filepath.Join(layer, "resources.yaml"), fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: release-%d
  namespace: flux-system
spec: {}
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: layer-%d
  namespace: flux-system
spec: {}
`, index, index), 0o600)
}

func (fixture validatorFixture) writeAlert(t *testing.T, wildcard bool) {
	t.Helper()

	name := "one-resource"
	if wildcard {
		name = "*"
	}
	alertPath := filepath.Join(
		fixture.workspace,
		"k8s",
		"providers",
		"hetzner",
		"infrastructure",
		"flux-notifications",
		"alert.yaml",
	)
	if err := os.MkdirAll(filepath.Dir(alertPath), 0o700); err != nil {
		t.Fatalf("create fixture alert directory: %v", err)
	}
	writeFile(t, alertPath, fmt.Sprintf(`apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Alert
metadata:
  name: reconciliation
  namespace: flux-system
spec:
  eventSources:
    - kind: HelmRelease
      name: %q
      namespace: flux-system
    - kind: Kustomization
      name: %q
      namespace: flux-system
`, name, name), 0o600)
}

func (fixture validatorFixture) runValidator(t *testing.T, environmentOverrides map[string]string) commandResult {
	t.Helper()

	command := exec.Command("bash", fixture.script)
	command.Dir = fixture.workspace
	command.Env = environmentWithOverrides(environmentOverrides)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run validator: %v", err)
		}
		exitCode = exitError.ExitCode()
	}

	return commandResult{
		exitCode: exitCode,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
	}
}

func environmentWithOverrides(overrides map[string]string) []string {
	environment := os.Environ()
	for key, value := range overrides {
		prefix := key + "="
		replaced := false
		for index, item := range environment {
			if strings.HasPrefix(item, prefix) {
				environment[index] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			environment = append(environment, prefix+value)
		}
	}
	return environment
}

func writeFile(t *testing.T, path string, contents string, permissions os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), permissions); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
