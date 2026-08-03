package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type manifestMetadata struct {
	Name        string            `yaml:"name"`
	Annotations map[string]string `yaml:"annotations"`
}

type manifestDocument struct {
	Kind     string           `yaml:"kind"`
	Metadata manifestMetadata `yaml:"metadata"`
}

func runAnnotator(t *testing.T, bundle, input string, extraArgs ...string) (string, string, error) {
	t.Helper()

	args := []string{"run", ".", "--bundle", bundle}
	args = append(args, extraArgs...)
	command := exec.Command("go", args...)
	command.Stdin = strings.NewReader(input)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()

	return stdout.String(), stderr.String(), err
}

func checkovFindingsFixture(t *testing.T, bundle, omittedCheck string) string {
	t.Helper()

	clusterRoleName := "cdi-operator-cluster"
	deploymentName := "cdi-operator"
	deploymentNamespace := "cdi"
	if bundle == "kubevirt" {
		clusterRoleName = "kubevirt-operator"
		deploymentName = "virt-operator"
		deploymentNamespace = "kubevirt"
	}

	failedChecks := make([]map[string]string, 0, 8)
	checks := []struct {
		id       string
		resource string
	}{
		{id: "CKV_K8S_155", resource: "ClusterRole.default." + clusterRoleName},
		{id: "CKV_K8S_11", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_13", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_15", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_22", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_38", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_40", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
		{id: "CKV_K8S_43", resource: "Deployment." + deploymentNamespace + "." + deploymentName},
	}
	for _, check := range checks {
		if check.id == omittedCheck {
			continue
		}
		failedChecks = append(failedChecks, map[string]string{
			"check_id": check.id,
			"resource": check.resource,
		})
	}

	report := map[string]any{
		"check_type": "kubernetes",
		"results": map[string]any{
			"failed_checks": failedChecks,
		},
		"summary": map[string]any{
			"parsing_errors": 0,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}
	return string(encoded)
}

func decodeDocuments(t *testing.T, input string) map[string]manifestDocument {
	t.Helper()

	decoder := yaml.NewDecoder(strings.NewReader(input))
	documents := make(map[string]manifestDocument)
	for {
		var document manifestDocument
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode annotated output: %v", err)
		}
		if document.Kind == "" {
			continue
		}
		documents[document.Kind+"/"+document.Metadata.Name] = document
	}

	return documents
}

func bundleFixture(clusterRoleName, deploymentName string) string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: untouched
  annotations:
    owner: platform
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + clusterRoleName + `
rules: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + deploymentName + `
  annotations:
    owner: upstream
spec: {}
`
}

func TestAnnotatorAddsOnlyResourceScopedSuppressions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		bundle          string
		clusterRoleName string
		deploymentName  string
	}{
		{name: "CDI", bundle: "cdi", clusterRoleName: "cdi-operator-cluster", deploymentName: "cdi-operator"},
		{name: "KubeVirt", bundle: "kubevirt", clusterRoleName: "kubevirt-operator", deploymentName: "virt-operator"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := bundleFixture(test.clusterRoleName, test.deploymentName)
			output, stderr, err := runAnnotator(t, test.bundle, input)
			if err != nil {
				t.Fatalf("annotator failed: %v\nstderr: %s", err, stderr)
			}

			documents := decodeDocuments(t, output)
			untouched := documents["ConfigMap/untouched"]
			if len(untouched.Metadata.Annotations) != 1 || untouched.Metadata.Annotations["owner"] != "platform" {
				t.Fatalf("unrelated manifest changed: %#v", untouched.Metadata.Annotations)
			}

			clusterRole := documents["ClusterRole/"+test.clusterRoleName]
			if got := clusterRole.Metadata.Annotations["checkov.io/skip1"]; got != "CKV_K8S_155=Vendored upstream operator RBAC; changes must go through the pinned vendor update path tracked by platform issue 2899." {
				t.Fatalf("ClusterRole suppression = %q", got)
			}
			if len(clusterRole.Metadata.Annotations) != 1 {
				t.Fatalf("ClusterRole received unexpected annotations: %#v", clusterRole.Metadata.Annotations)
			}

			deployment := documents["Deployment/"+test.deploymentName]
			wantChecks := []string{
				"CKV_K8S_11",
				"CKV_K8S_13",
				"CKV_K8S_15",
				"CKV_K8S_22",
				"CKV_K8S_38",
				"CKV_K8S_40",
				"CKV_K8S_43",
			}
			if deployment.Metadata.Annotations["owner"] != "upstream" {
				t.Fatalf("existing annotation was lost: %#v", deployment.Metadata.Annotations)
			}
			for index, check := range wantChecks {
				key := "checkov.io/skip" + strconv.Itoa(index+1)
				want := check + "=Pinned upstream operator deployment; changes must go through the vendor update path tracked by platform issue 2899."
				if got := deployment.Metadata.Annotations[key]; got != want {
					t.Fatalf("%s = %q, want %q", key, got, want)
				}
			}
			if len(deployment.Metadata.Annotations) != 8 {
				t.Fatalf("Deployment annotations = %#v", deployment.Metadata.Annotations)
			}

			secondOutput, secondStderr, err := runAnnotator(t, test.bundle, output)
			if err != nil {
				t.Fatalf("second annotator run failed: %v\nstderr: %s", err, secondStderr)
			}
			if secondOutput != output {
				t.Fatal("annotator is not idempotent")
			}
		})
	}
}

func TestAnnotatorFailsClosedWhenVendorTargetsDrift(t *testing.T) {
	t.Parallel()

	input := bundleFixture("cdi-operator-cluster", "renamed-cdi-operator")
	_, stderr, err := runAnnotator(t, "cdi", input)
	if err == nil {
		t.Fatal("annotator accepted a bundle whose Deployment target drifted")
	}
	if !strings.Contains(stderr, "expected exactly one Deployment/cdi-operator") {
		t.Fatalf("stderr did not name the missing target: %q", stderr)
	}
}

func TestAnnotatorFailsClosedOnConflictingDisposition(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"    owner: upstream",
		"    owner: upstream\n    checkov.io/skip1: \"CKV_K8S_11=some other reason\"",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input)
	if err == nil {
		t.Fatal("annotator replaced a conflicting Checkov disposition")
	}
	if !strings.Contains(stderr, "already has a different disposition") {
		t.Fatalf("stderr did not explain the conflicting disposition: %q", stderr)
	}
}

func TestAnnotatorFailsClosedOnInlineAnnotations(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"  annotations:\n    owner: upstream",
		"  annotations: {owner: upstream}",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input)
	if err == nil {
		t.Fatal("annotator accepted inline metadata.annotations that its line-preserving editor cannot merge")
	}
	if !strings.Contains(stderr, "metadata.annotations must use block mapping style") {
		t.Fatalf("stderr did not explain the unsupported annotation style: %q", stderr)
	}
}

func TestFindingsValidationAcceptsEveryCurrentDisposition(t *testing.T) {
	t.Parallel()

	for _, bundle := range []string{"cdi", "kubevirt"} {
		t.Run(bundle, func(t *testing.T) {
			t.Parallel()
			_, stderr, err := runAnnotator(
				t,
				bundle,
				checkovFindingsFixture(t, bundle, ""),
				"--validate-findings",
			)
			if err != nil {
				t.Fatalf("current dispositions were rejected: %v\nstderr: %s", err, stderr)
			}
		})
	}
}

func TestFindingsValidationRejectsObsoleteDisposition(t *testing.T) {
	t.Parallel()

	_, stderr, err := runAnnotator(
		t,
		"cdi",
		checkovFindingsFixture(t, "cdi", "CKV_K8S_43"),
		"--validate-findings",
	)
	if err == nil {
		t.Fatal("validator accepted a disposition whose finding disappeared upstream")
	}
	if !strings.Contains(stderr, "CKV_K8S_43") || !strings.Contains(stderr, "no longer matches") {
		t.Fatalf("stderr did not name the obsolete disposition: %q", stderr)
	}
}

func TestCheckovReportValidationRejectsParsingErrors(t *testing.T) {
	t.Parallel()

	report := map[string]any{
		"check_type": "kubernetes",
		"results": map[string]any{
			"failed_checks": []any{},
		},
		"summary": map[string]any{
			"parsing_errors": 1,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}

	_, stderr, err := runAnnotator(t, "cdi", string(encoded), "--validate-report")
	if err == nil {
		t.Fatal("validator accepted a Checkov report with parsing errors")
	}
	if !strings.Contains(stderr, "reported 1 parsing error") {
		t.Fatalf("stderr did not name the parsing error count: %q", stderr)
	}
}

func TestAnnotatorRejectsUnknownBundle(t *testing.T) {
	t.Parallel()

	_, stderr, err := runAnnotator(t, "other", bundleFixture("role", "deployment"))
	if err == nil {
		t.Fatal("annotator accepted an unknown bundle")
	}
	if !strings.Contains(stderr, "unsupported bundle") {
		t.Fatalf("stderr did not explain the rejected bundle: %q", stderr)
	}
}
