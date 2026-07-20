package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	roleManifestPath     = "k8s/providers/hetzner/apps/aws/role-eks-ci.yaml"
	boundaryManifestPath = "k8s/providers/hetzner/apps/aws/policy-eks-ci-smoke-boundary.yaml"
	appsOverlayPath      = "k8s/providers/hetzner/apps"

	expectedKubectlVersion   = "v1.36.2"
	expectedKustomizeVersion = "v5.8.1"
	expectedRoleManifestSHA  = "96a77d18160c450340e65b0953f44016a01a08429416f7a82142c3f90a61ca07"
	expectedBoundarySHA      = "b96bfd8c96baa2e09f32a1cc05f76473ecc021fed554a2880ce8e3dd399902c7"
	expectedTrustPolicySHA   = "85d5d45343f9eac5fdc35717c85c88c5b0f8fde9eddffb169c3a223617fd0a5e"
	expectedInlinePolicySHA  = "60e3086a6d3dac0092ffe8264c04ebae783c0d38f19a3cf073ed8991085a4df8"
	expectedBoundaryJSONSHA  = "e617004bce71a65f92934c4f7575d7559a290afe7a17363ce12db8ad7b519610"
)

type resourceIdentity struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
}

var expectedRenderedHashes = map[resourceIdentity]string{
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Role", namespace: "aws", name: "eks-ci"}:                       "0967890d16316a8cfcb1cca8a52085c6989c42000fafbbd0ada6323d4e15c97c",
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Policy", namespace: "aws", name: "eks-ci-smoke-boundary"}:      "66f79a06cd8f789f6a2dd66b263c3f4459447f96227f57996591d75b441b0104",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "aws", name: "aws-managed-resources"}:        "ff4c3264c519b1b4a7ec9b5145412f39ea2ba7b6163d8dc50fb029b1460edcda",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "aws", name: "aws-managed-resources"}: "d846c8d9810dd7c0cba33612d2de63183403ccb07c4d5a5c90d0563a444cd714",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "aws", name: "aws"}:               "7bde9c682a81b752bdf9d2b14ce69ca1690008a39f2562d4887f8200447dea71",
}

func fingerprint(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func canonicalFingerprint(value any) (string, error) {
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal canonical JSON: %w", err)
	}
	return fingerprint(canonical), nil
}

func decodeDocuments(contents []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	documents := make([]map[string]any, 0)
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		if len(document) != 0 {
			documents = append(documents, document)
		}
	}
	return documents, nil
}

func nestedMap(document map[string]any, keys ...string) (map[string]any, error) {
	current := document
	for _, key := range keys {
		value, ok := current[key]
		if !ok {
			return nil, fmt.Errorf("missing %s", strings.Join(keys, "."))
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s is not an object", strings.Join(keys, "."))
		}
		current = next
	}
	return current, nil
}

func requireExactKeys(object map[string]any, expected ...string) error {
	actual := make([]string, 0, len(object))
	for key := range object {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(expected)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf("unexpected keys: got %v, want %v", actual, expected)
	}
	return nil
}

func parseJSONPolicy(value any, description string) (map[string]any, error) {
	policyText, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON string", description)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(policyText), &policy); err != nil {
		return nil, fmt.Errorf("parse %s: %w", description, err)
	}
	return policy, nil
}

func requireCanonicalFingerprint(value any, expected string, description string) error {
	actual, err := canonicalFingerprint(value)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("unapproved %s fingerprint: %s", description, actual)
	}
	return nil
}

func validateRole(role []byte) error {
	if actual := fingerprint(role); actual != expectedRoleManifestSHA {
		return fmt.Errorf("unapproved role manifest fingerprint: %s", actual)
	}
	documents, err := decodeDocuments(role)
	if err != nil {
		return fmt.Errorf("decode role manifest: %w", err)
	}
	if len(documents) != 1 {
		return fmt.Errorf("role manifest must contain exactly one document, got %d", len(documents))
	}
	forProvider, err := nestedMap(documents[0], "spec", "forProvider")
	if err != nil {
		return err
	}
	if err := requireExactKeys(forProvider, "description", "maxSessionDuration", "assumeRolePolicy", "inlinePolicy"); err != nil {
		return fmt.Errorf("role forProvider: %w", err)
	}
	if forProvider["maxSessionDuration"] != 7200 {
		return fmt.Errorf("unapproved maxSessionDuration: %v", forProvider["maxSessionDuration"])
	}
	trust, err := parseJSONPolicy(forProvider["assumeRolePolicy"], "trust policy")
	if err != nil {
		return err
	}
	if err := requireCanonicalFingerprint(trust, expectedTrustPolicySHA, "trust policy"); err != nil {
		return err
	}
	inlinePolicies, ok := forProvider["inlinePolicy"].([]any)
	if !ok || len(inlinePolicies) != 1 {
		return errors.New("role must contain exactly one inline policy")
	}
	inlinePolicy, ok := inlinePolicies[0].(map[string]any)
	if !ok || inlinePolicy["name"] != "eks-ci-smoke" {
		return errors.New("role inline policy must be named eks-ci-smoke")
	}
	policy, err := parseJSONPolicy(inlinePolicy["policy"], "inline policy")
	if err != nil {
		return err
	}
	return requireCanonicalFingerprint(policy, expectedInlinePolicySHA, "inline policy")
}

