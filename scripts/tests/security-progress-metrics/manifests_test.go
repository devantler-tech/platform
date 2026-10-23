package securityprogress_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestSecurityProgressComponentHasAnExplicitActivationBoundary(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	component := filepath.Join(root, "k8s/bases/infrastructure/coroot/components/security-progress")
	for _, enabled := range []bool{false, true} {
		dir := t.TempDir()
		content := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"
		if enabled {
			if err := os.CopyFS(filepath.Join(dir, "component"), os.DirFS(component)); err != nil {
				t.Fatal(err)
			}
			content += "components:\n  - component\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("kubectl", "kustomize", dir).CombinedOutput()
		if err != nil {
			t.Fatalf("enabled=%v: %v %s", enabled, err, out)
		}
		if !enabled {
			if len(strings.TrimSpace(string(out))) != 0 {
				t.Fatalf("disabled component has resources: %s", out)
			}
			continue
		}
		var count int
		for _, doc := range strings.Split(string(out), "\n---\n") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				t.Fatal(err)
			}
			if obj["kind"] == "Service" {
				t.Fatal("private metrics must not acquire a network Service")
			}
			if obj["kind"] == "ClusterRole" {
				for _, rule := range obj["rules"].([]any) {
					for _, verb := range rule.(map[string]any)["verbs"].([]any) {
						if verb != "get" && verb != "list" {
							t.Fatalf("collector gained write authority: %v", rule)
						}
					}
					for _, resource := range rule.(map[string]any)["resources"].([]any) {
						if resource == "*" || resource == "secrets" {
							t.Fatalf("collector gained unbounded reads: %v", rule)
						}
					}
				}
			}
			if obj["kind"] == "Deployment" || obj["kind"] == "CronJob" {
				count++
			}
		}
		if count != 2 {
			t.Fatalf("expected one collector and one reconciler, found %d", count)
		}
	}
}

func TestPackagedScriptsMatchReviewedSources(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("go", "run", "./scripts/generate-coroot-security-config", "--check")
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}
