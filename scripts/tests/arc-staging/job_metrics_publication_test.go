package arcstaging_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMetricsHookSurvivesPublishedArtifactRender(t *testing.T) {
	// The real publisher includes manifest files, not arbitrary checkout files.
	// Render that artifact boundary so a checkout-only dependency cannot pass.
	component := filepath.Join(repoRoot, metricsComponent)
	published := t.TempDir()
	files, err := os.ReadDir(component)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		switch strings.ToLower(filepath.Ext(file.Name())) {
		case ".yaml", ".yml", ".json":
			data, err := os.ReadFile(filepath.Join(component, file.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(published, file.Name()), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	rendered, err := exec.Command("kubectl", "kustomize", published).CombinedOutput()
	if err != nil {
		t.Fatalf("published metrics dependencies cannot render: %v\n%s", err, rendered)
	}
	script, err := os.ReadFile(filepath.Join(repoRoot, metricsScript))
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	var config, release map[string]any
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object["kind"] == "ConfigMap" {
			if config != nil {
				t.Fatal("ambiguous metrics ConfigMap")
			}
			config = object
		}
		if object["kind"] == "HelmRelease" {
			release = object
		}
	}
	if config == nil || release == nil {
		t.Fatal("published artifact lacks the metrics ConfigMap or runner release")
	}
	equal(t, field(t, config, "immutable"), true)
	equal(t, field(t, config, "metadata", "annotations", "kustomize.toolkit.fluxcd.io/substitute"), "disabled")
	equal(t, field(t, config, "data", "job-metrics.sh"), string(script))
	volumes := field(t, release, "spec", "values", "template", "spec", "volumes").([]any)
	equal(t, field(t, volumes[2], "configMap", "name"), field(t, config, "metadata", "name"))
	equal(t, field(t, volumes[2], "configMap", "defaultMode"), 365)
}
