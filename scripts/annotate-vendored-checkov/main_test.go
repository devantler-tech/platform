package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	failedChecks := make([]map[string]any, 0, len(checks))
	for _, check := range checks {
		if check.id == omittedCheck {
			continue
		}
		kind := "Deployment"
		name := deploymentName
		if check.id == "CKV_K8S_155" {
			kind = "ClusterRole"
			name = clusterRoleName
		}
		evaluatedKeys, known := expectedEvaluatedKeys(check.id)
		if !known {
			t.Fatalf("fixture check %s has no expected evaluated keys", check.id)
		}
		failedChecks = append(failedChecks, map[string]any{
			"check_id":   check.id,
			"resource":   check.resource,
			"code_block": fixtureCodeBlock(kind, name),
			"check_result": map[string]any{
				"evaluated_keys": evaluatedKeys,
			},
		})
	}

	report := map[string]any{
		"check_type": "kubernetes",
		"results": map[string]any{
			"failed_checks": failedChecks,
		},
		"summary": map[string]any{
			"parsing_errors": 0,
			"failed":         len(failedChecks),
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}
	return string(encoded)
}

func fixtureCodeBlock(kind, name string) [][]any {
	return [][]any{
		{1, "kind: " + kind},
		{2, "metadata:"},
		{3, "  name: " + name},
		{4, "container: operator"},
	}
}

func fixtureTargets(t *testing.T, bundle string) []targetSpec {
	t.Helper()

	targets := append([]targetSpec(nil), bundleTargets[bundle]...)
	for index := range targets {
		fingerprint, err := codeBlockFingerprint(fixtureCodeBlock(targets[index].kind, targets[index].name))
		if err != nil {
			t.Fatalf("fingerprint fixture target: %v", err)
		}
		targets[index].resourceFingerprint = fingerprint
	}
	return targets
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

func cdiOperatorBundleFixture() string {
	return strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"spec: {}",
		`spec:
  template:
    spec:
      containers:
      - name: cdi-operator
        image: quay.io/kubevirt/cdi-operator:v1.65.0`,
		1,
	)
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
			err := validateCheckovFindings(
				[]byte(checkovFindingsFixture(t, bundle, "")),
				fixtureTargets(t, bundle),
			)
			if err != nil {
				t.Fatalf("current dispositions were rejected: %v", err)
			}
		})
	}
}

func TestFindingsValidationRejectsObsoleteDisposition(t *testing.T) {
	t.Parallel()

	err := validateCheckovFindings(
		[]byte(checkovFindingsFixture(t, "cdi", "CKV_K8S_43")),
		fixtureTargets(t, "cdi"),
	)
	if err == nil {
		t.Fatal("validator accepted a disposition whose finding disappeared upstream")
	}
	if !strings.Contains(err.Error(), "CKV_K8S_43") || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("error did not name the obsolete disposition: %v", err)
	}
}

func TestFindingsValidationRejectsMovedViolation(t *testing.T) {
	t.Parallel()

	report := strings.Replace(
		checkovFindingsFixture(t, "cdi", ""),
		"spec/template/spec/containers/[0]/image",
		"spec/template/spec/containers/[1]/image",
		1,
	)
	err := validateCheckovFindings([]byte(report), fixtureTargets(t, "cdi"))
	if err == nil {
		t.Fatal("validator accepted a reviewed check after its violation moved to another container")
	}
	if !strings.Contains(err.Error(), "evaluated keys changed") {
		t.Fatalf("error did not explain the changed finding evidence: %v", err)
	}
}

func TestFindingsValidationRejectsChangedResourceFingerprint(t *testing.T) {
	t.Parallel()

	report := strings.Replace(
		checkovFindingsFixture(t, "cdi", ""),
		"container: operator",
		"container: sidecar",
		1,
	)
	err := validateCheckovFindings([]byte(report), fixtureTargets(t, "cdi"))
	if err == nil {
		t.Fatal("validator accepted changed resource evidence for an empty-key disposition")
	}
	if !strings.Contains(err.Error(), "resource fingerprint changed") {
		t.Fatalf("error did not explain the changed resource evidence: %v", err)
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
			"failed":         0,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}

	_, stderr, err := runAnnotator(
		t,
		"cdi",
		string(encoded),
		"--validate-report",
		"--framework",
		"kubernetes",
	)
	if err == nil {
		t.Fatal("validator accepted a Checkov report with parsing errors")
	}
	if !strings.Contains(stderr, "reported 1 parsing error") {
		t.Fatalf("stderr did not name the parsing error count: %q", stderr)
	}
}

