package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repositoryInputs(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()

	repoRoot := filepath.Join("..", "..")
	read := func(path string) []byte {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(repoRoot, path)) //nolint:gosec // Explicit repository path.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return contents
	}

	ctx, cancel := context.WithTimeout(context.Background(), rendererCommandTimeout)
	defer cancel()
	rendered, err := renderAuthorizationLayers(ctx, repoRoot, commandOutput)
	if err != nil {
		t.Fatalf("render authorization layers: %v", err)
	}

	return read(roleManifestPath), read(boundaryManifestPath), rendered
}

func TestRenderAuthorizationLayersIncludesEveryProductionLayer(t *testing.T) {
	repoRoot := filepath.Join("test", "repo")
	wantOverlayPaths := []string{
		"k8s/providers/hetzner/apps",
		"k8s/providers/hetzner/infrastructure",
		"k8s/providers/hetzner/infrastructure/controllers",
		"k8s/clusters/prod/bootstrap",
		"k8s/clusters/prod",
	}
	if got, want := strings.Join(authorizationOverlayPaths, "\n"), strings.Join(wantOverlayPaths, "\n"); got != want {
		t.Fatalf("authorization overlay paths = %q, want %q", got, want)
	}

	renderedPaths := make([]string, 0, len(wantOverlayPaths))
	execute := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name != "kubectl" || len(args) != 2 || args[0] != "kustomize" {
			t.Fatalf("command = %q %v, want kubectl kustomize <path>", name, args)
		}
		renderedPaths = append(renderedPaths, args[1])
		return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: authorization-layer\n"), nil
	}

	rendered, err := renderAuthorizationLayers(context.Background(), repoRoot, execute)
	if err != nil {
		t.Fatalf("renderAuthorizationLayers() error = %v", err)
	}
	documents, err := decodeDocuments(rendered)
	if err != nil {
		t.Fatalf("decode rendered authorization layers: %v", err)
	}
	if got, want := len(documents), len(wantOverlayPaths); got != want {
		t.Fatalf("rendered documents = %d, want %d", got, want)
	}

	wantPaths := make([]string, 0, len(wantOverlayPaths))
	for _, overlayPath := range wantOverlayPaths {
		wantPaths = append(wantPaths, filepath.Join(repoRoot, overlayPath))
	}
	if got, want := strings.Join(renderedPaths, "\n"), strings.Join(wantPaths, "\n"); got != want {
		t.Fatalf("rendered paths = %q, want %q", got, want)
	}
}

func TestCommandOutputHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := commandOutput(ctx, "go", "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commandOutput() error = %v, want context cancellation", err)
	}
}

func TestParseJSONPolicyRejectsNull(t *testing.T) {
	if _, err := parseJSONPolicy("null", "test policy"); err == nil {
		t.Fatal("parseJSONPolicy() error = nil, want JSON object rejection")
	}
}

