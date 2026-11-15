package ocichartsources

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The signed manager image defaults to UID/GID 65532. Only its host-facing
// daemon and pre-upgrade command may override that identity; API-only global
// commands and the UI retain their existing non-root contexts.
func TestLonghornRuntimeCommandIdentities(t *testing.T) {
	root := repositoryRoot(t)
	base, err := os.ReadFile(filepath.Join(root, "k8s/providers/hetzner/infrastructure/controllers/longhorn/helm-release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"longhorn-platform-runtime", "longhorn-v2-engine-settings", "longhorn-v2-only"} {
		t.Run(component, func(t *testing.T) {
			// A real base HelmRelease and component exercise Kustomize's list merge
			// behavior too, including preservation of the existing UI/global patches.
			work, err := os.MkdirTemp(root, ".longhorn-runtime-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(work) })
			writeRuntimeYAML(t, filepath.Join(work, "release.yaml"), base)
			writeRuntimeYAML(t, filepath.Join(work, "kustomization.yaml"), map[string]any{
				"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
				"resources": []string{"release.yaml"}, "components": []string{"../k8s/bases/components/" + component},
			})
			var release map[string]any
			for _, doc := range buildRuntimeYAML(t, work) {
				if doc["kind"] == "HelmRelease" {
					release = doc
				}
			}
			spec := object(release, "spec")
			if spec["postRenderStrategy"] != "combined" {
				t.Fatal("the pre-upgrade hook must reach the identity post-renderer, not inherit nohooks")
			}

			// Model the relevant chart outputs before post-rendering: privileged
			// does not override the image's non-root USER on either host command.
			var manifests []map[string]any
			for _, workload := range []struct{ kind, name, container string }{
				{"DaemonSet", "longhorn-manager", "longhorn-manager"},
				{"Job", "longhorn-pre-upgrade", "longhorn-pre-upgrade"},
				{"Deployment", "longhorn-global-manager", "longhorn-global-manager"},
				{"Deployment", "longhorn-ui", "longhorn-ui"},
			} {
				apiVersion := "apps/v1"
				if workload.kind == "Job" {
					apiVersion = "batch/v1"
				}
				manifests = append(manifests, map[string]any{
					"apiVersion": apiVersion,
					"kind":       workload.kind, "metadata": map[string]any{"name": workload.name},
					"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
						"containers": []any{map[string]any{"name": workload.container, "image": "manager-with-nonroot-default"}},
					}}},
				})
			}
			rendered := filepath.Join(work, "rendered.yaml")
			for _, renderer := range spec["postRenderers"].([]any) {
				var stream bytes.Buffer
				encoder := yaml.NewEncoder(&stream)
				for _, doc := range manifests {
					if err := encoder.Encode(doc); err != nil {
						t.Fatal(err)
					}
				}
				if err := encoder.Close(); err != nil {
					t.Fatal(err)
				}
				writeRuntimeYAML(t, rendered, stream.Bytes())
				kustomize := object(renderer.(map[string]any), "kustomize")
				kustomize["apiVersion"], kustomize["kind"] = "kustomize.config.k8s.io/v1beta1", "Kustomization"
				kustomize["resources"] = []string{"rendered.yaml"}
				writeRuntimeYAML(t, filepath.Join(work, "kustomization.yaml"), kustomize)
				manifests = buildRuntimeYAML(t, work)
			}
			for _, doc := range manifests {
				name := object(doc, "metadata")["name"]
				pod := object(object(object(doc, "spec"), "template"), "spec")
				context := object(pod["containers"].([]any)[0].(map[string]any), "securityContext")
				wantUID, wantGID := 0, 0
				switch name {
				case "longhorn-global-manager":
					wantUID, wantGID = 65532, 65532
				case "longhorn-ui":
					wantUID, wantGID = 499, 486
				}
				if context["runAsUser"] != wantUID || context["runAsGroup"] != wantGID {
					t.Errorf("%s: got UID/GID %v/%v, want %d/%d", name, context["runAsUser"], context["runAsGroup"], wantUID, wantGID)
				}
				if wantUID != 0 && context["runAsNonRoot"] != true {
					t.Errorf("%s lost its non-root requirement", name)
				}
			}
		})
	}
}

func writeRuntimeYAML(t *testing.T, path string, value any) {
	t.Helper()
	data, raw := value.([]byte)
	if !raw {
		var err error
		data, err = yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildRuntimeYAML(t *testing.T, directory string) []map[string]any {
	t.Helper()
	output, err := exec.Command("kubectl", "kustomize", directory).CombinedOutput()
	if err != nil {
		t.Fatalf("Kustomize failed: %v\n%s", err, output)
	}
	var documents []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err == io.EOF {
			return documents
		} else if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, doc)
	}
}
