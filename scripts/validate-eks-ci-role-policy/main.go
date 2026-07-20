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

	expectedKubectlVersion   = "v1.36.2"
	expectedKustomizeVersion = "v5.8.1"
	expectedRoleManifestSHA  = "96a77d18160c450340e65b0953f44016a01a08429416f7a82142c3f90a61ca07"
	expectedBoundarySHA      = "b96bfd8c96baa2e09f32a1cc05f76473ecc021fed554a2880ce8e3dd399902c7"
	expectedTrustPolicySHA   = "85d5d45343f9eac5fdc35717c85c88c5b0f8fde9eddffb169c3a223617fd0a5e"
	expectedInlinePolicySHA  = "60e3086a6d3dac0092ffe8264c04ebae783c0d38f19a3cf073ed8991085a4df8"
	expectedBoundaryJSONSHA  = "e617004bce71a65f92934c4f7575d7559a290afe7a17363ce12db8ad7b519610"
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

// expectedRenderedHashes pins every selected EKS CI authorization object that
// may survive the final render; anything else in that surface fails closed.
var expectedRenderedHashes = map[resourceIdentity]string{
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Role", namespace: "aws", name: "eks-ci"}:                             "0967890d16316a8cfcb1cca8a52085c6989c42000fafbbd0ada6323d4e15c97c",
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Policy", namespace: "aws", name: "eks-ci-smoke-boundary"}:            "66f79a06cd8f789f6a2dd66b263c3f4459447f96227f57996591d75b441b0104",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "aws", name: "aws-managed-resources"}:              "ff4c3264c519b1b4a7ec9b5145412f39ea2ba7b6163d8dc50fb029b1460edcda",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "aws", name: "aws-managed-resources"}:       "d846c8d9810dd7c0cba33612d2de63183403ccb07c4d5a5c90d0563a444cd714",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "crossview", name: "crossview-portforward"}: "78992d9727763fdcf1bda05969fdc881e6d0e54cc72efc07555304b47d25bc3a",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: "kro-tenant-rgd"}:                                "4447f41c03e8297fafdabcadf4fdd8ca3260f2c84264c531b2179cb7df2c1556",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-cluster-reader"}:                    "7d896404f02d6418c289065d73f9ad79345217d76c8d89eadca2c06e6066b487",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-view"}:                              "4d07ba3a995cfc139351b4227739efeba9348777f7fe47ac69b87d08e70bd45f",
	{apiVersion: "kro.run/v1alpha1", kind: "ResourceGraphDefinition", name: "tenant.kro.run"}:                                "404de3502423d08af04eaa5d1ca6a6b76634ae09c270e7718994cfd346c8a07f",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "aws", name: "aws"}:                     "7bde9c682a81b752bdf9d2b14ce69ca1690008a39f2562d4887f8200447dea71",
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

