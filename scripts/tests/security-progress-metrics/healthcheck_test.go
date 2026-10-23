package securityprogress_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkSnapshotHealth(t *testing.T, metrics string, age time.Duration, missing, requireProject bool) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../../k8s/bases/infrastructure/coroot/components/security-progress/healthcheck.sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "metrics")
	if !missing {
		if err := os.WriteFile(path, []byte(metrics), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := time.Now().Add(-age)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{script}
	if requireProject {
		args = append(args, "--require-project")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Env = append(os.Environ(), "METRICS_FILE="+path)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestSnapshotHealthRejectsMissingOrOldPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics string
		age     time.Duration
		missing bool
		wantOK  bool
	}{
		{name: "fresh", metrics: "platform_security_smoke 1\n", age: time.Second, wantOK: true},
		{name: "fresh_source_failure_is_reporting", metrics: "platform_security_collection_success{surface=\"policy\"} 0\n", age: time.Second, wantOK: true},
		{name: "within_freshness_window", metrics: "platform_security_smoke 1\n", age: 899 * time.Second, wantOK: true},
		{name: "missing", missing: true},
		{name: "empty"},
		{name: "stale", metrics: "platform_security_smoke 1\n", age: 901 * time.Second},
		{name: "future", metrics: "platform_security_smoke 1\n", age: -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := checkSnapshotHealth(t, tc.metrics, tc.age, tc.missing, false)
			if (err == nil) != tc.wantOK {
				t.Fatalf("snapshot health: got err=%v output=%q; want healthy=%v", err, output, tc.wantOK)
			}
		})
	}
}

func TestSnapshotReadinessRequiresBothSourcesBoundToOneValidatedProject(t *testing.T) {
	policy := "platform_security_collection_success{coroot_project_id=\"synthetic-platform_1\",surface=\"policy\"} 1\n"
	kubescape := "platform_security_collection_success{coroot_project_id=\"synthetic-platform_1\",surface=\"kubescape\"} 1\n"
	for _, tc := range []struct {
		name    string
		metrics string
		wantOK  bool
	}{
		{"both_scoped_sources", policy + kubescape, true},
		{"both_source_failures_are_reporting", strings.ReplaceAll(policy+kubescape, "} 1", "} 0"), true},
		{"one_source_failure_is_reporting", strings.ReplaceAll(policy, "} 1", "} 0") + kubescape, true},
		{"label_order", "platform_security_collection_success{surface=\"policy\",coroot_project_id=\"synthetic-platform_1\"} 1\n" + kubescape, true},
		{"missing_policy", kubescape, false},
		{"missing_kubescape", policy, false},
		{"unscoped", "platform_security_collection_success{surface=\"policy\"} 1\nplatform_security_collection_success{surface=\"kubescape\"} 1\n", false},
		{"one_unscoped", policy + "platform_security_collection_success{surface=\"kubescape\"} 1\n", false},
		{"empty_project", strings.ReplaceAll(policy+kubescape, "synthetic-platform_1", ""), false},
		{"illegal_project", strings.ReplaceAll(policy+kubescape, "synthetic-platform_1", "invalid/project"), false},
		{"oversized_project", strings.ReplaceAll(policy+kubescape, "synthetic-platform_1", strings.Repeat("x", 129)), false},
		{"maximum_length_project", strings.ReplaceAll(policy+kubescape, "synthetic-platform_1", strings.Repeat("x", 128)), true},
		{"different_projects", policy + strings.ReplaceAll(kubescape, "synthetic-platform_1", "another-project"), false},
		{"duplicate_policy", policy + policy + kubescape, false},
		{"duplicate_kubescape", policy + kubescape + kubescape, false},
		{"invalid_source_value", policy + strings.ReplaceAll(kubescape, "} 1", "} 2"), false},
		{"unrelated_scoped_metrics", "platform_security_smoke{coroot_project_id=\"synthetic-platform_1\"} 1\n", false},
		{"comment_is_not_source", "# " + strings.TrimSuffix(policy, "\n") + "\n" + kubescape, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := checkSnapshotHealth(t, tc.metrics, time.Second, false, true)
			if (err == nil) != tc.wantOK {
				t.Fatalf("scoped readiness: got err=%v output=%q; want healthy=%v", err, output, tc.wantOK)
			}
		})
	}
}

func TestSnapshotReadinessAlsoRejectsOldOrFuturePublication(t *testing.T) {
	metrics := "platform_security_collection_success{coroot_project_id=\"synthetic-platform\",surface=\"policy\"} 1\n" +
		"platform_security_collection_success{coroot_project_id=\"synthetic-platform\",surface=\"kubescape\"} 1\n"
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{"stale", 901 * time.Second},
		{"future", -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := checkSnapshotHealth(t, metrics, tc.age, false, true)
			if err == nil {
				t.Fatalf("scope alone bypassed publication freshness: output=%q", output)
			}
		})
	}
}