func TestCheckovReportValidationRequiresExpectedFramework(t *testing.T) {
	t.Parallel()

	report := map[string]any{
		"check_type": "kubernetes",
		"results": map[string]any{
			"failed_checks": []any{},
		},
		"summary": map[string]any{
			"parsing_errors": 0,
			"failed":         0,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}

	_, stderr, err := runAnnotator(
		t,
		"cdi",
		string(encoded),
		"--validate-report",
		"--framework",
		"secrets",
	)
	if err == nil {
		t.Fatal("validator accepted a report from the wrong Checkov framework")
	}
	if !strings.Contains(stderr, "expected exactly one secrets report") {
		t.Fatalf("stderr did not name the missing framework: %q", stderr)
	}
}

func TestCheckovReportValidationRejectsEmptySecretsSummary(t *testing.T) {
	t.Parallel()

	report := `{"passed":0,"failed":0,"skipped":0,"parsing_errors":0,"resource_count":0,"checkov_version":"3.3.2"}`
	_, stderr, err := runAnnotator(
		t,
		"cdi",
		report,
		"--validate-report",
		"--framework",
		"secrets",
	)
	if err == nil {
		t.Fatal("validator accepted a framework-less empty secrets summary")
	}
	if !strings.Contains(stderr, "expected exactly one secrets report") {
		t.Fatalf("stderr did not require explicit secrets report identity: %q", stderr)
	}
}

func TestCheckovReportValidationAcceptsExactSecretsCanary(t *testing.T) {
	t.Parallel()

	report := map[string]any{
		"check_type": "secrets",
		"results": map[string]any{
			"failed_checks": []map[string]any{{
				"check_id":  "CKV_SECRET_2",
				"file_path": "/tmp/checkov-secrets-canary.txt",
				"code_block": [][]any{{
					1,
					"aws_access_key_id: AKIAQ**********\n",
				}},
			}},
		},
		"summary": map[string]any{
			"parsing_errors": 0,
			"failed":         1,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}

	_, stderr, err := runAnnotator(
		t,
		"cdi",
		string(encoded),
		"--validate-report",
		"--framework",
		"secrets",
		"--require-secrets-canary",
	)
	if err != nil {
		t.Fatalf("validator rejected the exact secrets canary: %v\nstderr: %s", err, stderr)
	}
}

func TestFindingsValidationRejectsUnknownConfiguredCheck(t *testing.T) {
	t.Parallel()

	targets := fixtureTargets(t, "cdi")
	targets[0].checks = append(targets[0].checks, "CKV_K8S_UNKNOWN")
	err := validateCheckovFindings(
		[]byte(checkovFindingsFixture(t, "cdi", "")),
		targets,
	)
	if err == nil {
		t.Fatal("validator accepted a configured check without reviewed evaluated keys")
	}
	if !strings.Contains(err.Error(), "CKV_K8S_UNKNOWN has no reviewed evaluated keys") {
		t.Fatalf("error did not name the unknown configured check: %v", err)
	}
}

func TestCheckovReportValidationRejectsFrameworklessKubernetesSummary(t *testing.T) {
	t.Parallel()

	report := `{"passed":0,"failed":0,"skipped":0,"parsing_errors":0,"resource_count":0,"checkov_version":"3.3.2"}`
	_, stderr, err := runAnnotator(
		t,
		"cdi",
		report,
		"--validate-report",
		"--framework",
		"kubernetes",
	)
	if err == nil {
		t.Fatal("validator treated a framework-less summary as proof of a Kubernetes scan")
	}
	if !strings.Contains(stderr, "expected exactly one kubernetes report") {
		t.Fatalf("stderr did not require explicit Kubernetes report identity: %q", stderr)
	}
}

func TestCheckovReportValidationRejectsSoftFailedFindings(t *testing.T) {
	t.Parallel()

	report := map[string]any{
		"check_type": "kubernetes",
		"results": map[string]any{
			"failed_checks": []map[string]string{{
				"check_id": "CKV_K8S_99",
				"resource": "Deployment.default.new-upstream-risk",
			}},
		},
		"summary": map[string]any{
			"parsing_errors": 0,
			"failed":         1,
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode Checkov fixture: %v", err)
	}

	_, stderr, err := runAnnotator(
		t,
		"cdi",
		string(encoded),
		"--validate-report",
		"--framework",
		"kubernetes",
	)
	if err == nil {
		t.Fatal("validator accepted undispositioned findings from a soft-failed Checkov run")
	}
	if !strings.Contains(stderr, "reported 1 failed check") {
		t.Fatalf("stderr did not name the failed-check count: %q", stderr)
	}
}

func TestSourceValidationRejectsUpstreamCheckovSuppressions(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"    owner: platform",
		"    owner: platform\n    checkov.io/skip1: \"CKV_K8S_99=upstream suppression\"",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted an upstream Checkov suppression")
	}
	if !strings.Contains(stderr, "upstream Checkov suppression checkov.io/skip1") {
		t.Fatalf("stderr did not name the upstream suppression: %q", stderr)
	}
}

func TestSourceValidationRejectsInlineCheckovSuppressions(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"    owner: platform",
		"    owner: platform # checkov:skip=CKV_SECRET_6:upstream suppression",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted an inline upstream Checkov suppression")
	}
	if !strings.Contains(stderr, "inline Checkov suppression") {
		t.Fatalf("stderr did not name the inline upstream suppression: %q", stderr)
	}
}

func TestSourceValidationRejectsCheckovSuppressionsInBlockScalars(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"    owner: platform",
		"    owner: platform\ndata:\n  embedded.yaml: |\n    password: value # checkov:skip=CKV_SECRET_6:upstream suppression",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted a Checkov suppression inside a block scalar")
	}
	if !strings.Contains(stderr, "inline Checkov suppression") {
		t.Fatalf("stderr did not name the block-scalar suppression: %q", stderr)
	}
}