// includesAWSServiceAccountIdentity recognizes every Kubernetes principal that
// can confer privileges on the aws/aws service account, including broad groups.
func includesAWSServiceAccountIdentity(document map[string]any) bool {
	subjects, ok := document["subjects"].([]any)
	if !ok {
		return false
	}
	for _, rawSubject := range subjects {
		subject, ok := rawSubject.(map[string]any)
		if !ok {
			continue
		}
		kind := fmt.Sprint(subject["kind"])
		name := fmt.Sprint(subject["name"])
		if kind == "ServiceAccount" && name == "aws" && subject["namespace"] == "aws" {
			return true
		}
		if kind == "User" && name == "system:serviceaccount:aws:aws" {
			return true
		}
		if kind == "Group" && (name == "system:serviceaccounts:aws" ||
			name == "system:serviceaccounts" || name == "system:authenticated") {
			return true
		}
	}
	return false
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

// grantsAuthorizationWrites identifies Roles and ClusterRoles that can mutate
// RBAC privileges or use bind/escalate, including wildcard grants.
func grantsAuthorizationWrites(document map[string]any) bool {
	identity := identityOf(document)
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "Role" && identity.kind != "ClusterRole") {
		return false
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
		if !stringListIncludes(rule["apiGroups"], "rbac.authorization.k8s.io", "*") ||
			!stringListIncludes(
				rule["resources"],
				"roles",
				"clusterroles",
				"rolebindings",
				"clusterrolebindings",
				"*",
			) {
			continue
		}
		if stringListIncludes(
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
	}
	return false
}

// renderedRoleIdentities records every Role and ClusterRole definition visible
// in the production renders so bindings to unavailable roles fail closed.
func renderedRoleIdentities(documents []map[string]any) map[resourceIdentity]struct{} {
	roles := make(map[resourceIdentity]struct{})
	for _, document := range documents {
		identity := identityOf(document)
		if identity.apiVersion == "rbac.authorization.k8s.io/v1" &&
			(identity.kind == "Role" || identity.kind == "ClusterRole") {
			roles[identity] = struct{}{}
		}
	}
	return roles
}

// authorizationWriterIdentities collects every rendered role that can mutate
// RBAC bindings so grants of those roles can be pinned as part of the surface.
func authorizationWriterIdentities(documents []map[string]any) map[resourceIdentity]struct{} {
	writers := make(map[resourceIdentity]struct{})
	for _, document := range documents {
		if grantsAuthorizationWrites(document) {
			writers[identityOf(document)] = struct{}{}
		}
	}
	return writers
}

// bindingReferencesAuthorizationWriter detects direct grants of a rendered
// binding-writer Role or ClusterRole to any controller identity.
func bindingReferencesAuthorizationWriter(
	document map[string]any,
	identity resourceIdentity,
	writers map[resourceIdentity]struct{},
) bool {
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "RoleBinding" && identity.kind != "ClusterRoleBinding") {
		return false
	}
	roleRef, ok := document["roleRef"].(map[string]any)
	if !ok || fmt.Sprint(roleRef["apiGroup"]) != "rbac.authorization.k8s.io" {
		return false
	}
	roleKind := fmt.Sprint(roleRef["kind"])
	roleNamespace := ""
	if roleKind == "Role" {
		roleNamespace = identity.namespace
	} else if roleKind != "ClusterRole" {
		return false
	}
	_, ok = writers[resourceIdentity{
		apiVersion: "rbac.authorization.k8s.io/v1",
		kind:       roleKind,
		namespace:  roleNamespace,
		name:       fmt.Sprint(roleRef["name"]),
	}]
	return ok
}

// bindingReferencesUnavailableRole detects grants of built-in, chart-created,
// or otherwise absent roles whose privileges cannot be inspected in this render.
func bindingReferencesUnavailableRole(
	document map[string]any,
	identity resourceIdentity,
	roles map[resourceIdentity]struct{},
) bool {
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "RoleBinding" && identity.kind != "ClusterRoleBinding") {
		return false
	}
	roleRef, ok := document["roleRef"].(map[string]any)
	if !ok || fmt.Sprint(roleRef["apiGroup"]) != "rbac.authorization.k8s.io" {
		return false
	}
	roleKind := fmt.Sprint(roleRef["kind"])
	roleNamespace := ""
	if roleKind == "Role" {
		roleNamespace = identity.namespace
	} else if roleKind != "ClusterRole" {
		return true
	}
	_, available := roles[resourceIdentity{
		apiVersion: "rbac.authorization.k8s.io/v1",
		kind:       roleKind,
		namespace:  roleNamespace,
		name:       fmt.Sprint(roleRef["name"]),
	}]
	return !available
}

// isRBACBindingKind recognizes Kyverno's short and group-qualified binding kinds.
func isRBACBindingKind(kind string) bool {
	return strings.Contains(kind, "${") || kind == "*" || kind == "RoleBinding" || kind == "ClusterRoleBinding" ||
		(strings.HasPrefix(kind, "rbac.authorization.k8s.io/") && strings.HasSuffix(kind, "/*")) ||
		strings.HasSuffix(kind, "/RoleBinding") || strings.HasSuffix(kind, "/ClusterRoleBinding")
}

// kindSelectorIncludesRBACBinding checks the value of a Kyverno kind/kinds key.
func kindSelectorIncludesRBACBinding(value any) bool {
	switch typedValue := value.(type) {
	case string:
		return isRBACBindingKind(typedValue)
	case []any:
		for _, item := range typedValue {
			if kind, ok := item.(string); ok && isRBACBindingKind(kind) {
				return true
			}
		}
	}
	return false
}

