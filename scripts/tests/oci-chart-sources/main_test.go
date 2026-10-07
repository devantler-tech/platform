package ocichartsources

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type chartSource struct {
	directory, name, namespace, url string
}

var chartSources = []chartSource{
	{"k8s/bases/infrastructure/controllers/flux-operator", "flux-operator", "flux-system", "oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator"},
	{"k8s/bases/infrastructure/controllers/kro", "kro", "kro-system", "oci://registry.k8s.io/kro/charts/kro"},
	{"k8s/bases/apps/homepage", "homepage", "homepage", "oci://ghcr.io/m0nsterrr/helm-charts/homepage"},
	{"k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer", "origin-ca-issuer", "cert-manager", "oci://ghcr.io/cloudflare/origin-ca-issuer-charts/origin-ca-issuer"},
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the checked-out manifests")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}

func readDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func object(doc map[string]any, key string) map[string]any {
	value, _ := doc[key].(map[string]any)
	return value
}

func validateSource(source, release map[string]any, expected chartSource) error {
	if source["apiVersion"] != "source.toolkit.fluxcd.io/v1" || source["kind"] != "OCIRepository" {
		return fmt.Errorf("chart source must expose OCIRepository readiness")
	}
	for _, doc := range []map[string]any{source, release} {
		metadata := object(doc, "metadata")
		if metadata["name"] != expected.name || metadata["namespace"] != expected.namespace {
			return fmt.Errorf("chart source or release identity changed")
		}
	}
	if release["apiVersion"] != "helm.toolkit.fluxcd.io/v2" || release["kind"] != "HelmRelease" {
		return fmt.Errorf("release must remain a v2 HelmRelease")
	}
	spec := object(source, "spec")
	if spec["url"] != expected.url || spec["type"] != nil {
		return fmt.Errorf("chart URL must identify the same chart artifact")
	}
	interval, _ := spec["interval"].(string)
	if duration, err := time.ParseDuration(interval); err != nil || duration <= 0 {
		return fmt.Errorf("chart source needs a positive reconciliation interval")
	}
	ref := object(spec, "ref")
	tag, _ := ref["tag"].(string)
	if len(ref) != 1 || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(tag) {
		return fmt.Errorf("chart source must retain an exact version tag, not a floating selector")
	}
	layer := object(spec, "layerSelector")
	if layer["mediaType"] != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" || layer["operation"] != "copy" {
		return fmt.Errorf("chart source must copy the Helm chart layer")
	}
	releaseSpec := object(release, "spec")
	chartRef := object(releaseSpec, "chartRef")
	if releaseSpec["chart"] != nil || chartRef["kind"] != "OCIRepository" || chartRef["name"] != expected.name ||
		(chartRef["namespace"] != nil && chartRef["namespace"] != expected.namespace) {
		return fmt.Errorf("release must reference only its same-namespace OCI chart source")
	}
	return nil
}

