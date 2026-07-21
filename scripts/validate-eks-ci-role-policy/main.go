package main

import (
	"bytes"
	"context"
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
	"time"

	"gopkg.in/yaml.v3"
)

const (
	roleManifestPath          = "k8s/providers/hetzner/apps/aws/role-eks-ci.yaml"
	boundaryManifestPath      = "k8s/providers/hetzner/apps/aws/policy-eks-ci-smoke-boundary.yaml"
	appsOverlayPath           = "k8s/providers/hetzner/apps"
	infrastructureOverlayPath = "k8s/providers/hetzner/infrastructure"
	controllerOverlayPath     = "k8s/providers/hetzner/infrastructure/controllers"
	bootstrapOverlayPath      = "k8s/clusters/prod/bootstrap"
	rootProductionOverlayPath = "k8s/clusters/prod"
	rendererCommandTimeout    = 2 * time.Minute

	expectedKubectlVersion     = "v1.36.2"
	expectedKustomizeVersion   = "v5.8.1"
	expectedRoleManifestSHA    = "96a77d18160c450340e65b0953f44016a01a08429416f7a82142c3f90a61ca07"
	expectedBoundarySHA        = "b96bfd8c96baa2e09f32a1cc05f76473ecc021fed554a2880ce8e3dd399902c7"
	expectedTrustPolicySHA     = "85d5d45343f9eac5fdc35717c85c88c5b0f8fde9eddffb169c3a223617fd0a5e"
	expectedInlinePolicySHA    = "60e3086a6d3dac0092ffe8264c04ebae783c0d38f19a3cf073ed8991085a4df8"
	expectedBoundaryJSONSHA    = "e617004bce71a65f92934c4f7575d7559a290afe7a17363ce12db8ad7b519610"
	expectedRenderedSurfaceSHA = "a7fc2f116afd6cc1e9595f6b61de85215c751d2cac033660befabc2c7e3fda61"
)

// authorizationOverlayPaths lists every independently reconciled production
// layer where an object can grant privileges to the aws/aws service account.
var authorizationOverlayPaths = []string{
	appsOverlayPath,
	infrastructureOverlayPath,
	controllerOverlayPath,
	bootstrapOverlayPath,
	rootProductionOverlayPath,
}

// commandExecutor makes the renderer orchestration independently testable
// without weakening the production command and deadline contract.
type commandExecutor func(context.Context, string, ...string) ([]byte, error)

// resourceIdentity is the complete Kubernetes identity used to distinguish
// approved authorization objects from aliases and same-named resources.
type resourceIdentity struct {
	apiVersion string
	kind       string
	namespace  string
	name       string
}

// resourceType identifies every instance of a controller-defined API kind.
type resourceType struct {
	apiVersion string
	kind       string
}

// expectedRenderedHashes preserves object-specific diagnostics for the core
// EKS CI identities while the aggregate surface hash pins every selected
// source, controller, binding, and indirect authorization object.
var expectedRenderedHashes = map[resourceIdentity]string{
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Role", namespace: "aws", name: "eks-ci"}:                                        "0967890d16316a8cfcb1cca8a52085c6989c42000fafbbd0ada6323d4e15c97c",
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Policy", namespace: "aws", name: "eks-ci-smoke-boundary"}:                       "66f79a06cd8f789f6a2dd66b263c3f4459447f96227f57996591d75b441b0104",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "aws", name: "aws-managed-resources"}:                         "ff4c3264c519b1b4a7ec9b5145412f39ea2ba7b6163d8dc50fb029b1460edcda",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "aws", name: "aws-managed-resources"}:                  "d846c8d9810dd7c0cba33612d2de63183403ccb07c4d5a5c90d0563a444cd714",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "crossview", name: "crossview-portforward"}:            "78992d9727763fdcf1bda05969fdc881e6d0e54cc72efc07555304b47d25bc3a",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: "kro-tenant-rgd"}:                                           "4447f41c03e8297fafdabcadf4fdd8ca3260f2c84264c531b2179cb7df2c1556",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-cluster-reader"}:                               "7d896404f02d6418c289065d73f9ad79345217d76c8d89eadca2c06e6066b487",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-view"}:                                         "4d07ba3a995cfc139351b4227739efeba9348777f7fe47ac69b87d08e70bd45f",
	{apiVersion: "kro.run/v1alpha1", kind: "ResourceGraphDefinition", name: "tenant.kro.run"}:                                           "404de3502423d08af04eaa5d1ca6a6b76634ae09c270e7718994cfd346c8a07f",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "ascoachingogvaner", name: "ascoachingogvaner"}:    "89ea0484e37b691594b7a72be2ca2de285697818bf88a5b37b4fa8a9161c54fa",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "aws", name: "aws"}:                                "7bde9c682a81b752bdf9d2b14ce69ca1690008a39f2562d4887f8200447dea71",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "apps"}:                       "1a2ecb3104630c44466d846159ee68ff6a98888887c02ecd0278782793dead4a",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "bootstrap"}:                  "7f674a1762f298330c7c9e4d9d4e8bf46108b10727e02a25ca5096d7913cc0a7",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure"}:             "312d84288f510b4a38d985385487a52cb2dc1c634bbcbab8dc5e438689891189",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure-controllers"}: "9d9b62d3221442d6355d16a34d31c198619fb3b3728df960fd67222a531ece7b",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "github-config", name: "github-config"}:            "8e9f72b0f4f982d050aff0b97d246c68b538cbc397cdd45d031c95cfae981e7c",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "unifi", name: "unifi"}:                            "47c63f6a762caeacf257ddd32cbbeb3f3568eeea0e258ec006621579114731ff",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "wedding-app", name: "wedding-app"}:                "6cca0d2d0e7874bf3f0c82f4e04f151d6c172eeae7929a8dbacbf37ed9793a6c",
}

