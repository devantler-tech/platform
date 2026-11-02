// Package ksailoperatorstate checks the writable k9s log location the KSail
// operator needs on its read-only root filesystem (devantler-tech/platform#4769).
package ksailoperatorstate

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const helmReleasePath = "k8s/bases/infrastructure/ksail-operator/helm-release.yaml"

type container struct {
	Name string `yaml:"name"`
	Env  []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"volumeMounts"`
	SecurityContext map[string]any `yaml:"securityContext"`
}

type deploymentPatch struct {
	Spec struct {
		Template struct {
			Spec struct {
				Volumes    []map[string]any `yaml:"volumes"`
				Containers []container      `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// operatorPatch returns the post-renderer patch that targets the operator
// Deployment. Exactly one such patch must exist.
func operatorPatch(release []byte) (string, error) {
	var document struct {
		Spec struct {
			PostRenderers []struct {
				Kustomize struct {
					Patches []struct {
						Target struct {
							Kind string `yaml:"kind"`
							Name string `yaml:"name"`
						} `yaml:"target"`
						Patch string `yaml:"patch"`
					} `yaml:"patches"`
				} `yaml:"kustomize"`
			} `yaml:"postRenderers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(release, &document); err != nil {
		return "", err
	}
	var found []string
	for _, renderer := range document.Spec.PostRenderers {
		for _, patch := range renderer.Kustomize.Patches {
			if patch.Target.Kind == "Deployment" && patch.Target.Name == "ksail-operator" {
				found = append(found, patch.Patch)
			}
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("want exactly one operator Deployment patch, found %d", len(found))
	}
	return found[0], nil
}

// stateProblems lists every way the patch fails to give k9s a bounded,
// writable log directory without loosening the read-only root filesystem.
func stateProblems(patch string) []string {
	var parsed deploymentPatch
	if err := yaml.Unmarshal([]byte(patch), &parsed); err != nil {
		return []string{"patch is not valid YAML: " + err.Error()}
	}
	spec := parsed.Spec.Template.Spec
	var operator *container
	for index := range spec.Containers {
		if spec.Containers[index].Name == "operator" {
			operator = &spec.Containers[index]
		}
	}
	if operator == nil {
		return []string{"patch does not configure the operator container"}
	}
	var problems []string
	if value, ok := operator.SecurityContext["readOnlyRootFilesystem"]; ok && value != true {
		problems = append(problems, "the operator's root filesystem is no longer read-only")
	}
	logDir := ""
	for _, variable := range operator.Env {
		if variable.Name == "K9S_LOGS_DIR" {
			logDir = variable.Value
		}
	}
	if logDir == "" || !path.IsAbs(logDir) || path.Clean(logDir) != logDir || logDir == "/" {
		return append(problems, fmt.Sprintf("K9S_LOGS_DIR must name a clean absolute directory, got %q", logDir))
	}
	volumeName := ""
	for _, mount := range operator.VolumeMounts {
		if mount.MountPath == logDir {
			volumeName = mount.Name
			if mount.ReadOnly {
				problems = append(problems, "the k9s log directory is mounted read-only")
			}
		}
	}
	if volumeName == "" {
		return append(problems, "no volume is mounted at K9S_LOGS_DIR "+logDir)
	}
	var volume map[string]any
	for _, candidate := range spec.Volumes {
		if candidate["name"] == volumeName {
			volume = candidate
		}
	}
	if volume == nil {
		return append(problems, "the k9s log mount names no declared volume "+volumeName)
	}
	if len(volume) != 2 {
		problems = append(problems, "the k9s log volume must be an emptyDir and nothing else")
	}
	emptyDir, ok := volume["emptyDir"].(map[string]any)
	if !ok {
		return append(problems, "the k9s log volume is not an emptyDir")
	}
	if limit, ok := emptyDir["sizeLimit"].(string); !ok || strings.TrimSpace(limit) == "" {
		problems = append(problems, "the k9s log emptyDir has no size limit")
	}
	return problems
}

// at walks a decoded YAML document by map keys and list indexes.
func at(t *testing.T, node any, keys ...any) any {
	t.Helper()
	for _, key := range keys {
		switch typed := key.(type) {
		case string:
			child, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("expected a mapping before %q", typed)
			}
			node = child[typed]
		case int:
			child, ok := node.([]any)
			if !ok || typed >= len(child) {
				t.Fatalf("expected a list with index %d", typed)
			}
			node = child[typed]
		}
	}
	return node
}

func TestOperatorGivesK9sABoundedWritableLogDirectory(t *testing.T) {
	release, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(helmReleasePath)))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := operatorPatch(release)
	if err != nil {
		t.Fatal(err)
	}
	if problems := stateProblems(patch); len(problems) > 0 {
		t.Fatalf("committed operator patch: %s", strings.Join(problems, "; "))
	}

	operatorOf := func(t *testing.T, document any) map[string]any {
		t.Helper()
		for _, entry := range at(t, document, "spec", "template", "spec", "containers").([]any) {
			if candidate := entry.(map[string]any); candidate["name"] == "operator" {
				return candidate
			}
		}
		t.Fatal("no operator container")
		return nil
	}
	volumeOf := func(t *testing.T, document any) map[string]any {
		t.Helper()
		return at(t, document, "spec", "template", "spec", "volumes", 0).(map[string]any)
	}
	// Each mutation removes or weakens one part of the committed patch and must
	// be reported, so the check above cannot pass on a patch it never examined.
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, any)
	}{
		{"variable removed", func(t *testing.T, d any) { delete(operatorOf(t, d), "env") }},
		{"variable points elsewhere", func(t *testing.T, d any) {
			at(t, operatorOf(t, d), "env", 0).(map[string]any)["value"] = "/var/log/other"
		}},
		{"relative directory", func(t *testing.T, d any) {
			at(t, operatorOf(t, d), "env", 0).(map[string]any)["value"] = "var/log/k9s"
		}},
		{"mount removed", func(t *testing.T, d any) { delete(operatorOf(t, d), "volumeMounts") }},
		{"mount read-only", func(t *testing.T, d any) {
			at(t, operatorOf(t, d), "volumeMounts", 0).(map[string]any)["readOnly"] = true
		}},
		{"volume removed", func(t *testing.T, d any) {
			delete(at(t, d, "spec", "template", "spec").(map[string]any), "volumes")
		}},
		{"size limit removed", func(t *testing.T, d any) { delete(volumeOf(t, d)["emptyDir"].(map[string]any), "sizeLimit") }},
		{"host path instead", func(t *testing.T, d any) {
			volume := volumeOf(t, d)
			delete(volume, "emptyDir")
			volume["hostPath"] = map[string]any{"path": "/var/log"}
		}},
		{"second source added", func(t *testing.T, d any) { volumeOf(t, d)["hostPath"] = map[string]any{"path": "/var/log"} }},
		{"root filesystem writable", func(t *testing.T, d any) {
			operatorOf(t, d)["securityContext"].(map[string]any)["readOnlyRootFilesystem"] = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document any
			if err := yaml.Unmarshal([]byte(patch), &document); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, document)
			mutated, err := yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if problems := stateProblems(string(mutated)); len(problems) == 0 {
				t.Fatalf("mutation %q was accepted", tc.name)
			}
		})
	}
}