func validateBoundary(boundary []byte) error {
	if actual := fingerprint(boundary); actual != expectedBoundarySHA {
		return fmt.Errorf("unapproved boundary manifest fingerprint: %s", actual)
	}
	documents, err := decodeDocuments(boundary)
	if err != nil {
		return fmt.Errorf("decode boundary manifest: %w", err)
	}
	if len(documents) != 1 {
		return fmt.Errorf("boundary manifest must contain exactly one document, got %d", len(documents))
	}
	forProvider, err := nestedMap(documents[0], "spec", "forProvider")
	if err != nil {
		return err
	}
	if err := requireExactKeys(forProvider, "description", "policy"); err != nil {
		return fmt.Errorf("boundary forProvider: %w", err)
	}
	policy, err := parseJSONPolicy(forProvider["policy"], "permissions boundary")
	if err != nil {
		return err
	}
	return requireCanonicalFingerprint(policy, expectedBoundaryJSONSHA, "permissions boundary")
}

func identityOf(document map[string]any) resourceIdentity {
	metadata, _ := document["metadata"].(map[string]any)
	return resourceIdentity{
		apiVersion: fmt.Sprint(document["apiVersion"]),
		kind:       fmt.Sprint(document["kind"]),
		namespace:  fmt.Sprint(metadata["namespace"]),
		name:       fmt.Sprint(metadata["name"]),
	}
}

func targetsAWSServiceAccount(document map[string]any) bool {
	subjects, ok := document["subjects"].([]any)
	if !ok {
		return false
	}
	for _, rawSubject := range subjects {
		subject, ok := rawSubject.(map[string]any)
		if !ok {
			continue
		}
		if subject["kind"] == "ServiceAccount" &&
			subject["name"] == "aws" && subject["namespace"] == "aws" {
			return true
		}
	}
	return false
}

func isAuthorizationResource(document map[string]any, identity resourceIdentity) bool {
	if strings.HasPrefix(identity.apiVersion, "iam.aws.") {
		return true
	}
	if identity.apiVersion == "rbac.authorization.k8s.io/v1" {
		if identity.namespace == "aws" &&
			(identity.kind == "Role" || identity.kind == "RoleBinding") {
			return true
		}
		// Follow the privileged subject across namespaces and cluster scope. The
		// rendered allowlist then pins the one approved binding, including roleRef.
		if (identity.kind == "RoleBinding" || identity.kind == "ClusterRoleBinding") &&
			targetsAWSServiceAccount(document) {
			return true
		}
	}
	return identity.namespace == "aws" &&
		identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" &&
		identity.kind == "Kustomization"
}

func validateRendered(rendered []byte) error {
	documents, err := decodeDocuments(rendered)
	if err != nil {
		return err
	}
	seen := make(map[resourceIdentity]bool, len(expectedRenderedHashes))
	problems := make([]error, 0)
	for _, document := range documents {
		identity := identityOf(document)
		if !isAuthorizationResource(document, identity) {
			continue
		}
		expected, ok := expectedRenderedHashes[identity]
		if !ok {
			return fmt.Errorf("unexpected rendered authorization resource: %+v", identity)
		}
		if seen[identity] {
			return fmt.Errorf("duplicate rendered authorization resource: %+v", identity)
		}
		seen[identity] = true
		actual, hashErr := canonicalFingerprint(document)
		if hashErr != nil {
			return hashErr
		}
		if actual != expected {
			problems = append(problems, fmt.Errorf("unapproved rendered %+v fingerprint: %s", identity, actual))
		}
	}
	for identity := range expectedRenderedHashes {
		if !seen[identity] {
			problems = append(problems, fmt.Errorf("missing rendered authorization resource: %+v", identity))
		}
	}
	return errors.Join(problems...)
}

func validateAuthorization(role []byte, boundary []byte, rendered []byte) error {
	if err := validateRole(role); err != nil {
		return err
	}
	if err := validateBoundary(boundary); err != nil {
		return err
	}
	return validateRendered(rendered)
}

func validateRendererVersion(versionJSON []byte) error {
	var version struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
		KustomizeVersion string `json:"kustomizeVersion"`
	}
	if err := json.Unmarshal(versionJSON, &version); err != nil {
		return fmt.Errorf("parse kubectl version: %w", err)
	}
	if version.ClientVersion.GitVersion != expectedKubectlVersion ||
		version.KustomizeVersion != expectedKustomizeVersion {
		return fmt.Errorf(
			"unapproved renderer: kubectl=%s kustomize=%s",
			version.ClientVersion.GitVersion,
			version.KustomizeVersion,
		)
	}
	return nil
}

func commandOutput(name string, args ...string) ([]byte, error) {
	command := exec.Command(name, args...) //nolint:gosec // Fixed binary and repository-controlled arguments.
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return output, nil
}

func run(repoRoot string, stdout io.Writer, stderr io.Writer) int {
	version, err := commandOutput("kubectl", "version", "--client", "-o", "json")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	if err := validateRendererVersion(version); err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	rendered, err := commandOutput("kubectl", "kustomize", filepath.Join(repoRoot, appsOverlayPath))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	role, err := os.ReadFile(filepath.Join(repoRoot, roleManifestPath)) //nolint:gosec // Explicit repository path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: read role: %v\n", err)
		return 1
	}
	boundary, err := os.ReadFile(filepath.Join(repoRoot, boundaryManifestPath)) //nolint:gosec // Explicit repository path.
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: read boundary: %v\n", err)
		return 1
	}
	if err := validateAuthorization(role, boundary, rendered); err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "EKS CI role authorization contract passed.")
	return 0
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-eks-ci-role-policy <repository-root>")
		return 2
	}
	return run(args[0], stdout, stderr)
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