// fingerprint returns the SHA-256 identity used for byte-exact source checks.
func fingerprint(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

// canonicalFingerprint hashes a parsed value after canonical JSON encoding so
// semantically identical YAML formatting cannot bypass structural checks.
func canonicalFingerprint(value any) (string, error) {
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal canonical JSON: %w", err)
	}
	return fingerprint(canonical), nil
}

// decodeDocuments parses every non-empty YAML document and rejects malformed
// input instead of silently validating a partial stream.
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

// nestedMap resolves a required object path and fails when any segment is
// missing or has the wrong shape.
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

// requireExactKeys prevents approved objects from hiding extra policy-bearing
// siblings that a selected-leaf assertion would miss.
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

// parseJSONPolicy requires Crossplane's embedded IAM policy to remain a valid
// JSON object before its canonical shape is compared.
func parseJSONPolicy(value any, description string) (map[string]any, error) {
	policyText, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON string", description)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(policyText), &policy); err != nil {
		return nil, fmt.Errorf("parse %s: %w", description, err)
	}
	if policy == nil {
		return nil, fmt.Errorf("%s is not a JSON object", description)
	}
	return policy, nil
}

// requireCanonicalFingerprint rejects any structural policy drift with a
// diagnostic hash that can be reviewed and deliberately approved.
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

// validateRole pins the complete EKS CI role source, trust relationship,
// session limit, and sole inline policy rather than a subset of actions.
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

// validateBoundary pins both the permissions-boundary manifest and its embedded
// policy so role grants cannot escape the intended ceiling.
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

// identityOf derives the canonical identity used by the rendered authorization
// allowlist; cluster-scoped resources use an empty namespace.
func identityOf(document map[string]any) resourceIdentity {
	metadata, _ := document["metadata"].(map[string]any)
	stringValue := func(object map[string]any, key string) string {
		value, ok := object[key]
		if !ok || value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
	return resourceIdentity{
		apiVersion: stringValue(document, "apiVersion"),
		kind:       stringValue(document, "kind"),
		namespace:  stringValue(metadata, "namespace"),
		name:       stringValue(metadata, "name"),
	}
}

// stringListIncludes reports whether a decoded YAML string list contains any
// requested value, including the Kubernetes RBAC wildcard when requested.
func stringListIncludes(value any, expected ...string) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawValue := range values {
		for _, candidate := range expected {
			if fmt.Sprint(rawValue) == candidate {
				return true
			}
		}
	}
	return false
}

