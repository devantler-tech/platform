package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryInputs(t *testing.T) ([]byte, []byte, []byte) {
	t.Helper()

	repoRoot := filepath.Join("..", "..")
	read := func(path string) []byte {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(repoRoot, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return contents
	}

	command := exec.Command("kubectl", "kustomize", filepath.Join(repoRoot, "k8s/providers/hetzner/apps"))
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("render apps overlay: %v", err)
	}

	return read(roleManifestPath), read(boundaryManifestPath), rendered
}

func TestValidateAuthorizationAcceptsCommittedPolicy(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)

	if err := validateAuthorization(role, boundary, rendered); err != nil {
		t.Fatalf("validateAuthorization() error = %v", err)
	}
}

func TestValidateAuthorizationRejectsSourceAndRenderedMutations(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)

	tests := []struct {
		name      string
		role      []byte
		boundary  []byte
		rendered  []byte
		wantError string
	}{
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
			rendered:  append(append([]byte{}, rendered...), role...),
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
