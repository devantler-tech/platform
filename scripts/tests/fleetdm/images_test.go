package fleetdm

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fleetImages includes hook Jobs as well as regular chart workloads.
func fleetImages(value any) []string {
	var images []string
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if image, ok := child.(string); key == "image" && ok && strings.HasPrefix(image, "fleetdm/fleet:") {
				images = append(images, image)
			}
			images = append(images, fleetImages(child)...)
		}
	case []any:
		for _, child := range value {
			images = append(images, fleetImages(child)...)
		}
	}
	return images
}

// TestPreparationUsesServerImage catches independently updated database and server
// releases in the actual chart output, including the authored post-renderers.
// It renders the retained opt-in app even while no production overlay enables it.
func TestPreparationUsesServerImage(t *testing.T) {
	helmbin := os.Getenv("FLEETDM_HELM_BIN")
	if helmbin == "" {
		helmbin, _ = exec.LookPath("helm")
	}
	kubectlbin := os.Getenv("FLEETDM_KUBECTL_BIN")
	if kubectlbin == "" {
		kubectlbin, _ = exec.LookPath("kubectl")
	}
	if helmbin == "" || kubectlbin == "" {
		if os.Getenv("FLEETDM_REQUIRE_NATIVE") == "1" {
			t.Fatal("native Helm and kubectl are required")
		}
		t.Skip("native Helm or kubectl is unavailable")
	}
	root := filepath.Join("..", "..", "..", "k8s", "bases", "apps", "fleetdm")
	b, err := os.ReadFile(filepath.Join(root, "helm-release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the committed opt-in defaults without production configuration.
	b = regexp.MustCompile(`\$\{[^:}]+:=([^}]+)\}`).ReplaceAll(b, []byte("$1"))
	b = bytes.ReplaceAll(b, []byte("${domain}"), []byte("example.invalid"))
	var release struct {
		Spec struct {
			Chart struct {
				Spec struct{ Chart, Version string }
			}
			Values        map[string]any
			PostRenderers []struct {
				Kustomize struct {
					Patches []struct {
						Target map[string]string
						Patch  string
					}
				}
			} `yaml:"postRenderers"`
		}
	}
	if err := yaml.Unmarshal(b, &release); err != nil {
		t.Fatal(err)
	}
	if len(release.Spec.PostRenderers) != 1 {
		t.Fatal("multiple post-renderer stages need a separate ordered evaluation")
	}
	b, err = os.ReadFile(filepath.Join(root, "helm-repository.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var repository struct{ Spec struct{ URL string } }
	if err := yaml.Unmarshal(b, &repository); err != nil {
		t.Fatal(err)
	}
	values, err := yaml.Marshal(release.Spec.Values)
	if err != nil {
		t.Fatal(err)
	}
	var patches []map[string]any
	for _, renderer := range release.Spec.PostRenderers {
		for _, patch := range renderer.Kustomize.Patches {
			patches = append(patches, map[string]any{"target": patch.Target, "patch": patch.Patch})
		}
	}
	b, err = os.ReadFile(filepath.Join(root, "pod-disruption-budget.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var budget struct {
		Spec struct {
			Selector struct {
				MatchLabels      map[string]string `yaml:"matchLabels"`
				MatchExpressions []any             `yaml:"matchExpressions"`
			}
		}
	}
	if err := yaml.Unmarshal(b, &budget); err != nil {
		t.Fatal(err)
	}
	if len(budget.Spec.Selector.MatchLabels) == 0 || len(budget.Spec.Selector.MatchExpressions) != 0 {
		t.Fatal("Fleet budget requires a nonempty matchLabels selector; expressions need explicit coverage")
	}
	for _, mode := range []string{"install", "upgrade"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			args := []string{"template", "fleetdm", release.Spec.Chart.Spec.Chart, "--repo", repository.Spec.URL, "--version", release.Spec.Chart.Spec.Version, "--namespace", "fleetdm", "--values", "-"}
			if mode == "upgrade" {
				args = append(args, "--is-upgrade")
			}
			cmd := exec.CommandContext(ctx, helmbin, args...)
			cmd.Stdin = bytes.NewReader(values)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("native chart render failed: %v", err)
			}
			decoder := yaml.NewDecoder(bytes.NewReader(out))
			var regular bytes.Buffer
			var chartFleetImages []string
			encoder := yaml.NewEncoder(&regular)
			for {
				var doc map[string]any
				if err := decoder.Decode(&doc); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				metadata, _ := doc["metadata"].(map[string]any)
				chartFleetImages = append(chartFleetImages, fleetImages(doc)...)
				annotations, _ := metadata["annotations"].(map[string]any)
				// Flux 2.8.8 post-renders regular manifests, not Helm hooks.
				if doc != nil && annotations["helm.sh/hook"] == nil {
					if err := encoder.Encode(doc); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := encoder.Close(); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "chart.yaml"), regular.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			kustomization, err := yaml.Marshal(map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": []string{"chart.yaml"}, "patches": patches})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), kustomization, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err = exec.CommandContext(ctx, kubectlbin, "kustomize", dir).Output()
			if err != nil {
				t.Fatalf("native chart post-render failed: %v", err)
			}
			decoder = yaml.NewDecoder(bytes.NewReader(out))
			var deployment []byte
			for {
				var doc map[string]any
				if err := decoder.Decode(&doc); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				metadata, _ := doc["metadata"].(map[string]any)
				if doc["kind"] == "Deployment" && metadata["name"] == "fleet" {
					if deployment != nil {
						t.Fatal("multiple Fleet Deployments rendered")
					}
					deployment, err = yaml.Marshal(doc)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if deployment == nil {
				t.Fatal("chart rendered no Fleet Deployment")
			}
			var result struct {
				Spec struct {
					Template struct {
						Metadata struct{ Labels map[string]string }
						Spec     struct {
							Containers     []struct{ Name, Image string }
							InitContainers []struct {
								Name, Image string
								Command     []string
							} `yaml:"initContainers"`
						}
					}
				}
			}
			if err := yaml.Unmarshal(deployment, &result); err != nil {
				t.Fatal(err)
			}
			for key, value := range budget.Spec.Selector.MatchLabels {
				actual, exists := result.Spec.Template.Metadata.Labels[key]
				if !exists || actual != value {
					t.Fatalf("Fleet budget does not select the rendered server: label %q", key)
				}
			}
			var server, preparation string
			for _, container := range result.Spec.Template.Spec.Containers {
				if container.Name == "fleet" {
					server = container.Image
				}
			}
			for _, container := range result.Spec.Template.Spec.InitContainers {
				if container.Name == "fleet-db-init" {
					preparation = container.Image
					if strings.Join(container.Command, " ") != "fleet prepare db" {
						t.Fatal("database preparation command changed")
					}
				}
			}
			if server == "" || preparation == "" || server != preparation {
				t.Fatalf("migration/server release mismatch: preparation=%q server=%q", preparation, server)
			}
			for _, image := range chartFleetImages {
				if image != server {
					t.Fatalf("charted Fleet workload image %q differs from server %q", image, server)
				}
			}
		})
	}
}