// grantsAuthorizationControl identifies Roles and ClusterRoles that can mutate
// RBAC privileges, aggregate them, or assume service-account identities.
func grantsAuthorizationControl(document map[string]any) bool {
	identity := identityOf(document)
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "Role" && identity.kind != "ClusterRole") {
		return false
	}
	if identity.kind == "ClusterRole" {
		if _, aggregates := document["aggregationRule"]; aggregates {
			return true
		}
	}
	rules, ok := document["rules"].([]any)
	if !ok {
		return false
	}
	for _, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			continue
		}
		if stringListIncludes(
			rule["verbs"],
			"create",
			"update",
			"patch",
			"delete",
			"deletecollection",
			"*",
		) {
			protectedResources := []struct {
				apiGroup  string
				resources []string
			}{
				{apiGroup: "iam.aws.m.upbound.io", resources: []string{"roles", "policies", "*"}},
				{apiGroup: "iam.aws.upbound.io", resources: []string{"roles", "policies", "*"}},
				{apiGroup: "kustomize.toolkit.fluxcd.io", resources: []string{"kustomizations", "*"}},
				{apiGroup: "source.toolkit.fluxcd.io", resources: []string{"*"}},
				{apiGroup: "helm.toolkit.fluxcd.io", resources: []string{"helmreleases", "*"}},
				{apiGroup: "pkg.crossplane.io", resources: []string{"providers", "functions", "configurations", "deploymentruntimeconfigs", "*"}},
				{apiGroup: "kyverno.io", resources: []string{"policies", "clusterpolicies", "*"}},
				{apiGroup: "policies.kyverno.io", resources: []string{"mutatingpolicies", "generatingpolicies", "*"}},
			}
			for _, protected := range protectedResources {
				if stringListIncludes(rule["apiGroups"], protected.apiGroup, "*") &&
					stringListIncludes(rule["resources"], protected.resources...) {
					return true
				}
			}
		}
		if stringListIncludes(rule["apiGroups"], "rbac.authorization.k8s.io", "*") &&
			stringListIncludes(
				rule["resources"],
				"roles",
				"clusterroles",
				"rolebindings",
				"clusterrolebindings",
				"*",
			) &&
			stringListIncludes(
				rule["verbs"],
				"create",
				"update",
				"patch",
				"delete",
				"deletecollection",
				"bind",
				"escalate",
				"*",
			) {
			return true
		}
		if stringListIncludes(rule["apiGroups"], "", "*") &&
			stringListIncludes(rule["resources"], "serviceaccounts/token", "*") &&
			stringListIncludes(rule["verbs"], "create", "*") {
			return true
		}
		if stringListIncludes(rule["apiGroups"], "", "*") &&
			stringListIncludes(rule["resources"], "serviceaccounts", "*") &&
			stringListIncludes(rule["verbs"], "impersonate", "*") {
			return true
		}
	}
	return false
}

// isRBACAuthorizationKind recognizes Kyverno's short and group-qualified role
// and binding kinds, all of which can change effective privileges.
func isRBACAuthorizationKind(kind string) bool {
	return strings.Contains(kind, "${") || kind == "*" || kind == "Role" || kind == "ClusterRole" ||
		kind == "RoleBinding" || kind == "ClusterRoleBinding" ||
		(strings.HasPrefix(kind, "rbac.authorization.k8s.io/") && strings.HasSuffix(kind, "/*")) ||
		strings.HasSuffix(kind, "/Role") || strings.HasSuffix(kind, "/ClusterRole") ||
		strings.HasSuffix(kind, "/RoleBinding") || strings.HasSuffix(kind, "/ClusterRoleBinding")
}

// isAWSIAMAuthorizationKind recognizes the Crossplane IAM kinds that CARRY the
// protected permissions rather than merely pointing at them: the role itself,
// the boundary policy whose document is the permission set, and the attachments
// that decide which policies apply to it.
//
// These are the kinds actually under guard — the role and the boundary policy
// are both in the protected surface — so a Kyverno selector reaching them is an
// authorization selector by definition. Missing them let a legacy ClusterPolicy
// match iam.aws.m.upbound.io/v1beta1/Policy and mutate spec.forProvider.policy,
// widening the permissions boundary on the next admission without moving the
// validator hash.
//
// Bare kind names are accepted even though "Policy" and "Role" are ambiguous
// across API groups. The consequence of over-matching is that a policy joins the
// aggregate surface and the expected hash must be refreshed; the consequence of
// under-matching is a silent boundary widening. This fails closed on purpose.
func isAWSIAMAuthorizationKind(kind string) bool {
	if strings.HasPrefix(kind, "iam.aws.") {
		return true
	}
	targets := []string{
		"Policy",
		"RolePolicyAttachment",
		"UserPolicyAttachment",
		"GroupPolicyAttachment",
		"PolicyAttachment",
	}
	for _, target := range targets {
		if kind == target || strings.HasSuffix(kind, "/"+target) {
			return true
		}
	}
	return false
}

// isFluxSourceResource recognizes artifacts that a Flux Kustomization or
// HelmRelease can consume independently of the handoff object itself.
func isFluxSourceResource(identity resourceIdentity) bool {
	return strings.HasPrefix(identity.apiVersion, "source.toolkit.fluxcd.io/")
}

