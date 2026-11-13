// Package openfeatureoperatorsecurity checks the security context the
// OpenFeature operator's HelmRelease authors for its manager container (#5445).
//
// The open-feature-operator chart exposes no securityContext values, so the
// release supplies the hardening through a strategic-merge post-renderer. The
// cluster-wide add-security-context mutation sets readOnlyRootFilesystem on the
// admitted Pods, but Kubescape scores the stored Deployment template (C-0017),
// which only the release itself can change.
package openfeatureoperatorsecurity

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	deployment = "open-feature-operator-controller-manager"
	container  = "manager"
)

type podPatch struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext map[string]any `yaml:"securityContext"`
				Containers      []struct {
					Name            string         `yaml:"name"`
					SecurityContext map[string]any `yaml:"securityContext"`
					VolumeMounts    []any          `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []any `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type release struct {
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
}

func readRelease(t *testing.T) release {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..",
		"k8s", "bases", "infrastructure", "controllers", "openfeature-operator", "helm-release.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document struct {
		Spec release `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return document.Spec
}

// deploymentPatch returns the one post-renderer patch for the manager Deployment.
func deploymentPatch(t *testing.T, spec release) podPatch {
	t.Helper()
	var found []podPatch
	for _, renderer := range spec.PostRenderers {
		for _, patch := range renderer.Kustomize.Patches {
			if patch.Target.Kind != "Deployment" || patch.Target.Name != deployment {
				continue
			}
			var parsed podPatch
			if err := yaml.Unmarshal([]byte(patch.Patch), &parsed); err != nil {
				t.Fatalf("parse the %s patch: %v", deployment, err)
			}
			if parsed.Kind != "Deployment" || parsed.Metadata.Name != deployment {
				t.Fatalf("the %s patch names %s/%s", deployment, parsed.Kind, parsed.Metadata.Name)
			}
			found = append(found, parsed)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one post-renderer patch for Deployment %s, found %d", deployment, len(found))
	}
	return found[0]
}

func TestManagerRunsWithAnImmutableRootFilesystem(t *testing.T) {
	patch := deploymentPatch(t, readRelease(t))
	pod := patch.Spec.Template.Spec

	// The pod-level seccomp profile from #3474 stays in the same patch.
	seccomp, _ := pod.SecurityContext["seccompProfile"].(map[string]any)
	if seccomp["type"] != "RuntimeDefault" {
		t.Errorf("the pod must keep seccompProfile RuntimeDefault, got %v", pod.SecurityContext["seccompProfile"])
	}

	var managers int
	for _, c := range pod.Containers {
		if c.Name != container {
			t.Errorf("the patch must touch only the %s container, also names %q", container, c.Name)
			continue
		}
		managers++
		if c.SecurityContext["readOnlyRootFilesystem"] != true {
			t.Errorf("the %s container must set readOnlyRootFilesystem: true, got %v", container, c.SecurityContext["readOnlyRootFilesystem"])
		}
		// The patch adds the one field; it must not restate or relax the
		// chart's own hardening, which the strategic merge keeps.
		for key := range c.SecurityContext {
			if key != "readOnlyRootFilesystem" {
				t.Errorf("the %s container patch must set only readOnlyRootFilesystem, also sets %s", container, key)
			}
		}
		// The chart's two read-only certificate mounts are enough at runtime
		// (pods already run read-only through the admission mutation), so no
		// writable scratch volume is added.
		if len(c.VolumeMounts) != 0 {
			t.Errorf("the %s container patch must not add volume mounts, adds %d", container, len(c.VolumeMounts))
		}
	}
	if managers != 1 {
		t.Errorf("want the %s container in the patch exactly once, found %d", container, managers)
	}
	if len(pod.Volumes) != 0 {
		t.Errorf("the patch must not add volumes, adds %d", len(pod.Volumes))
	}
}