func TestSourceValidationRejectsWorkloadOutsideProtectedNamespace(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"  name: cdi-operator\n  annotations:",
		"  name: cdi-operator\n  namespace: other\n  annotations:",
		1,
	)
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted a workload outside the protected namespace")
	}
	if !strings.Contains(stderr, "Deployment/cdi-operator uses namespace other, want cdi") {
		t.Fatalf("stderr did not name the unprotected workload namespace: %q", stderr)
	}
}

func TestSourceValidationRejectsWorkloadWithoutMetadata(t *testing.T) {
	t.Parallel()

	input := `apiVersion: apps/v1
kind: Deployment
spec: {}
`
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted a workload without metadata")
	}
	if !strings.Contains(stderr, "Deployment metadata is missing") {
		t.Fatalf("stderr did not name the missing workload metadata: %q", stderr)
	}
}

func TestSourceValidationRejectsYAMLMergeSuppression(t *testing.T) {
	t.Parallel()

	input := `apiVersion: v1
kind: ConfigMap
metadata:
  name: merged-suppression
  annotations:
    <<: &suppression
      checkov.io/skip1: CKV_K8S_99=upstream suppression
`
	_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
	if err == nil {
		t.Fatal("source validator accepted a Checkov suppression inherited through a YAML merge")
	}
	if !strings.Contains(stderr, "YAML merge key") {
		t.Fatalf("stderr did not name the YAML merge key: %q", stderr)
	}
}

func TestSourceValidationRejectsNestedListWorkloadOutsideProtectedNamespace(t *testing.T) {
	t.Parallel()

	for _, listKind := range []string{"List", "DeploymentList"} {
		t.Run(listKind, func(t *testing.T) {
			t.Parallel()
			input := `apiVersion: v1
kind: ` + listKind + `
items:
- apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: nested-operator
    namespace: other
`
			_, stderr, err := runAnnotator(t, "cdi", input, "--validate-source")
			if err == nil {
				t.Fatal("source validator accepted a nested workload outside the protected namespace")
			}
			if !strings.Contains(stderr, "Deployment/nested-operator uses namespace other, want cdi") {
				t.Fatalf("stderr did not name the unprotected nested workload namespace: %q", stderr)
			}
		})
	}
}

