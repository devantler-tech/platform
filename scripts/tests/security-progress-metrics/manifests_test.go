package securityprogress_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestPackagedDashboardPassesEmbeddedJSONGate(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", "scripts/validate-embedded-json.sh")
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("dashboard JSON is not readable by the manifest gate: %v %s", err, out)
	}
	component := filepath.Join(root, "k8s/bases/infrastructure/coroot/components/security-progress")
	b, err := os.ReadFile(filepath.Join(component, "config-map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(component, "dashboards.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(source, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.ReplaceAll(config.Data["dashboards.json"], "$${", "${")), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("packaging changed dashboard semantics")
	}
}
