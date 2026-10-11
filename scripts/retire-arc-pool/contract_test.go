package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionRendersTheFailClosedFence(t *testing.T) {
	root := filepath.Join("..", "..")
	command := exec.Command("kubectl", "kustomize", "k8s/providers/hetzner/infrastructure")
	command.Dir = root
	data, err := command.Output()
	if err != nil {
		t.Fatal("cannot render production", err)
	}
	d := yaml.NewDecoder(strings.NewReader(string(data)))
	count := 0
	for {
		var doc map[string]any
		if err := d.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		metadata, _ := doc["metadata"].(map[string]any)
		if doc["kind"] != "ClusterPolicy" || metadata["name"] != "restrict-arc-retirement" {
			continue
		}
		count++
		spec := doc["spec"].(map[string]any)
		if spec["background"] != false || spec["webhookConfiguration"].(map[string]any)["failurePolicy"] != "Fail" {
			t.Fatal("rendered fence fails open")
		}
		rules := spec["rules"].([]any)
		if len(rules) != 8 {
			t.Fatal("missing fence rule")
		}
		for _, rule := range rules {
			validate := rule.(map[string]any)["validate"].(map[string]any)
			if validate["failureAction"] != "Enforce" || validate["allowExistingViolations"] != false {
				t.Fatal("update or admission bypass")
			}
		}
	}
	if count != 1 {
		t.Fatal("production must render exactly one retirement fence")
	}
}
func TestNativeRetirementPrecedesUnchangedAbsenceAndPublicationGuards(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "actions", "deploy-prod", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, stage := range []string{"before-publish", "after-reconcile"} {
		retire := strings.Index(body, "bash scripts/retire-arc-pool.sh "+stage)
		guard := strings.Index(body, "go run ./scripts/guard-arc-recovery "+stage)
		if retire < 0 || guard <= retire {
			t.Fatal("retirement must precede the complete absence guard", stage)
		}
	}
	if strings.Count(body, "bash scripts/retire-arc-pool.sh") != 2 {
		t.Fatal("unexpected retirement invocation")
	}
}