func TestAnnotatedValidationAcceptsOnlyConfiguredDispositions(t *testing.T) {
	t.Parallel()

	input := cdiOperatorBundleFixture()
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	if err := validateAnnotatedBundle([]byte(annotated), bundleTargets["cdi"]); err != nil {
		t.Fatalf("configured dispositions were rejected: %v", err)
	}
}

func TestPinnedSourceValidationAcceptsAnnotatedExactSource(t *testing.T) {
	t.Parallel()

	input := cdiOperatorBundleFixture()
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	expectedSHA256 := fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
	if err := validatePinnedSource(
		[]byte(annotated),
		bundleTargets["cdi"],
		expectedSHA256,
		"v1.65.0",
	); err != nil {
		t.Fatalf("exact pinned source was rejected: %v", err)
	}
}

func TestPinnedSourceStrippingPreservesMetadataComment(t *testing.T) {
	t.Parallel()

	input := strings.Replace(
		bundleFixture("cdi-operator-cluster", "cdi-operator"),
		"kind: ClusterRole\nmetadata:\n  name:",
		"kind: ClusterRole\nmetadata:\n  # upstream metadata comment\n  name:",
		1,
	)
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	stripped, err := stripConfiguredDispositions(annotated, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("strip configured dispositions: %v", err)
	}
	if stripped != input {
		t.Fatalf("stripped source did not preserve the exact commented metadata:\n%s", stripped)
	}
}

func TestPinnedSourceValidationBindsOperatorImageVersion(t *testing.T) {
	t.Parallel()

	input := cdiOperatorBundleFixture()
	if err := validateOperatorImageVersion([]byte(input), bundleTargets["cdi"], "v1.65.0"); err != nil {
		t.Fatalf("matching release version was rejected: %v", err)
	}
	if err := validateOperatorImageVersion([]byte(input), bundleTargets["cdi"], "v1.66.0"); err == nil {
		t.Fatal("operator-image validator accepted a mismatched release version")
	} else if !strings.Contains(err.Error(), "operator image") {
		t.Fatalf("error did not name the mismatched operator image: %v", err)
	}
}

func TestPinnedSourceValidationRejectsNonAnnotationChange(t *testing.T) {
	t.Parallel()

	input := cdiOperatorBundleFixture()
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	expectedSHA256 := fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
	annotated = strings.Replace(
		annotated,
		"quay.io/kubevirt/cdi-operator:v1.65.0",
		"quay.io/kubevirt/cdi-operator:v9.99.0",
		1,
	)
	if err := validatePinnedSource(
		[]byte(annotated),
		bundleTargets["cdi"],
		expectedSHA256,
		"v1.65.0",
	); err == nil {
		t.Fatal("pinned-source validator accepted a non-annotation bundle change")
	} else if !strings.Contains(err.Error(), "source SHA-256") {
		t.Fatalf("error did not name the pinned source digest: %v", err)
	}
}

func TestAnnotatedValidationRejectsSuppressionOnUnrelatedResource(t *testing.T) {
	t.Parallel()

	input := bundleFixture("cdi-operator-cluster", "cdi-operator")
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	annotated = strings.Replace(
		annotated,
		"    owner: platform",
		"    owner: platform\n    checkov.io/skip1: \"CKV_K8S_99=unreviewed\"",
		1,
	)
	if err := validateAnnotatedBundle([]byte(annotated), bundleTargets["cdi"]); err == nil {
		t.Fatal("committed-bundle validator accepted a suppression on an unrelated resource")
	} else if !strings.Contains(err.Error(), "unexpected Checkov disposition") {
		t.Fatalf("error did not name the unexpected disposition: %v", err)
	}
}

func TestAnnotatedValidationRejectsChangedDisposition(t *testing.T) {
	t.Parallel()

	input := bundleFixture("cdi-operator-cluster", "cdi-operator")
	annotated, err := annotateBundle(input, bundleTargets["cdi"])
	if err != nil {
		t.Fatalf("annotate fixture: %v", err)
	}
	annotated = strings.Replace(annotated, clusterRoleReason, "different reason", 1)
	if err := validateAnnotatedBundle([]byte(annotated), bundleTargets["cdi"]); err == nil {
		t.Fatal("committed-bundle validator accepted a changed disposition")
	} else if !strings.Contains(err.Error(), "does not match the configured disposition") {
		t.Fatalf("error did not name the changed disposition: %v", err)
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