// isControllerRBACEmitter recognizes declarative packages whose controllers
// can materialize RBAC that does not exist in the Kustomize render.
func isControllerRBACEmitter(identity resourceIdentity) bool {
	if strings.HasPrefix(identity.apiVersion, "helm.toolkit.fluxcd.io/") && identity.kind == "HelmRelease" {
		return true
	}
	return strings.HasPrefix(identity.apiVersion, "pkg.crossplane.io/")
}

// isCurrentKyvernoMutationPolicy recognizes the non-legacy Kyverno resources
// that can generate or mutate objects using CEL-based policy APIs.
func isCurrentKyvernoMutationPolicy(identity resourceIdentity) bool {
	return strings.HasPrefix(identity.apiVersion, "policies.kyverno.io/") &&
		(identity.kind == "MutatingPolicy" || identity.kind == "GeneratingPolicy")
}

// isLegacyKyvernoPolicy recognizes rule-based mutation and generation APIs.
func isLegacyKyvernoPolicy(identity resourceIdentity) bool {
	return strings.HasPrefix(identity.apiVersion, "kyverno.io/") &&
		(identity.kind == "Policy" || identity.kind == "ClusterPolicy")
}

// isAuthorizationKind recognizes every kind whose contents or controller can
// redirect, emit, or grant the protected authorization surface.
func isAuthorizationKind(kind string) bool {
	if isRBACAuthorizationKind(kind) || isAWSIAMAuthorizationKind(kind) {
		return true
	}
	targets := []string{
		"Kustomization",
		"OCIRepository",
		"GitRepository",
		"Bucket",
		"HelmRepository",
		"ExternalArtifact",
		"HelmRelease",
		"Provider",
		"Function",
		"Configuration",
		"DeploymentRuntimeConfig",
	}
	for _, target := range targets {
		if kind == target || strings.HasSuffix(kind, "/"+target) {
			return true
		}
	}
	return strings.HasPrefix(kind, "source.toolkit.fluxcd.io/") && strings.HasSuffix(kind, "/*") ||
		strings.HasPrefix(kind, "helm.toolkit.fluxcd.io/") && strings.HasSuffix(kind, "/*") ||
		strings.HasPrefix(kind, "pkg.crossplane.io/") && strings.HasSuffix(kind, "/*") ||
		strings.HasPrefix(kind, "kustomize.toolkit.fluxcd.io/") && strings.HasSuffix(kind, "/*")
}

// kindSelectorIncludesAuthorization checks a Kyverno kind/kinds value.
func kindSelectorIncludesAuthorization(value any) bool {
	switch typedValue := value.(type) {
	case string:
		return isAuthorizationKind(typedValue)
	case []any:
		for _, item := range typedValue {
			if kind, ok := item.(string); ok && isAuthorizationKind(kind) {
				return true
			}
		}
	}
	return false
}

// containsAuthorizationKind finds protected kinds inside Kyverno match and
// target shapes, including Flux sources and controller package resources.
func containsAuthorizationKind(value any) bool {
	switch typedValue := value.(type) {
	case []any:
		for _, item := range typedValue {
			switch item.(type) {
			case []any, map[string]any:
				if containsAuthorizationKind(item) {
					return true
				}
			}
		}
	case map[string]any:
		for key, item := range typedValue {
			if (key == "kind" || key == "kinds") && kindSelectorIncludesAuthorization(item) {
				return true
			}
			switch item.(type) {
			case []any, map[string]any:
				if containsAuthorizationKind(item) {
					return true
				}
			}
		}
	}
	return false
}

// containsEmbeddedAuthorizationTemplate finds nested RBAC object templates
// emitted later by controllers such as KRO rather than by Kustomize itself.
func containsEmbeddedAuthorizationTemplate(value any, depth int) bool {
	switch typedValue := value.(type) {
	case []any:
		for _, item := range typedValue {
			if containsEmbeddedAuthorizationTemplate(item, depth+1) {
				return true
			}
		}
	case map[string]any:
		if depth > 0 {
			identity := identityOf(typedValue)
			if strings.Contains(identity.apiVersion, "${") && isAuthorizationKind(identity.kind) ||
				strings.HasPrefix(identity.apiVersion, "iam.aws.") ||
				identity.apiVersion == "rbac.authorization.k8s.io/v1" && isRBACAuthorizationKind(identity.kind) ||
				identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" && identity.kind == "Kustomization" ||
				isFluxSourceResource(identity) ||
				isControllerRBACEmitter(identity) ||
				isCurrentKyvernoMutationPolicy(identity) ||
				isLegacyKyvernoPolicy(identity) {
				return true
			}
		}
		for _, item := range typedValue {
			if containsEmbeddedAuthorizationTemplate(item, depth+1) {
				return true
			}
		}
	}
	return false
}