func TestCheckedInOCIChartSources(t *testing.T) {
	root := repositoryRoot(t)
	for _, expected := range chartSources {
		t.Run(expected.name, func(t *testing.T) {
			directory := filepath.Join(root, expected.directory)
			source := readDocument(t, filepath.Join(directory, "helm-repository.yaml"))
			release := readDocument(t, filepath.Join(directory, "helm-release.yaml"))
			if err := validateSource(source, release, expected); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationContractRejectsUnsafeInputs(t *testing.T) {
	expected := chartSources[0]
	root := filepath.Join(repositoryRoot(t), expected.directory)
	source := readDocument(t, filepath.Join(root, "helm-repository.yaml"))
	release := readDocument(t, filepath.Join(root, "helm-release.yaml"))
	if err := validateSource(source, release, expected); err != nil {
		t.Fatalf("negative controls require a valid checked-in positive control: %v", err)
	}
	cases := []struct {
		name, reason string
		mutate       func(map[string]any, map[string]any)
	}{
		{"legacy source", "readiness", func(s, _ map[string]any) { s["kind"] = "HelmRepository" }},
		{"wrong artifact", "same chart", func(s, _ map[string]any) { object(s, "spec")["url"] = "oci://ghcr.io/example/other" }},
		{"floating tag", "exact version", func(s, _ map[string]any) { object(object(s, "spec"), "ref")["tag"] = "latest" }},
		{"mixed selectors", "exact version", func(s, _ map[string]any) { object(object(s, "spec"), "ref")["semver"] = ">=1.0.0" }},
		{"missing layer", "Helm chart layer", func(s, _ map[string]any) { delete(object(s, "spec"), "layerSelector") }},
		{"wrong reference", "same-namespace", func(_, r map[string]any) { object(object(r, "spec"), "chartRef")["name"] = "other" }},
		{"two chart definitions", "same-namespace", func(_, r map[string]any) { object(r, "spec")["chart"] = map[string]any{} }},
		{"zero interval", "positive reconciliation", func(s, _ map[string]any) { object(s, "spec")["interval"] = "0s" }},
		{"extracted chart", "Helm chart layer", func(s, _ map[string]any) { object(object(s, "spec"), "layerSelector")["operation"] = "extract" }},
		{"cross-namespace reference", "same-namespace", func(_, r map[string]any) { object(object(r, "spec"), "chartRef")["namespace"] = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clone := func(doc map[string]any) map[string]any {
				data, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				var copied map[string]any
				if err := json.Unmarshal(data, &copied); err != nil {
					t.Fatal(err)
				}
				return copied
			}
			s, r := clone(source), clone(release)
			tc.mutate(s, r)
			if err := validateSource(s, r, expected); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("unsafe migration not rejected for its intended reason: %v", err)
			}
		})
	}
}

func TestFluxOperatorVersionRemainsDiscoverable(t *testing.T) {
	root := repositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github/renovate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		MinimumReleaseAge string `json:"minimumReleaseAge"`
		CustomManagers    []struct {
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
			DepNameTemplate     string   `json:"depNameTemplate"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.MinimumReleaseAge != "7 days" {
		t.Fatal("migration must preserve dependency update stability policy")
	}
	expected := chartSources[0]
	path := expected.directory + "/helm-repository.yaml"
	source := readDocument(t, filepath.Join(root, path))
	tag := object(object(source, "spec"), "ref")["tag"]
	text, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, manager := range config.CustomManagers {
		if manager.DepNameTemplate != strings.TrimPrefix(expected.url, "oci://") {
			continue
		}
		count++
		if manager.DatasourceTemplate != "docker" || len(manager.ManagerFilePatterns) != 1 || len(manager.MatchStrings) != 1 {
			t.Fatal("flux-operator must retain one registry version manager")
		}
		pattern, err := regexp.Compile(strings.Trim(manager.ManagerFilePatterns[0], "/"))
		if err != nil || !pattern.MatchString(path) {
			t.Fatalf("version manager must read the migrated source: %v", err)
		}
		matcher, err := regexp.Compile(manager.MatchStrings[0])
		if err != nil {
			t.Fatal(err)
		}
		matches := matcher.FindAllStringSubmatch(string(text), -1)
		index := matcher.SubexpIndex("currentValue")
		if len(matches) != 1 || index < 1 || matches[0][index] != tag {
			t.Fatal("version manager must extract exactly the migrated chart's version")
		}
	}
	if count != 1 {
		t.Fatalf("expected one flux-operator manager, found %d", count)
	}
}

func TestCIExecutesMigrationContract(t *testing.T) {
	workflow := readDocument(t, filepath.Join(repositoryRoot(t), ".github/workflows/ci.yaml"))
	steps, _ := object(object(workflow, "jobs"), "changes")["steps"].([]any)
	count := 0
	for _, raw := range steps {
		step, _ := raw.(map[string]any)
		if step["run"] != "go test ./scripts/tests/oci-chart-sources" {
			continue
		}
		count++
		if step["if"] != nil || step["continue-on-error"] != nil {
			t.Fatal("migration contract must run unconditionally and fail CI on regression")
		}
	}
	if count != 1 {
		t.Fatalf("CI must execute the migration contract exactly once; found %d", count)
	}
}