func TestValidateRenderedRejectsAuthorizationSubstitutions(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name: "binding subject namespace",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: substituted-subject
  namespace: tenant
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: view
subjects:
  - kind: ServiceAccount
    name: aws
    namespace: ${target_namespace:=aws}
`,
		},
		{
			name: "resource kind",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ${binding_kind:=RoleBinding}
metadata:
  name: substituted-kind
  namespace: tenant
subjects: []
`,
		},
		{
			name: "generated binding kind",
			manifest: `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: substituted-generator
spec:
  rules:
    - name: generate-binding
      generate:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ${generated_kind:=RoleBinding}
        name: generated
        namespace: tenant
        data: {}
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRendered([]byte(tt.manifest))
			if err == nil || !strings.Contains(err.Error(), "unresolved Flux substitution in authorization resource") {
				t.Fatalf("validateRendered() error = %v, want unresolved authorization substitution", err)
			}
		})
	}
}

func TestValidateRenderedRejectsIndirectAuthorizationResources(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name: "Kyverno generator",
			manifest: `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: generate-aws-binding
spec:
  rules:
    - name: generate-binding
      generate:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: RoleBinding
        name: generated
        namespace: tenant
        data:
          roleRef:
            apiGroup: rbac.authorization.k8s.io
            kind: ClusterRole
            name: cluster-admin
          subjects:
            - kind: ServiceAccount
              name: aws
              namespace: aws
`,
		},
		{
			name: "RBAC writer",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: binding-writer
rules:
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [rolebindings, clusterrolebindings]
    verbs: [create, update, patch]
`,
		},
		{
			name: "Kyverno binding mutation",
			manifest: `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: mutate-aws-binding
spec:
  rules:
    - name: mutate-binding
      match:
        any:
          - resources:
              kinds: [RoleBinding]
      mutate:
        patchStrategicMerge:
          subjects:
            - kind: ServiceAccount
              name: aws
              namespace: aws
`,
		},
		{
			name: "Kyverno RBAC group wildcard mutation",
			manifest: `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: mutate-rbac-group
spec:
  rules:
    - name: mutate-rbac
      match:
        any:
          - resources:
              kinds: [rbac.authorization.k8s.io/*]
      mutate:
        patchStrategicMerge:
          metadata:
            labels:
              example: unsafe
`,
		},
		{
			name: "KRO binding template",
			manifest: `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: generate-binding
  annotations:
    kustomize.toolkit.fluxcd.io/substitute: disabled
spec:
  resources:
    - id: binding
      template:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: RoleBinding
        metadata:
          name: generated
          namespace: tenant
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: cluster-admin
        subjects:
          - kind: ServiceAccount
            name: ${schema.spec.name}
            namespace: aws
`,
		},
		{
			name: "role patch writer",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: role-writer
rules:
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [roles, clusterroles]
    verbs: [patch]
`,
		},
		{
			name: "role privilege escalation",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: role-binder
rules:
  - apiGroups: [rbac.authorization.k8s.io]
    resources: [clusterroles]
    verbs: [bind, escalate]
`,
		},
		{
			name: "binding to unavailable privileged role",
			manifest: `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: external-cluster-admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: controller
    namespace: controllers
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRendered([]byte(tt.manifest))
			if err == nil || !strings.Contains(err.Error(), "unexpected rendered authorization resource") {
				t.Fatalf("validateRendered() error = %v, want unexpected authorization resource", err)
			}
		})
	}
}

func TestWorkflowValidatesAuthorizationBeforeMergeGroupDeploy(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yaml")) //nolint:gosec // Explicit repository path.
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	documents, err := decodeDocuments(contents)
	if err != nil || len(documents) != 1 {
		t.Fatalf("decode CI workflow: documents=%d error=%v", len(documents), err)
	}
	jobs, err := nestedMap(documents[0], "jobs")
	if err != nil {
		t.Fatal(err)
	}
	authorizationJob, ok := jobs["validate-eks-authorization"].(map[string]any)
	if !ok {
		t.Fatal("CI workflow is missing validate-eks-authorization job")
	}
	condition := fmt.Sprint(authorizationJob["if"])
	if !strings.Contains(condition, "merge_group") || !strings.Contains(condition, "needs.changes.outputs.k8s") {
		t.Fatalf("authorization job condition = %q, want merge-group k8s gate", condition)
	}

	for _, jobName := range []string{"deploy-prod", "ci-required-checks"} {
		job, ok := jobs[jobName].(map[string]any)
		if !ok || !stringListIncludes(job["needs"], "validate-eks-authorization") {
			t.Fatalf("%s must need validate-eks-authorization", jobName)
		}
	}
	requiredChecks, _ := jobs["ci-required-checks"].(map[string]any)
	steps, _ := requiredChecks["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("ci-required-checks steps = %d, want 1", len(steps))
	}
	step, _ := steps[0].(map[string]any)
	with, _ := step["with"].(map[string]any)
	if !strings.Contains(fmt.Sprint(with["job-results"]), "needs.validate-eks-authorization.result") {
		t.Fatal("required-check aggregation omits validate-eks-authorization result")
	}
}

func TestIndirectAuthorizationPolicyIgnoresNonRBACWildcardSelectors(t *testing.T) {
	documents, err := decodeDocuments([]byte(`apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: mutate-pods
spec:
  rules:
    - name: mutate-pod
      match:
        any:
          - resources:
              kinds: [Pod]
              namespaces: ["*"]
      mutate:
        patchStrategicMerge:
          metadata:
            labels:
              example: safe
`))
	if err != nil || len(documents) != 1 {
		t.Fatalf("decode policy: documents=%d error=%v", len(documents), err)
	}
	if isIndirectAuthorizationPolicy(documents[0], identityOf(documents[0])) {
		t.Fatal("isIndirectAuthorizationPolicy() = true for non-RBAC wildcard selector")
	}
}

func TestBindingReferencesAuthorizationWriter(t *testing.T) {
	documents, err := decodeDocuments([]byte(`apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: controller-grant
  namespace: controllers
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: binding-writer
subjects: []
`))
	if err != nil || len(documents) != 1 {
		t.Fatalf("decode binding: documents=%d error=%v", len(documents), err)
	}
	writers := map[resourceIdentity]struct{}{
		{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "controllers", name: "binding-writer"}: {},
	}
	if !bindingReferencesAuthorizationWriter(documents[0], identityOf(documents[0]), writers) {
		t.Fatal("bindingReferencesAuthorizationWriter() = false, want true")
	}
}

func TestValidateAuthorizationAcceptsCommittedPolicy(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)

	if err := validateAuthorization(role, boundary, rendered); err != nil {
		t.Fatalf("validateAuthorization() error = %v", err)
	}
}

func TestValidateAuthorizationRejectsSourceAndRenderedMutations(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	originalDocuments, err := decodeDocuments(rendered)
	if err != nil {
		t.Fatalf("decode committed render: %v", err)
	}
	duplicateRendered := append(append([]byte{}, rendered...), role...)
	duplicateDocuments, err := decodeDocuments(duplicateRendered)
	if err != nil {
		t.Fatalf("duplicate rendered role fixture must contain valid YAML documents: %v", err)
	}
	if got, want := len(duplicateDocuments), len(originalDocuments)+1; got != want {
		t.Fatalf("duplicate rendered role fixture documents = %d, want %d", got, want)
	}
	missingBoundary := new(bytes.Buffer)
	encoder := yaml.NewEncoder(missingBoundary)
	documents, err := decodeDocuments(rendered)
	if err != nil {
		t.Fatalf("decode rendered documents: %v", err)
	}
	for _, document := range documents {
		if identityOf(document) == (resourceIdentity{
			apiVersion: "iam.aws.m.upbound.io/v1beta1",
			kind:       "Policy",
			namespace:  "aws",
			name:       "eks-ci-smoke-boundary",
		}) {
			continue
		}
		if err := encoder.Encode(document); err != nil {
			t.Fatalf("encode rendered document: %v", err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("close rendered document encoder: %v", err)
	}

	tests := []struct {
		name      string
		role      []byte
		boundary  []byte
		rendered  []byte
		wantError string
	}{
		{
			name:      "missing rendered boundary",
			role:      role,
			boundary:  boundary,
			rendered:  missingBoundary.Bytes(),
			wantError: "missing rendered authorization resource",
		},
		{
			name: "expanded EKS grant",
			role: bytes.Replace(
				role,
				[]byte(`"eks:DescribeClusterVersions"`),
				[]byte(`"eks:DeleteCluster"`),
				1,
			),
			boundary:  boundary,
			rendered:  rendered,
			wantError: "role manifest fingerprint",
		},
		{
			name: "unreviewed role management surface",
			role: bytes.Replace(
				role,
				[]byte("\n  providerConfigRef:"),
				[]byte("\n  initProvider:\n    managedPolicyArns:\n      - arn:aws:iam::aws:policy/AdministratorAccess\n  providerConfigRef:"),
				1,
			),
			boundary:  boundary,
			rendered:  rendered,
			wantError: "role manifest fingerprint",
		},
		{
			name: "expanded permissions boundary",
			role: role,
			boundary: bytes.Replace(
				boundary,
				[]byte(`"sts:GetCallerIdentity"`),
				[]byte(`"sts:*"`),
				1,
			),
			rendered:  rendered,
			wantError: "boundary manifest fingerprint",
		},
		{
			name:      "duplicate rendered role",
			role:      role,
			boundary:  boundary,
			rendered:  duplicateRendered,
			wantError: "duplicate rendered authorization resource",
		},
		{
			name:     "additional rendered IAM resource",
			role:     role,
			boundary: boundary,
			rendered: append(append([]byte{}, rendered...), []byte(`---
apiVersion: iam.aws.m.upbound.io/v1beta1
kind: Role
metadata:
  name: unexpected
  namespace: aws
`)...),
			wantError: "unexpected rendered authorization resource",
		},
		{
			name:     "additional tenant Flux handoff",
			role:     role,
			boundary: boundary,
			rendered: append(append([]byte{}, rendered...), []byte(`---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: aws-shadow
  namespace: aws
`)...),
			wantError: "unexpected rendered authorization resource",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuthorization(tt.role, tt.boundary, tt.rendered)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("validateAuthorization() error = %v, want containing %q", err, tt.wantError)
			}
		})
	}
}

func TestValidateAuthorizationRejectsBindingsThatIncludeAWSServiceAccountIdentity(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)

	tests := []struct {
		name    string
		binding string
	}{
		{
			name: "RoleBinding outside AWS namespace",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: aws-shadow
  namespace: tenant-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aws-managed-resources
subjects:
  - kind: ServiceAccount
    name: aws
    namespace: aws
`,
		},
		{
			name: "cluster-wide binding",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: aws
    namespace: aws
`,
		},
		{
			name: "service account user identity",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: aws-shadow
  namespace: tenant-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aws-managed-resources
subjects:
  - kind: User
    name: system:serviceaccount:aws:aws
`,
		},
		{
			name: "namespace service account group",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: Group
    name: system:serviceaccounts:aws
`,
		},
		{
			name: "all service accounts group",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: Group
    name: system:serviceaccounts
`,
		},
		{
			name: "all authenticated identities group",
			binding: `---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-shadow
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: Group
    name: system:authenticated
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := append(append([]byte{}, rendered...), []byte(tt.binding)...)
			err := validateAuthorization(role, boundary, mutated)
			if err == nil || !strings.Contains(err.Error(), "unexpected rendered authorization resource") {
				t.Fatalf("validateAuthorization() error = %v, want unexpected rendered authorization resource", err)
			}
		})
	}
}

func TestValidateRendererVersionPinsKubectlAndKustomize(t *testing.T) {
	valid := []byte(`{
  "clientVersion": {"gitVersion": "v1.36.2"},
  "kustomizeVersion": "v5.8.1"
}`)
	if err := validateRendererVersion(valid); err != nil {
		t.Fatalf("validateRendererVersion() error = %v", err)
	}

	invalid := bytes.Replace(valid, []byte("v5.8.1"), []byte("v5.9.0"), 1)
	if err := validateRendererVersion(invalid); err == nil {
		t.Fatal("validateRendererVersion() error = nil, want unapproved renderer")
	}
}

func TestWorkflowRunsValidatorForAuthorizationChanges(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	contract := string(workflow)
	for _, required := range []string{
		"- 'scripts/validate-eks-ci-role-policy/**'",
		"KUBECTL_VERSION: \"v1.36.2\"",
		"go test ./scripts/validate-eks-ci-role-policy",
		"go run ./scripts/validate-eks-ci-role-policy .",
	} {
		if !strings.Contains(contract, required) {
			t.Errorf("CI workflow is missing %q", required)
		}
	}
}