// isIndirectAuthorizationPolicy selects Kyverno policies that can generate or
// mutate RBAC privileges without declaring the resulting object in this render.
func isIndirectAuthorizationPolicy(document map[string]any, identity resourceIdentity) bool {
	if !isLegacyKyvernoPolicy(identity) {
		return false
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return false
	}
	rules, ok := spec["rules"].([]any)
	if !ok {
		return false
	}
	for _, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			continue
		}
		if rawGenerate, generates := rule["generate"]; generates {
			generate, ok := rawGenerate.(map[string]any)
			kind, hasKind := generate["kind"]
			if !ok || !hasKind || kind == nil || isAuthorizationKind(fmt.Sprint(kind)) {
				return true
			}
		}
		if mutate, mutates := rule["mutate"]; mutates {
			match, hasMatch := rule["match"]
			if !hasMatch || containsAuthorizationKind(match) || containsAuthorizationKind(mutate) {
				return true
			}
		}
	}
	return false
}

// containsFluxSubstitution finds unresolved post-build substitution tokens in
// parsed YAML values before they can change an authorization identity at apply.
func containsFluxSubstitution(value any) bool {
	switch typedValue := value.(type) {
	case string:
		return strings.Contains(typedValue, "${")
	case []any:
		for _, item := range typedValue {
			if containsFluxSubstitution(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typedValue {
			if strings.Contains(key, "${") || containsFluxSubstitution(item) {
				return true
			}
		}
	}
	return false
}

// containsSOPSCiphertext finds encrypted scalar values that the static render
// cannot semantically classify before Flux decrypts them in the cluster.
func containsSOPSCiphertext(value any) bool {
	switch typedValue := value.(type) {
	case string:
		return strings.Contains(typedValue, "ENC[AES256_GCM,")
	case []any:
		for _, item := range typedValue {
			if containsSOPSCiphertext(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typedValue {
			if containsSOPSCiphertext(item) {
				return true
			}
		}
	}
	return false
}

// isSOPSEncrypted recognizes both standard root metadata and encrypted values.
func isSOPSEncrypted(document map[string]any) bool {
	_, hasMetadata := document["sops"]
	return hasMetadata || containsSOPSCiphertext(document)
}

// hasDisabledFluxSubstitution distinguishes controller template expressions
// from post-build variables when Flux is explicitly forbidden from expanding
// the document. The document remains subject to its exact authorization hash.
func hasDisabledFluxSubstitution(document map[string]any) bool {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return false
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	return ok && fmt.Sprint(annotations["kustomize.toolkit.fluxcd.io/substitute"]) == "disabled"
}

// isAuthorizationCapableDocument scopes substitution rejection to resources
// that can directly or indirectly change the EKS CI authorization surface.
func isAuthorizationCapableDocument(document map[string]any, identity resourceIdentity) bool {
	if strings.Contains(identity.apiVersion, "${") || strings.Contains(identity.kind, "${") {
		return true
	}
	if strings.HasPrefix(identity.apiVersion, "iam.aws.") ||
		identity.apiVersion == "rbac.authorization.k8s.io/v1" {
		return true
	}
	if identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" && identity.kind == "Kustomization" ||
		isFluxSourceResource(identity) ||
		isControllerRBACEmitter(identity) ||
		isCurrentKyvernoMutationPolicy(identity) ||
		isLegacyKyvernoPolicy(identity) {
		return true
	}
	return isIndirectAuthorizationPolicy(document, identity) ||
		containsEmbeddedAuthorizationTemplate(document, 0)
}

// isAuthorizationResource selects every rendered object capable of changing
// the EKS CI identity's IAM, RBAC, or Flux authorization surface.
func isAuthorizationResource(
	document map[string]any,
	identity resourceIdentity,
) bool {
	if strings.HasPrefix(identity.apiVersion, "iam.aws.") {
		return true
	}
	if identity.apiVersion == "rbac.authorization.k8s.io/v1" {
		if identity.kind == "RoleBinding" || identity.kind == "ClusterRoleBinding" ||
			grantsAuthorizationControl(document) {
			return true
		}
		if identity.namespace == "aws" &&
			identity.kind == "Role" {
			return true
		}
	}
	if isIndirectAuthorizationPolicy(document, identity) ||
		isCurrentKyvernoMutationPolicy(identity) ||
		isFluxSourceResource(identity) ||
		isControllerRBACEmitter(identity) {
		return true
	}
	if containsEmbeddedAuthorizationTemplate(document, 0) {
		return true
	}
	return identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" &&
		identity.kind == "Kustomization"
}

// bindingRoleIdentity returns the Role or ClusterRole resolved by one binding.
func bindingRoleIdentity(document map[string]any, identity resourceIdentity) (resourceIdentity, bool) {
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "RoleBinding" && identity.kind != "ClusterRoleBinding") {
		return resourceIdentity{}, false
	}
	roleRef, ok := document["roleRef"].(map[string]any)
	if !ok || fmt.Sprint(roleRef["apiGroup"]) != "rbac.authorization.k8s.io" {
		return resourceIdentity{}, false
	}
	kind := fmt.Sprint(roleRef["kind"])
	name := fmt.Sprint(roleRef["name"])
	if name == "" || kind != "Role" && kind != "ClusterRole" {
		return resourceIdentity{}, false
	}
	namespace := ""
	if kind == "Role" {
		namespace = identity.namespace
	}
	return resourceIdentity{
		apiVersion: "rbac.authorization.k8s.io/v1",
		kind:       kind,
		namespace:  namespace,
		name:       name,
	}, true
}

// labelsMatchSelector implements the aggregation label-selector shapes used by RBAC.
func labelsMatchSelector(labels map[string]any, selector map[string]any) bool {
	if matchLabels, ok := selector["matchLabels"].(map[string]any); ok {
		for key, expected := range matchLabels {
			if fmt.Sprint(labels[key]) != fmt.Sprint(expected) {
				return false
			}
		}
	}
	expressions, ok := selector["matchExpressions"].([]any)
	if !ok {
		return true
	}
	for _, rawExpression := range expressions {
		expression, ok := rawExpression.(map[string]any)
		if !ok {
			return true
		}
		key := fmt.Sprint(expression["key"])
		actual, exists := labels[key]
		switch fmt.Sprint(expression["operator"]) {
		case "In":
			if !exists || !stringListIncludes(expression["values"], fmt.Sprint(actual)) {
				return false
			}
		case "NotIn":
			if exists && stringListIncludes(expression["values"], fmt.Sprint(actual)) {
				return false
			}
		case "Exists":
			if !exists {
				return false
			}
		case "DoesNotExist":
			if exists {
				return false
			}
		default:
			return true
		}
	}
	return true
}

// aggregationSelectors returns every selector that contributes to one role.
func aggregationSelectors(document map[string]any) []map[string]any {
	aggregationRule, ok := document["aggregationRule"].(map[string]any)
	if !ok {
		return nil
	}
	rawSelectors, ok := aggregationRule["clusterRoleSelectors"].([]any)
	if !ok {
		return nil
	}
	selectors := make([]map[string]any, 0, len(rawSelectors))
	for _, rawSelector := range rawSelectors {
		if selector, ok := rawSelector.(map[string]any); ok {
			selectors = append(selectors, selector)
		}
	}
	return selectors
}

// authorizationRoleIdentities finds bound roles and transitive aggregation contributors.
func authorizationRoleIdentities(documents []map[string]any) map[resourceIdentity]bool {
	selected := make(map[resourceIdentity]bool)
	clusterRoles := make(map[resourceIdentity]map[string]any)
	for _, document := range documents {
		identity := identityOf(document)
		if roleIdentity, ok := bindingRoleIdentity(document, identity); ok {
			selected[roleIdentity] = true
		}
		if identity.apiVersion == "rbac.authorization.k8s.io/v1" && identity.kind == "ClusterRole" {
			clusterRoles[identity] = document
		}
	}
	for changed := true; changed; {
		changed = false
		selectors := make([]map[string]any, 0, len(selected))
		for identity := range selected {
			if identity.kind != "ClusterRole" {
				continue
			}
			selectors = append(selectors, map[string]any{"matchLabels": map[string]any{
				"rbac.authorization.k8s.io/aggregate-to-" + identity.name: "true",
			}})
			selectors = append(selectors, aggregationSelectors(clusterRoles[identity])...)
		}
		for identity, document := range clusterRoles {
			if selected[identity] {
				continue
			}
			metadata, _ := document["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			for _, selector := range selectors {
				if labelsMatchSelector(labels, selector) {
					selected[identity] = true
					changed = true
					break
				}
			}
		}
	}
	return selected
}

// authorizationSubstitutionSourceIdentities finds every Flux post-build input.
func authorizationSubstitutionSourceIdentities(documents []map[string]any) map[resourceIdentity]bool {
	selected := make(map[resourceIdentity]bool)
	for _, document := range documents {
		identity := identityOf(document)
		if identity.apiVersion != "kustomize.toolkit.fluxcd.io/v1" || identity.kind != "Kustomization" {
			continue
		}
		spec, ok := document["spec"].(map[string]any)
		if !ok {
			continue
		}
		postBuild, ok := spec["postBuild"].(map[string]any)
		if !ok {
			continue
		}
		references, ok := postBuild["substituteFrom"].([]any)
		if !ok {
			continue
		}
		for _, rawReference := range references {
			reference, ok := rawReference.(map[string]any)
			if !ok {
				continue
			}
			kind := fmt.Sprint(reference["kind"])
			name := fmt.Sprint(reference["name"])
			if name == "" || kind != "ConfigMap" && kind != "Secret" {
				continue
			}
			selected[resourceIdentity{
				apiVersion: "v1",
				kind:       kind,
				namespace:  identity.namespace,
				name:       name,
			}] = true
		}
	}
	return selected
}

// authorizationTemplateInstanceTypes finds CRDs whose instances emit authorization.
func authorizationTemplateInstanceTypes(documents []map[string]any) map[resourceType]bool {
	selected := make(map[resourceType]bool)
	for _, document := range documents {
		identity := identityOf(document)
		if !strings.HasPrefix(identity.apiVersion, "kro.run/") ||
			identity.kind != "ResourceGraphDefinition" ||
			!containsEmbeddedAuthorizationTemplate(document, 0) {
			continue
		}
		schema, err := nestedMap(document, "spec", "schema")
		if err != nil {
			continue
		}
		apiVersion := fmt.Sprint(schema["apiVersion"])
		kind := fmt.Sprint(schema["kind"])
		if apiVersion == "" || kind == "" {
			continue
		}
		if !strings.Contains(apiVersion, "/") {
			dot := strings.Index(identity.name, ".")
			if dot < 0 || dot == len(identity.name)-1 {
				continue
			}
			apiVersion = identity.name[dot+1:] + "/" + apiVersion
		}
		selected[resourceType{apiVersion: apiVersion, kind: kind}] = true
	}
	return selected
}

// authorizationSurfaceEntry serializes one selected object with its complete
// identity so the aggregate hash preserves additions, removals, and duplicates.
func authorizationSurfaceEntry(identity resourceIdentity, document map[string]any) (string, error) {
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("marshal authorization surface entry: %w", err)
	}
	return strings.Join([]string{
		identity.apiVersion,
		identity.kind,
		identity.namespace,
		identity.name,
		string(canonical),
	}, "\x00"), nil
}

// validateRendered requires the complete selected authorization surface to
// match one canonical hash while preserving precise core-object diagnostics.
func validateRendered(rendered []byte) error {
	documents, err := decodeDocuments(rendered)
	if err != nil {
		return err
	}
	roleIdentities := authorizationRoleIdentities(documents)
	substitutionSourceIdentities := authorizationSubstitutionSourceIdentities(documents)
	templateInstanceTypes := authorizationTemplateInstanceTypes(documents)
	seen := make(map[resourceIdentity]bool, len(expectedRenderedHashes))
	surfaceEntries := make([]string, 0, len(expectedRenderedHashes))
	problems := make([]error, 0)
	substitutionProblems := make([]error, 0)
	for _, document := range documents {
		identity := identityOf(document)
		isAuthorizationCapable := isAuthorizationCapableDocument(document, identity)
		hasAuthorizationSubstitution := isAuthorizationCapable &&
			containsFluxSubstitution(document) &&
			!hasDisabledFluxSubstitution(document)
		hasEncryptedAuthorization := isAuthorizationCapable && isSOPSEncrypted(document)
		instanceType := resourceType{apiVersion: identity.apiVersion, kind: identity.kind}
		if !roleIdentities[identity] && !substitutionSourceIdentities[identity] &&
			!templateInstanceTypes[instanceType] &&
			!hasAuthorizationSubstitution && !hasEncryptedAuthorization &&
			!isAuthorizationResource(document, identity) {
			continue
		}
		entry, entryErr := authorizationSurfaceEntry(identity, document)
		if entryErr != nil {
			problems = append(problems, entryErr)
			continue
		}
		surfaceEntries = append(surfaceEntries, entry)
		actual, hashErr := canonicalFingerprint(document)
		if hashErr != nil {
			problems = append(problems, hashErr)
			continue
		}
		if hasEncryptedAuthorization {
			problems = append(problems, fmt.Errorf(
				"encrypted SOPS authorization resource cannot be validated before reconciliation: %+v fingerprint: %s",
				identity,
				actual,
			))
		}
		if hasAuthorizationSubstitution {
			substitutionProblems = append(substitutionProblems, fmt.Errorf(
				"unresolved Flux substitution in authorization resource: %+v fingerprint: %s",
				identity,
				actual,
			))
		}
		expected, ok := expectedRenderedHashes[identity]
		if !ok {
			continue
		}
		if seen[identity] {
			problems = append(problems, fmt.Errorf("duplicate rendered authorization resource: %+v", identity))
			continue
		}
		seen[identity] = true
		if actual != expected {
			problems = append(problems, fmt.Errorf("unapproved rendered %+v fingerprint: %s", identity, actual))
		}
	}
	for identity := range expectedRenderedHashes {
		if !seen[identity] {
			problems = append(problems, fmt.Errorf("missing rendered authorization resource: %+v", identity))
		}
	}
	// substitutionProblems are DIAGNOSTIC, not a control, and that is deliberate.
	// The control is surface MEMBERSHIP: a resource carrying an unresolved
	// substitution is forced into the aggregate surface above, so its text —
	// including the `${…}` literal — is covered by the fingerprint and cannot
	// change without moving it. Emitting the notes only alongside a mismatch is
	// what keeps them useful: they explain a hash that moved.
	//
	// Measured 2026-07-21: promoting them to unconditional errors fails the
	// committed, approved tree on THIRTY-plus HelmReleases, because post-build
	// substitution is the platform's normal configuration mechanism and
	// containsFluxSubstitution matches a document anywhere. A validator that is
	// red on the approved state is not a stricter gate, it is a disabled one.
	sort.Strings(surfaceEntries)
	canonicalSurface, marshalErr := json.Marshal(surfaceEntries)
	if marshalErr != nil {
		problems = append(problems, fmt.Errorf("marshal authorization surface: %w", marshalErr))
		problems = append(problems, substitutionProblems...)
	} else if actualSurfaceSHA := fingerprint(canonicalSurface); actualSurfaceSHA != expectedRenderedSurfaceSHA {
		problems = append(problems, fmt.Errorf(
			"unapproved rendered authorization surface fingerprint: %s",
			actualSurfaceSHA,
		))
		problems = append(problems, substitutionProblems...)
	}
	return errors.Join(problems...)
}

// validateAuthorization combines source and final-render checks so neither
// Kustomize transformations nor source edits can bypass the contract.
func validateAuthorization(role []byte, boundary []byte, rendered []byte) error {
	if err := validateRole(role); err != nil {
		return err
	}
	if err := validateBoundary(boundary); err != nil {
		return err
	}
	return validateRendered(rendered)
}

// validateRendererVersion pins kubectl and its embedded Kustomize version,
// keeping canonical render hashes reproducible across CI and local validation.
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

// commandOutput runs a repository-controlled command under the caller's
// deadline and includes its output in failures instead of returning a false red.
func commandOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // Fixed binary and repository-controlled arguments.
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return output, nil
}

// renderAuthorizationLayers renders every independently reconciled production
// layer and joins them into one YAML stream for fail-closed authorization checks.
func renderAuthorizationLayers(ctx context.Context, repoRoot string, execute commandExecutor) ([]byte, error) {
	var rendered bytes.Buffer
	for _, overlayPath := range authorizationOverlayPaths {
		layer, err := execute(ctx, "kubectl", "kustomize", filepath.Join(repoRoot, overlayPath))
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", overlayPath, err)
		}
		if rendered.Len() > 0 {
			if previous := rendered.Bytes(); previous[len(previous)-1] != '\n' {
				_ = rendered.WriteByte('\n')
			}
			_, _ = rendered.WriteString("---\n")
		}
		_, _ = rendered.Write(layer)
	}
	return rendered.Bytes(), nil
}

// run executes the complete repository-root authorization validation and
// returns a process-compatible status without mutating cluster state.
func run(repoRoot string, stdout io.Writer, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), rendererCommandTimeout)
	defer cancel()

	version, err := commandOutput(ctx, "kubectl", "version", "--client", "-o", "json")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	if err := validateRendererVersion(version); err != nil {
		_, _ = fmt.Fprintf(stderr, "EKS CI role policy: %v\n", err)
		return 1
	}
	rendered, err := renderAuthorizationLayers(ctx, repoRoot, commandOutput)
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

// runCLI enforces the single explicit repository-root argument before invoking
// validation, preventing ambient working-directory assumptions.
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: validate-eks-ci-role-policy <repository-root>")
		return 2
	}
	return run(args[0], stdout, stderr)
}

// main executes the validator process and returns its contract result to CI.
func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
