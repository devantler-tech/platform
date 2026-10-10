package main

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const metricsComponent = "k8s/bases/infrastructure/actions-runners"

func field(t *testing.T, value any, path ...string) any {
	t.Helper()
	for _, key := range path {
		mapping, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s: expected a mapping, got %T", strings.Join(path, "."), value)
		}
		value, ok = mapping[key]
		if !ok {
			t.Fatalf("missing %s", strings.Join(path, "."))
		}
	}
	return value
}

func equal(t *testing.T, value, want any) {
	t.Helper()
	if value != want {
		t.Fatalf("got %#v, want %#v", value, want)
	}
}

func TestMetricsHookSurvivesPublishedArtifactRender(t *testing.T) {
	// The real publisher includes manifest files, not arbitrary checkout files.
	// Render that artifact boundary so a checkout-only dependency cannot pass.
	component := filepath.Join(repoRoot, metricsComponent)
	published := t.TempDir()
	err := filepath.WalkDir(component, func(path string, file fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if file.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml", ".json":
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(component, path)
			if err != nil {
				return err
			}
			destination := filepath.Join(published, relative)
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				return err
			}
			return os.WriteFile(destination, data, 0600)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
