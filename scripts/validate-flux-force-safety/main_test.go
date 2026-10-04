package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const layer = "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: apps}\nspec: {force: false}\n"

func TestForceBoundary(t *testing.T) {
	for _, tt := range []struct{ name, data, want string }{
		{"safe-layer", layer, ""},
		{"forcing-layer", strings.Replace(layer, "force: false", "force: true", 1), "layer-wide force"},
		{"disabled-annotation-does-not-override", strings.Replace(layer, "force: false", "force: true", 1) + "---\napiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata: {name: db, annotations: {kustomize.toolkit.fluxcd.io/force: disabled}}\n", "layer-wide force"},
		{"forcing-tenant-template", "apiVersion: kro.run/v1alpha1\nkind: ResourceGraphDefinition\nspec:\n  resources:\n  - id: tenant\n    template:\n      " + strings.ReplaceAll(strings.Replace(layer, "force: false", "force: true", 1), "\n", "\n      "), "layer-wide force"},
		{"safe-tenant-template", "kind: ResourceGraphDefinition\nspec:\n  resources:\n  - template:\n      " + strings.ReplaceAll(layer, "\n", "\n      "), ""},
		{"invalid-force", strings.Replace(layer, "force: false", "force: '${schema.spec.force}'", 1), "flux force must be a literal boolean"},
		{"claim-opt-in", layer + "---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: data, annotations: {kustomize.toolkit.fluxcd.io/force: enabled}}\n", "persistent resource"},
		{"database-opt-in", layer + "---\napiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata: {name: db, annotations: {kustomize.toolkit.fluxcd.io/force: ENABLED}}\n", "persistent resource"},
		{"job-opt-in", layer + "---\napiVersion: batch/v1\nkind: Job\nmetadata: {name: setup, annotations: {kustomize.toolkit.fluxcd.io/force: enabled}}\n", ""},
		{"helm-force-is-separate", layer + "---\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nspec: {upgrade: {force: true}}\n", ""},
		{"no-census", "kind: ConfigMap\n", "no Flux Kustomization"},
		{"partial-yaml", layer + "---\nkind: [\n", "yaml"},
		{"duplicate-force", strings.Replace(layer, "spec: {force: false}", "spec: {force: false, force: true}", 1), "duplicate"},
		{"merge-hidden-force", "defaults: &defaults {force: true}\n" + strings.Replace(layer, "spec: {force: false}", "spec: {<<: *defaults}", 1), "merge keys"},
		{"aliased-force", "enabled: &enabled true\n" + strings.Replace(layer, "force: false", "force: *enabled", 1), "layer-wide force"},
		{"cyclic-list", layer + "---\n&cycle\nkind: List\nitems: [*cycle]\n", "cyclic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(tt.data), 0600); err != nil {
				t.Fatal(err)
			}
			err := verify(path)
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
		})
	}
}

func TestPersistentForceRequiresInspectableValues(t *testing.T) {
	for _, resource := range []string{"apiVersion: v1\nkind: PersistentVolumeClaim", "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster"} {
		for _, tc := range []struct{ name, metadata, want string }{
			{"absent", "{name: data}", ""},
			{"disabled", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: disabled}}", ""},
			{"unrelated-expression", "{name: data, annotations: {description: '${schema.description}'}}", ""},
			{"force-expression", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: '${schema.force}'}}", "literal string"},
			{"force-map", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: {value: enabled}}}", "literal string"},
			{"force-list", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: [enabled]}}", "literal string"},
			{"force-boolean", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: false}}", "literal string"},
			{"force-null", "{name: data, annotations: {kustomize.toolkit.fluxcd.io/force: null}}", "literal string"},
			{"annotations-expression", "{name: data, annotations: '${schema.annotations}'}", "expected YAML mapping"},
			{"metadata-expression", "'${schema.metadata}'", "expected YAML mapping"},
		} {
			t.Run(resource+"/"+tc.name, func(t *testing.T) {
				template := resource + "\nmetadata: " + tc.metadata + "\n"
				data := layer + "---\napiVersion: kro.run/v1alpha1\nkind: ResourceGraphDefinition\nspec:\n  resources:\n  - id: data\n    template:\n      " + strings.ReplaceAll(template, "\n", "\n      ")
				path := filepath.Join(t.TempDir(), "manifest.yaml")
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				err := verify(path)
				if tc.want == "" && err != nil {
					t.Fatal(err)
				}
				if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
					t.Fatalf("wanted %q, got %v", tc.want, err)
				}
			})
		}
	}
}

func TestRenderedOverride(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"layer.yaml":         layer,
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [layer.yaml]\npatches:\n- target: {kind: Kustomization, name: apps}\n  patch: |-\n    - op: replace\n      path: /spec/force\n      value: true\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := exec.Command("kubectl", "kustomize", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, data)
	}
	path := filepath.Join(t.TempDir(), "render.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verify(path); err == nil || !strings.Contains(err.Error(), "layer-wide force") {
		t.Fatalf("rendered override passed: %v", err)
	}
}

func TestRepositoryBoundary(t *testing.T) {
	if err := verify("../../k8s"); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredGuard(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If    string   `yaml:"if"`
			Needs []string `yaml:"needs"`
			Steps []struct {
				If       string `yaml:"if"`
				Continue bool   `yaml:"continue-on-error"`
				Run      string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "merge_group"} {
		if _, exists := workflow.On[event]; !exists {
			t.Fatalf("missing %s trigger", event)
		}
	}
	changes := workflow.Jobs["changes"]
	if changes.If != "" {
		t.Fatal("replacement guard job cannot be conditional")
	}
	guarded := false
	for _, step := range changes.Steps {
		if step.If == "" && !step.Continue && strings.Contains(step.Run, "bash scripts/tests/test-flux-force-safety.sh") {
			guarded = true
		}
	}
	if !guarded {
		t.Fatal("missing unconditional rendered replacement guard")
	}
	for _, need := range workflow.Jobs["ci-required-checks"].Needs {
		if need == "changes" {
			return
		}
	}
	t.Fatal("required CI does not wait for the replacement guard")
}
