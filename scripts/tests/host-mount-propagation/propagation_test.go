// Package propagation_test pins mount propagation on node agents that
// bind-mount host paths holding CSI volume mounts (#6041).
package propagation_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

type helmRelease struct {
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

type daemonSetPatch struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name         string `yaml:"name"`
					VolumeMounts []struct {
						MountPath        string `yaml:"mountPath"`
						MountPropagation string `yaml:"mountPropagation"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// propagation returns the mount propagation the release's post-renderers set
// on each mount path of one container of one DaemonSet.
func propagation(t *testing.T, release, target, container string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), release))
	if err != nil {
		t.Fatal(err)
	}
	var document helmRelease
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	set := map[string]string{}
	for _, renderer := range document.Spec.PostRenderers {
		for _, patch := range renderer.Kustomize.Patches {
			if patch.Target.Kind != "DaemonSet" || patch.Target.Name != target {
				continue
			}
			var strategic daemonSetPatch
			if yaml.Unmarshal([]byte(patch.Patch), &strategic) != nil || strategic.Kind != "DaemonSet" {
				continue // a JSON 6902 patch on the same DaemonSet
			}
			for _, item := range strategic.Spec.Template.Spec.Containers {
				if item.Name != container {
					continue
				}
				for _, mount := range item.VolumeMounts {
					if _, duplicate := set[mount.MountPath]; duplicate {
						t.Fatalf("%s patches %s twice", release, mount.MountPath)
					}
					set[mount.MountPath] = mount.MountPropagation
				}
			}
		}
	}
	return set
}

// A container started without propagation keeps a private copy of every
// volume mounted on its node, so an encrypted Longhorn volume cannot close its
// LUKS device and never detaches. Each host path that can hold a CSI mount
// must receive unmounts from the host, and none may propagate mounts back.
func TestNodeAgentsReceiveHostUnmounts(t *testing.T) {
	for _, tt := range []struct {
		release, target, container string
		paths                      []string
	}{
		{"k8s/bases/infrastructure/controllers/kubescape/helm-release.yaml", "^node-agent$", "node-agent", []string{"/host", "/var", "/var/lib/kubelet"}},
		{"k8s/bases/infrastructure/substrate/helm-release-atelet.yaml", "atelet", "atelet", []string{"/var/lib/kubelet/plugins"}},
	} {
		t.Run(tt.container, func(t *testing.T) {
			set := propagation(t, tt.release, tt.target, tt.container)
			for _, path := range tt.paths {
				if set[path] != "HostToContainer" {
					t.Errorf("%s must mount %s with HostToContainer, got %q", tt.container, path, set[path])
				}
			}
			for path, mode := range set {
				if mode == "Bidirectional" {
					t.Errorf("%s must not propagate mounts from %s back to the host", tt.container, path)
				}
			}
		})
	}
}
