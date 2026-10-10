package ocichartsources

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderedRBACChartResolver(t *testing.T) {
	for _, tool := range []string{"jq", "yq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("chart-rendering regression test requires %s: %v", tool, err)
		}
	}
	cases := []struct {
		name, wantURL, failure string
		mutate                 func(map[string]any, map[string]any)
		duplicate              bool
	}{
		{name: "OCI chartRef", wantURL: "oci://example.com/charts/kro"},
		{name: "explicit source namespace", wantURL: "oci://example.com/charts/kro", mutate: func(r, s map[string]any) {
			object(object(r, "spec"), "chartRef")["namespace"] = "charts"
			object(s, "metadata")["namespace"] = "charts"
		}},
		{name: "HTTP HelmRepository", wantURL: "https://example.com/charts", mutate: func(r, s map[string]any) {
			useHelmRepository(r, s, "https://example.com/charts")
		}},
		{name: "legacy OCI HelmRepository", wantURL: "oci://example.com/charts/kro", mutate: func(r, s map[string]any) {
			useHelmRepository(r, s, "oci://example.com/charts")
		}},
		{name: "missing source", failure: "exactly one", mutate: func(_, s map[string]any) { object(s, "metadata")["name"] = "other" }},
		{name: "duplicate source", failure: "exactly one", duplicate: true},
		{name: "wrong namespace", failure: "exactly one", mutate: func(_, s map[string]any) { object(s, "metadata")["namespace"] = "other" }},
		{name: "two chart definitions", failure: "one chart definition", mutate: func(r, _ map[string]any) { object(r, "spec")["chart"] = map[string]any{} }},
		{name: "unsupported chartRef", failure: "OCIRepository", mutate: func(r, _ map[string]any) { object(object(r, "spec"), "chartRef")["kind"] = "HelmChart" }},
		{name: "missing source name", failure: "source identity", mutate: func(r, _ map[string]any) { delete(object(object(r, "spec"), "chartRef"), "name") }},
		{name: "floating tag", failure: "exact version", mutate: func(_, s map[string]any) { object(object(s, "spec"), "ref")["tag"] = "latest" }},
		{name: "multiple selectors", failure: "exact version", mutate: func(_, s map[string]any) { object(object(s, "spec"), "ref")["semver"] = ">=0.9.0" }},
		{name: "non-OCI artifact", failure: "OCI chart URL", mutate: func(_, s map[string]any) { object(s, "spec")["url"] = "https://example.com/charts/kro" }},
		{name: "missing chart layer", failure: "copy the Helm chart layer", mutate: func(_, s map[string]any) { delete(object(s, "spec"), "layerSelector") }},
		{name: "extracted chart layer", failure: "copy the Helm chart layer", mutate: func(_, s map[string]any) { object(object(s, "spec"), "layerSelector")["operation"] = "extract" }},
		{name: "wrong media type", failure: "copy the Helm chart layer", mutate: func(_, s map[string]any) {
			object(object(s, "spec"), "layerSelector")["mediaType"] = "application/octet-stream"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := map[string]any{
				"metadata": map[string]any{"name": "kro", "namespace": "kro-system"},
				"spec":     map[string]any{"chartRef": map[string]any{"kind": "OCIRepository", "name": "kro"}},
			}
			source := map[string]any{
				"kind": "OCIRepository", "metadata": map[string]any{"name": "kro", "namespace": "kro-system"},
				"spec": map[string]any{
					"url": "oci://example.com/charts/kro", "ref": map[string]any{"tag": "0.9.2"},
					"layerSelector": map[string]any{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "operation": "copy"},
				},
			}
			if tc.mutate != nil {
				tc.mutate(release, source)
			}
			work := t.TempDir()
			releaseFile, sourceFile := filepath.Join(work, "release.json"), filepath.Join(work, "overlay.yaml")
			writeJSONFixture(t, releaseFile, release, false)
			writeJSONFixture(t, sourceFile, source, tc.duplicate)
			command := exec.Command("bash", filepath.Join(repositoryRoot(t), "scripts/tests/resolve-rendered-chart.sh"), releaseFile, sourceFile)
			output, err := command.CombinedOutput()
			if tc.failure != "" {
				if err == nil || !strings.Contains(string(output), tc.failure) {
					t.Fatalf("unsafe source must fail for %q: %v\n%s", tc.failure, err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("supported source must resolve: %v\n%s", err, output)
			}
			var result struct{ Chart, Version, URL string }
			if err := json.Unmarshal(output, &result); err != nil {
				t.Fatal(err)
			}
			if result.Chart != "kro" || result.Version != "0.9.2" || result.URL != tc.wantURL {
				t.Fatalf("resolved the wrong chart: %+v", result)
			}
		})
	}
}

func useHelmRepository(release, source map[string]any, url string) {
	spec := object(release, "spec")
	delete(spec, "chartRef")
	spec["chart"] = map[string]any{"spec": map[string]any{
		"chart": "kro", "version": "0.9.2", "sourceRef": map[string]any{"kind": "HelmRepository", "name": "kro"},
	}}
	source["kind"] = "HelmRepository"
	source["spec"] = map[string]any{"url": url}
}

func writeJSONFixture(t *testing.T, path string, doc map[string]any, duplicate bool) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		data = append(append(data, []byte("\n---\n")...), data...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRenderedRBACUsesChartResolver(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/tests/test-chart-rendered-rbac-policy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `bash "${repo_root}/scripts/tests/resolve-rendered-chart.sh"`) {
		t.Fatal("actual RBAC renderer must execute the tested chart resolver")
	}
}
