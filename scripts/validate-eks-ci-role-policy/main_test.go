package main

import (
	"bytes"
	"context"
	"errors"
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
	renderedPaths := make([]string, 0, len(authorizationOverlayPaths))
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
	if got, want := len(documents), len(authorizationOverlayPaths); got != want {
		t.Fatalf("rendered documents = %d, want %d", got, want)
	}

	wantPaths := make([]string, 0, len(authorizationOverlayPaths))
	for _, overlayPath := range authorizationOverlayPaths {
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