// containsRBACBindingKind finds binding kinds inside Kyverno match/target shapes.
func containsRBACBindingKind(value any) bool {
	switch typedValue := value.(type) {
	case []any:
		for _, item := range typedValue {
			switch item.(type) {
			case []any, map[string]any:
				if containsRBACBindingKind(item) {
					return true
				}
			}
		}
	case map[string]any:
		for key, item := range typedValue {
			if (key == "kind" || key == "kinds") && kindSelectorIncludesRBACBinding(item) {
				return true
			}
			switch item.(type) {
			case []any, map[string]any:
				if containsRBACBindingKind(item) {
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
		if depth > 0 && fmt.Sprint(typedValue["apiVersion"]) == "rbac.authorization.k8s.io/v1" {
			kind := fmt.Sprint(typedValue["kind"])
			if kind == "Role" || kind == "ClusterRole" || kind == "RoleBinding" || kind == "ClusterRoleBinding" {
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
// mutate RBAC bindings without declaring the resulting binding in this render.
func isIndirectAuthorizationPolicy(document map[string]any, identity resourceIdentity) bool {
	if !strings.HasPrefix(identity.apiVersion, "kyverno.io/") ||
		(identity.kind != "Policy" && identity.kind != "ClusterPolicy") {
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
			if !ok || !hasKind || kind == nil || isRBACBindingKind(fmt.Sprint(kind)) {
				return true
			}
		}
		if mutate, mutates := rule["mutate"]; mutates {
			match, hasMatch := rule["match"]
			if !hasMatch || containsRBACBindingKind(match) || containsRBACBindingKind(mutate) {
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
	if identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" && identity.kind == "Kustomization" {
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
	writers map[resourceIdentity]struct{},
	roles map[resourceIdentity]struct{},
) bool {
	if strings.HasPrefix(identity.apiVersion, "iam.aws.") {
		return true
	}
	if identity.apiVersion == "rbac.authorization.k8s.io/v1" {
		if grantsAuthorizationWrites(document) ||
			bindingReferencesAuthorizationWriter(document, identity, writers) ||
			bindingReferencesUnavailableRole(document, identity, roles) {
			return true
		}
		if identity.namespace == "aws" &&
			(identity.kind == "Role" || identity.kind == "RoleBinding") {
			return true
		}
		// Follow the privileged subject across namespaces and cluster scope. The
		// rendered allowlist then pins the one approved binding, including roleRef.
		if (identity.kind == "RoleBinding" || identity.kind == "ClusterRoleBinding") &&
			includesAWSServiceAccountIdentity(document) {
			return true
		}
	}
	if isIndirectAuthorizationPolicy(document, identity) {
		return true
	}
	if containsEmbeddedAuthorizationTemplate(document, 0) {
		return true
	}
	return identity.namespace == "aws" &&
		identity.apiVersion == "kustomize.toolkit.fluxcd.io/v1" &&
		identity.kind == "Kustomization"
}

// validateRendered requires each approved authorization object exactly once
// and rejects additions, aliases, omissions, duplicates, or structural drift.
func validateRendered(rendered []byte) error {
	documents, err := decodeDocuments(rendered)
	if err != nil {
		return err
	}
	writers := authorizationWriterIdentities(documents)
	roles := renderedRoleIdentities(documents)
	seen := make(map[resourceIdentity]bool, len(expectedRenderedHashes))
	problems := make([]error, 0)
	for _, document := range documents {
		identity := identityOf(document)
		hasAuthorizationSubstitution := isAuthorizationCapableDocument(document, identity) &&
			containsFluxSubstitution(document) &&
			!hasDisabledFluxSubstitution(document)
		if !hasAuthorizationSubstitution && !isAuthorizationResource(document, identity, writers, roles) {
			continue
		}
		actual, hashErr := canonicalFingerprint(document)
		if hashErr != nil {
			problems = append(problems, hashErr)
			continue
		}
		expected, ok := expectedRenderedHashes[identity]
		if !ok {
			if hasAuthorizationSubstitution {
				problems = append(problems, fmt.Errorf(
					"unresolved Flux substitution in authorization resource: %+v fingerprint: %s",
					identity,
					actual,
				))
			} else {
				problems = append(problems, fmt.Errorf(
					"unexpected rendered authorization resource: %+v fingerprint: %s",
					identity,
					actual,
				))
			}
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

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
