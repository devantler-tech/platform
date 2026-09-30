// Command validate-eks-ci-role-policy pins the privileges production grants to
// the EKS CI identity.
//
// The permissions that identity ends up with are not written in one place: they
// are the sum of several independently reconciled overlays, so a change in any
// one of them can widen the identity's reach without that being visible in the
// diff under review. This command renders each of those overlays and compares
// the result against approved fingerprints, so an unreviewed privilege grant
// fails CI instead of reaching the cluster.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	expectedBoundarySHA      = "6e79792b08aa023900734d31c45d6abe1765991ad16b63e84598cc8d7d5b05af"
	expectedTrustPolicySHA   = "85d5d45343f9eac5fdc35717c85c88c5b0f8fde9eddffb169c3a223617fd0a5e"
	expectedInlinePolicySHA  = "60e3086a6d3dac0092ffe8264c04ebae783c0d38f19a3cf073ed8991085a4df8"
	expectedBoundaryJSONSHA  = "2c9bc1ce56efeb6fa30d885d5f9dff8d5d8129a07d9393ccdeb376605cbc5ad8"
)

// approvedSurfaceLedger is the approved rendered authorization surface: one
// line per selected document, `<apiVersion>|<kind>|<namespace>|<name> <sha256>`,
// sorted, separated by blank lines, after a `#` comment header. The rendered
// surface must equal it exactly, which is the same guarantee the single
// aggregate digest gave before platform#3182, but two changes that move
// different documents now touch different lines and merge cleanly instead of
// serializing on one constant.
// Evidence for earlier approvals is in approved-surface-history.md.
//
//go:embed approved-surface.txt
var approvedSurfaceLedger string

// authorizationOverlayPaths lists every independently reconciled production
// layer where an object can grant privileges to the aws/aws service account.
var authorizationOverlayPaths = []string{
	appsOverlayPath,
	infrastructureOverlayPath,
	controllerOverlayPath,
	bootstrapOverlayPath,
	rootProductionOverlayPath,
}

// exactPinnedHelmChartVersion accepts one immutable SemVer selector, including
// the optional v prefix and prerelease/build suffixes used by this portfolio.
// Ranges, wildcards, substitutions, and omitted versions remain fingerprinted
// verbatim because they let a chart move without a reviewed dependency PR.
var exactPinnedHelmChartVersion = regexp.MustCompile(
	`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
)

var exactGitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var exactSHA256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const authorizationIsolationAnnotation = "security.devantler.tech/authorization-scope"

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

// validatePinnedUnifiSource keeps the external repository from moving without
// a reviewed Platform change. This source retains patch/update authority over
// live UniFi managed resources, so a branch, tag, abbreviated hash, alternate
// repository, or mixed selector is not an acceptable provenance boundary.
func validatePinnedUnifiSource(document map[string]any, identity resourceIdentity) error {
	wantIdentity := resourceIdentity{
		apiVersion: "source.toolkit.fluxcd.io/v1",
		kind:       "GitRepository",
		namespace:  "unifi",
		name:       "unifi",
	}
	if identity != wantIdentity {
		return nil
	}

	spec, ok := document["spec"].(map[string]any)
	if !ok || spec["url"] != "https://github.com/devantler-tech/unifi" {
		return errors.New("unifi GitRepository must use the trusted devantler-tech/unifi source")
	}
	ref, ok := spec["ref"].(map[string]any)
	if !ok || len(ref) != 1 {
		return errors.New("unifi GitRepository must pin exactly one full immutable commit")
	}
	commit, ok := ref["commit"].(string)
	if !ok || !exactGitCommit.MatchString(commit) {
		return errors.New("unifi GitRepository must pin exactly one full immutable commit")
	}
	verify, ok := spec["verify"].(map[string]any)
	if !ok || len(verify) != 2 || verify["mode"] != "HEAD" {
		return errors.New("unifi GitRepository must verify the pinned HEAD commit")
	}
	secretRef, ok := verify["secretRef"].(map[string]any)
	if !ok || len(secretRef) != 1 || secretRef["name"] != "unifi-git-signing-keys" {
		return errors.New("unifi GitRepository must trust only the reviewed unifi Git signing key Secret")
	}
	return nil
}

const expectedUnifiPublicMaterialSHA = "40ce89d21fb075092d256f9fbf62a1c19299d3282cb913d3e61d08235d0c491a"

// validateUnifiSigningKey pins the public trust root used by the UniFi
// GitRepository. A secretRef alone only proves that Flux will look up a name;
// it says nothing about which signer that Secret actually trusts.
func validateUnifiSigningKey(documents []map[string]any) error {
	wantIdentity := resourceIdentity{
		apiVersion: "v1",
		kind:       "Secret",
		namespace:  "unifi",
		name:       "unifi-git-signing-keys",
	}
	count := 0
	for _, document := range documents {
		if identityOf(document) != wantIdentity {
			continue
		}
		count++
		if document["type"] != "Opaque" {
			return errors.New("unifi Git signing key Secret must be type Opaque")
		}
		if _, exists := document["data"]; exists {
			return errors.New("unifi Git signing key Secret must not contain data")
		}
		stringData, ok := document["stringData"].(map[string]any)
		if !ok || len(stringData) != 1 {
			return errors.New("unifi Git signing key Secret must contain only github.asc")
		}
		key, ok := stringData["github.asc"].(string)
		if !ok || key == "" {
			return errors.New("unifi Git signing key Secret must contain github.asc")
		}
		if actual := fingerprint([]byte(key)); actual != expectedUnifiPublicMaterialSHA {
			return fmt.Errorf("unapproved unifi Git signing key fingerprint: %s", actual)
		}
	}
	if count != 1 {
		return fmt.Errorf("unifi Git signing key Secret count is %d, want exactly 1", count)
	}
	return nil
}

// validateUnifiPruneExemption keeps the deliberate non-pruning reconciler
// deployable while requiring the admission-policy exception to remain scoped
// to exactly one namespaced Kustomization.
func validateUnifiPruneExemption(document map[string]any, identity resourceIdentity) error {
	wantIdentity := resourceIdentity{
		apiVersion: "kyverno.io/v1",
		kind:       "ClusterPolicy",
		name:       "enforce-flux-best-practices",
	}
	if identity != wantIdentity {
		return nil
	}

	exactSingleton := func(value any, want string) bool {
		values, ok := value.([]any)
		return ok && len(values) == 1 && values[0] == want
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return errors.New("enforce-flux-best-practices must exempt only unifi/unifi from prune enforcement")
	}
	rules, ok := spec["rules"].([]any)
	if !ok {
		return errors.New("enforce-flux-best-practices must exempt only unifi/unifi from prune enforcement")
	}
	for _, ruleValue := range rules {
		rule, ok := ruleValue.(map[string]any)
		if !ok || rule["name"] != "kustomization-recommended-settings" {
			continue
		}
		exclude, ok := rule["exclude"].(map[string]any)
		if !ok {
			break
		}
		exclusions, ok := exclude["any"].([]any)
		if !ok {
			break
		}
		seenFluxSystem := false
		seenUnifi := false
		for _, exclusionValue := range exclusions {
			exclusion, ok := exclusionValue.(map[string]any)
			if !ok {
				break
			}
			resources, ok := exclusion["resources"].(map[string]any)
			if !ok {
				break
			}
			switch {
			case exactSingleton(resources["namespaces"], "flux-system") &&
				exactSingleton(resources["names"], "flux-system") && !seenFluxSystem:
				seenFluxSystem = true
			case exactSingleton(resources["namespaces"], "unifi") &&
				exactSingleton(resources["names"], "unifi") && !seenUnifi:
				seenUnifi = true
			default:
				return errors.New("enforce-flux-best-practices must exempt only unifi/unifi from prune enforcement")
			}
		}
		if seenFluxSystem && seenUnifi && len(exclusions) == 2 {
			return nil
		}
		break
	}
	return errors.New("enforce-flux-best-practices must exempt only unifi/unifi from prune enforcement")
}

// expectedRenderedHashes preserves object-specific diagnostics for the core
// EKS CI identities while the aggregate surface hash pins every selected
// source, controller, binding, and indirect authorization object.
var expectedRenderedHashes = map[resourceIdentity]string{
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Role", namespace: "aws", name: "eks-ci"}:                                        "0967890d16316a8cfcb1cca8a52085c6989c42000fafbbd0ada6323d4e15c97c",
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Policy", namespace: "aws", name: "eks-ci-smoke-boundary"}:                       "6f14b5243c945d0d2230821733ea12096d6e92ab155a35482b20a6080c03c037",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "aws", name: "aws-managed-resources"}:                         "ff4c3264c519b1b4a7ec9b5145412f39ea2ba7b6163d8dc50fb029b1460edcda",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "aws", name: "aws-managed-resources"}:                  "d846c8d9810dd7c0cba33612d2de63183403ccb07c4d5a5c90d0563a444cd714",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: "kro-tenant-rgd"}:                                           "4447f41c03e8297fafdabcadf4fdd8ca3260f2c84264c531b2179cb7df2c1556",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "crossview-cluster-reader"}:                          "bc6c370f5bff72c541428274f9ef7ab13e3bb5a2b804ec5f4c81087171311c0d",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "crossview-view"}:                                    "536a4baa1970100ea117d1655f80e06ed874e2248b75f33f161e8b44ca3df50c",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-cluster-reader"}:                               "7d896404f02d6418c289065d73f9ad79345217d76c8d89eadca2c06e6066b487",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-view"}:                                         "4d07ba3a995cfc139351b4227739efeba9348777f7fe47ac69b87d08e70bd45f",
	{apiVersion: "kro.run/v1alpha1", kind: "ResourceGraphDefinition", name: "tenant.kro.run"}:                                           "072e4478cdad39c0a7d9f5119cad63d4c56a9fc96ba88d657fef97f6b91bae31",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "ascoachingogvaner", name: "ascoachingogvaner"}:    "89ea0484e37b691594b7a72be2ca2de285697818bf88a5b37b4fa8a9161c54fa",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "aws", name: "aws"}:                                "7bde9c682a81b752bdf9d2b14ce69ca1690008a39f2562d4887f8200447dea71",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "apps"}:                       "ee11a54686a68eb49b833b234949f9d21a7b8106c1b3ae677e5c205e5506f6ac",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "bootstrap"}:                  "7f674a1762f298330c7c9e4d9d4e8bf46108b10727e02a25ca5096d7913cc0a7",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure"}:             "d1bc403b6458bd22cf967bd570e24718341cbd584f58e7f0069aaffe1e187945",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure-controllers"}: "9d9b62d3221442d6355d16a34d31c198619fb3b3728df960fd67222a531ece7b",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "github-config", name: "github-config"}:            "8e9f72b0f4f982d050aff0b97d246c68b538cbc397cdd45d031c95cfae981e7c",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "unifi", name: "unifi"}:                            "33a579299700de2467631854bac4982d3e14caa3bad8cbcd2613ac180b30af32",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "wedding-app", name: "wedding-app"}:                "eb6253380641dbfac9936b4a2c938524d4f4cc9352d45ab18e8fdfd9e59bf8a8",
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

// authorizationIsolationRequested identifies the explicit reviewed contract
// used by namespace-local charts whose effective RBAC is validated after Helm
// rendering. Merely adding the annotation never makes it valid.
func authorizationIsolationRequested(document map[string]any) bool {
	metadata, ok := document["metadata"].(map[string]any)
	if !ok {
		return false
	}
	annotations, ok := metadata["annotations"].(map[string]any)
	return ok && fmt.Sprint(annotations[authorizationIsolationAnnotation]) == "isolated-chart"
}

// reviewedIsolatedChartIdentities is the explicit allowlist of namespace/name
// pairs whose isolated-chart declaration and exact rendered-child namespace
// gate have both been reviewed. Each entry needs a path-scoped Helm render that
// rejects every namespaced child outside the identity's namespace; the
// data-product-controller gate is test-isolated-chart-namespace-rules.sh.
//
// A namespace denylist is not sufficient. Denying only "", "aws" and
// "flux-system" leaves every other namespace able to exempt itself from the
// production CEL authorization rules by adding the annotation to its own
// manifest -- the resource declaring the scope is the same resource the scope
// is granted to, so nothing outside this list constrains it. Adding an entry
// here is a deliberate, reviewed act.
var reviewedIsolatedChartIdentities = map[string]struct{}{
	"data-product-controller/data-product-controller": {},
}

// validateAuthorizationIsolation fails closed on every precondition behind an
// isolated-chart declaration. It is intentionally limited to a namespace-local
// HelmRelease and its immutable public OCIRepository. A separate path-scoped
// KSail gate Helm-renders that exact artifact, constrains every child namespace,
// and applies the production CEL authorization rules to its effective RBAC.
func validateAuthorizationIsolation(document map[string]any, identity resourceIdentity) error {
	if !authorizationIsolationRequested(document) {
		return nil
	}
	if _, reviewed := reviewedIsolatedChartIdentities[identity.namespace+"/"+identity.name]; !reviewed {
		return fmt.Errorf("isolated-chart authorization scope is not reviewed for identity %q", identity.namespace+"/"+identity.name)
	}

	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return errors.New("isolated-chart resource must have an object spec")
	}
	optionalString := func(object map[string]any, key string) string {
		value, exists := object[key]
		if !exists || value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
	switch {
	case strings.HasPrefix(identity.apiVersion, "helm.toolkit.fluxcd.io/") && identity.kind == "HelmRelease":
		if targetNamespace := optionalString(spec, "targetNamespace"); targetNamespace != "" && targetNamespace != identity.namespace {
			return errors.New("isolated-chart HelmRelease must target only its own namespace")
		}
		if storageNamespace := optionalString(spec, "storageNamespace"); storageNamespace != "" && storageNamespace != identity.namespace {
			return errors.New("isolated-chart HelmRelease must store release metadata only in its own namespace")
		}
		if serviceAccountName := optionalString(spec, "serviceAccountName"); serviceAccountName != "" {
			return errors.New("isolated-chart HelmRelease must not select a reconciliation service account")
		}
		if _, hasKubeConfig := spec["kubeConfig"]; hasKubeConfig {
			return errors.New("isolated-chart HelmRelease must not target another cluster")
		}
		chartRef, ok := spec["chartRef"].(map[string]any)
		if !ok || fmt.Sprint(chartRef["kind"]) != "OCIRepository" || fmt.Sprint(chartRef["name"]) == "" {
			return errors.New("isolated-chart HelmRelease must reference an OCIRepository")
		}
		if chartNamespace := optionalString(chartRef, "namespace"); chartNamespace != "" && chartNamespace != identity.namespace {
			return errors.New("isolated-chart HelmRelease must reference an OCIRepository in its own namespace")
		}
		if containsAuthorizationKind(spec["postRenderers"]) ||
			containsEmbeddedAuthorizationTemplate(spec["postRenderers"], 0) {
			return errors.New("isolated-chart HelmRelease must not post-render authorization resources")
		}
		return nil

	case strings.HasPrefix(identity.apiVersion, "source.toolkit.fluxcd.io/") && identity.kind == "OCIRepository":
		url := fmt.Sprint(spec["url"])
		if !strings.HasPrefix(url, "oci://ghcr.io/devantler-tech/charts/") {
			return errors.New("isolated-chart OCIRepository must use the reviewed public chart registry")
		}
		ref, ok := spec["ref"].(map[string]any)
		if !ok || requireExactKeys(ref, "digest") != nil || !exactSHA256Digest.MatchString(fmt.Sprint(ref["digest"])) {
			return errors.New("isolated-chart OCIRepository must use exactly one immutable sha256 digest")
		}
		for _, key := range []string{"secretRef", "certSecretRef", "proxySecretRef", "serviceAccountName"} {
			if _, exists := spec[key]; exists {
				return fmt.Errorf("isolated-chart OCIRepository must not configure %s", key)
			}
		}
		return nil
	}

	return fmt.Errorf("isolated-chart authorization scope is unsupported for %s/%s", identity.apiVersion, identity.kind)
}

func isAuthorizationIsolated(document map[string]any, identity resourceIdentity) bool {
	return authorizationIsolationRequested(document) && validateAuthorizationIsolation(document, identity) == nil
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

// maxEmbeddedTextDepth bounds how many times a string leaf may be decoded into
// a further document. Real manifests nest once — a post-renderer patch string
// holding one YAML document — so the bound only guards against a pathological
// input; beyond it the declaration scan still applies, so deep nesting fails
// closed rather than escaping inspection.
const maxEmbeddedTextDepth = 4

// embeddedAuthorizationKindDeclaration matches a `kind:` line naming an RBAC
// kind. It backs the fail-closed scan for a string that does not parse, where
// there is no decoded `kind` key to test. It deliberately requires the `kind:`
// key rather than the bare word, so prose that merely mentions a role is not
// mistaken for a declaration.
var embeddedAuthorizationKindDeclaration = regexp.MustCompile(
	`(?m)^[\t ]*kind[\t ]*:[\t ]*["']?(ClusterRoleBinding|ClusterRole|RoleBinding|Role)["']?[\t ]*(#.*)?$`,
)

// textDeclaresAuthorizationKind inspects a string leaf. Flux stores a
// post-renderer patch as a string, and Kustomize matches a target-less
// strategic-merge patch on the GVK and name inside that string, so a string
// leaf can introduce an authorization object with no map for a kind selector to
// catch. Decode it and test the real `kind`; when it does not parse — or when
// nesting exceeds the bound — there is nothing left to decode, so fall back to
// the declaration scan and fail closed rather than pass for want of a decode.
func textDeclaresAuthorizationKind(text string, textDepth int) bool {
	if textDepth < maxEmbeddedTextDepth {
		if documents, err := decodeDocuments([]byte(text)); err == nil {
			for _, document := range documents {
				if containsAuthorizationKindAtDepth(document, textDepth) {
					return true
				}
			}
			return false
		}
	}
	return embeddedAuthorizationKindDeclaration.MatchString(text)
}

// containsAuthorizationKind finds protected kinds inside Kyverno match and
// target shapes, including Flux sources and controller package resources, and
// inside string leaves that carry an embedded document.
func containsAuthorizationKind(value any) bool {
	return containsAuthorizationKindAtDepth(value, 0)
}

func containsAuthorizationKindAtDepth(value any, textDepth int) bool {
	switch typedValue := value.(type) {
	case string:
		return textDeclaresAuthorizationKind(typedValue, textDepth+1)
	case []any:
		for _, item := range typedValue {
			if containsAuthorizationKindAtDepth(item, textDepth) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typedValue {
			if (key == "kind" || key == "kinds") && kindSelectorIncludesAuthorization(item) {
				return true
			}
			if containsAuthorizationKindAtDepth(item, textDepth) {
				return true
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
		(isFluxSourceResource(identity) || isControllerRBACEmitter(identity)) &&
			!isAuthorizationIsolated(document, identity) ||
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
		(isFluxSourceResource(identity) || isControllerRBACEmitter(identity)) &&
			!isAuthorizationIsolated(document, identity) {
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

// cloneStringAnyMap returns a shallow copy used to project one nested field
// without changing the decoded document used by the remaining checks.
func cloneStringAnyMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// normalizedPinnedContainerImage keeps the repository and explicit tag as
// reviewed policy while replacing only an immutable SHA-256 digest. Requiring
// a tag means floating, substituted, and digest-only shapes remain exact.
func normalizedPinnedContainerImage(image string) (string, bool) {
	delimiter := strings.LastIndex(image, "@")
	if delimiter <= 0 || !exactSHA256Digest.MatchString(image[delimiter+1:]) {
		return "", false
	}
	reference := image[:delimiter]
	lastSlash := strings.LastIndex(reference, "/")
	lastColon := strings.LastIndex(reference, ":")
	if lastColon <= lastSlash || lastColon == len(reference)-1 || strings.Contains(reference, "${") {
		return "", false
	}
	return reference + "@sha256:<exact-digest>", true
}

// normalizePinnedContainerImages recursively projects only map fields named
// image. Helm-rendered manifests remain validated and scanned by the required
// manifest job, while routine digest refreshes do not move this authorization
// fingerprint when the repository and tag are unchanged.
func normalizePinnedContainerImages(value any) any {
	switch typedValue := value.(type) {
	case map[string]any:
		projected := cloneStringAnyMap(typedValue)
		for key, item := range projected {
			if key == "image" {
				if image, ok := item.(string); ok {
					if normalized, pinned := normalizedPinnedContainerImage(image); pinned {
						projected[key] = normalized
						continue
					}
				}
			}
			projected[key] = normalizePinnedContainerImages(item)
		}
		return projected
	case []any:
		projected := make([]any, len(typedValue))
		for index, item := range typedValue {
			projected[index] = normalizePinnedContainerImages(item)
		}
		return projected
	default:
		return value
	}
}

// authorizationSurfaceDocument normalizes immutable Helm dependency pins and
// exact container image digests beneath Helm values,
// strict signer subsets handled by the required approved-revisions guard, and
// the uninterpretable ciphertext of SOPS-encrypted substitution sources.
// Identity, source, values, post-renderers, substitutions and policy stay exact.
// The required manifest job separately Helm-renders and security-scans the
// selected chart version, so a routine Renovate pin does not require a manual
// refresh of an otherwise unchanged authorization fingerprint.
func authorizationSurfaceDocument(
	identity resourceIdentity,
	document map[string]any,
) map[string]any {
	if identity.kind == "OCIRepository" {
		return publishMatcherSurfaceDocument(identity, document)
	}
	if identity.kind == "Secret" {
		return sopsSubstitutionSourceSurfaceDocument(identity, document)
	}
	if !strings.HasPrefix(identity.apiVersion, "helm.toolkit.fluxcd.io/") || identity.kind != "HelmRelease" {
		return document
	}
	spec, ok := document["spec"].(map[string]any)
	if !ok {
		return document
	}
	chart, ok := spec["chart"].(map[string]any)
	if !ok {
		return document
	}
	chartSpec, ok := chart["spec"].(map[string]any)
	if !ok {
		return document
	}
	version, ok := chartSpec["version"].(string)
	if !ok || !exactPinnedHelmChartVersion.MatchString(version) {
		return document
	}

	projected := cloneStringAnyMap(document)
	projectedSpec := cloneStringAnyMap(spec)
	projectedChart := cloneStringAnyMap(chart)
	projectedChartSpec := cloneStringAnyMap(chartSpec)
	projectedChartSpec["version"] = "<exact-semver>"
	projectedChart["spec"] = projectedChartSpec
	projectedSpec["chart"] = projectedChart
	if values, ok := spec["values"].(map[string]any); ok {
		projectedSpec["values"] = normalizePinnedContainerImages(values)
	}
	projected["spec"] = projectedSpec
	return projected
}

// authorizationSurfaceEntry serializes one selected object with its complete
// identity so the aggregate hash preserves additions, removals, and duplicates.
func authorizationSurfaceEntry(identity resourceIdentity, document map[string]any) (string, error) {
	canonical, err := json.Marshal(authorizationSurfaceDocument(identity, document))
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

// awsIdentityGrantError names an unapproved RBAC binding whose subjects reach the aws/aws
// service account. It is deliberately distinct from the aggregate surface mismatch, which any
// added document triggers, so a test can prove the identity itself was detected (#2806).
const awsIdentityGrantError = "unapproved binding grants the aws/aws service account identity"

// awsIdentityGroups are the RBAC groups the aws/aws service account belongs to. A binding to
// any of them grants that identity whatever else it grants.
var awsIdentityGroups = map[string]bool{
	"system:serviceaccounts:aws": true,
	"system:serviceaccounts":     true,
	"system:authenticated":       true,
}

// awsIdentityGrantProblem reports a RoleBinding or ClusterRoleBinding whose subjects reach the
// aws/aws service account, unless the binding is byte-for-byte one of the pinned, approved
// resources in expectedRenderedHashes. The exemption keys on CONTENT, not name: a binding that
// only borrows an approved identity (a modified or duplicated aws-managed-resources) still has
// its grant reported here, rather than surfacing only as a fingerprint mismatch.
func awsIdentityGrantProblem(document map[string]any, identity resourceIdentity) error {
	if identity.apiVersion != "rbac.authorization.k8s.io/v1" ||
		(identity.kind != "RoleBinding" && identity.kind != "ClusterRoleBinding") {
		return nil
	}
	if expected, pinned := expectedRenderedHashes[identity]; pinned {
		if actual, hashErr := canonicalFingerprint(document); hashErr == nil && actual == expected {
			return nil
		}
	}
	subjects, _ := document["subjects"].([]any)
	for _, rawSubject := range subjects {
		subject, ok := rawSubject.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := subject["kind"].(string)
		name, _ := subject["name"].(string)
		namespace, _ := subject["namespace"].(string)
		reaches := false
		switch kind {
		case "ServiceAccount":
			// A RoleBinding subject without a namespace resolves to the binding's own namespace.
			if namespace == "" && identity.kind == "RoleBinding" {
				namespace = identity.namespace
			}
			reaches = name == "aws" && namespace == "aws"
		case "User":
			reaches = name == "system:serviceaccount:aws:aws"
		case "Group":
			reaches = awsIdentityGroups[name]
		}
		if reaches {
			return fmt.Errorf("%s: %+v subject %s %q", awsIdentityGrantError, identity, kind, name)
		}
	}
	return nil
}

// validateRendered requires the complete selected authorization surface to
// match the approved ledger exactly while preserving precise core-object
// diagnostics.
func validateRendered(rendered []byte) error {
	surfaceEntries, problems, substitutionProblems, err := evaluateRenderedSurface(rendered)
	if err != nil {
		return err
	}
	approved, ledgerErr := parseSurfaceLedger(approvedSurfaceLedger)
	if ledgerErr != nil {
		problems = append(problems, ledgerErr)
		problems = append(problems, substitutionProblems...)
	} else if delta := describeLedgerDelta(approved, surfaceLedger(surfaceEntries)); len(delta) > 0 {
		problems = append(problems, &surfaceMismatchError{delta: delta})
		problems = append(problems, substitutionProblems...)
	}
	return errors.Join(problems...)
}

// surfaceMismatchError reports a render that differs from the approved ledger
// and names every line that has to change, so approving a reviewed change is a
// matter of copying the "+" lines in and dropping the "-" lines.
type surfaceMismatchError struct {
	delta []string
}

// Error names the moved lines under the message the gate has always reported.
func (e *surfaceMismatchError) Error() string {
	var message strings.Builder
	fmt.Fprintf(&message, "unapproved rendered authorization surface: %d ledger line(s) differ from %s "+
		"(\"-\" approved but not rendered, \"+\" rendered but not approved):", len(e.delta), surfaceLedgerFile)
	for _, line := range e.delta {
		message.WriteString("\n  " + line)
	}
	return message.String()
}

// evaluateRenderedSurface selects the authorization surface of one render. It
// returns the sorted surface entries with the per-object problems it found, and
// leaves the comparison with the approved ledger to its caller.
func evaluateRenderedSurface(rendered []byte) ([]string, []error, []error, error) {
	documents, err := decodeDocuments(rendered)
	if err != nil {
		return nil, nil, nil, err
	}
	roleIdentities := authorizationRoleIdentities(documents)
	substitutionSourceIdentities := authorizationSubstitutionSourceIdentities(documents)
	templateInstanceTypes := authorizationTemplateInstanceTypes(documents)
	seen := make(map[resourceIdentity]bool, len(expectedRenderedHashes))
	surfaceEntries := make([]string, 0, len(expectedRenderedHashes))
	problems := make([]error, 0)
	if keyErr := validateUnifiSigningKey(documents); keyErr != nil {
		problems = append(problems, keyErr)
	}
	substitutionProblems := make([]error, 0)
	for _, document := range documents {
		identity := identityOf(document)
		if grantErr := awsIdentityGrantProblem(document, identity); grantErr != nil {
			problems = append(problems, grantErr)
		}
		if isolationErr := validateAuthorizationIsolation(document, identity); isolationErr != nil {
			problems = append(problems, fmt.Errorf("invalid authorization isolation for %+v: %w", identity, isolationErr))
		}
		if sourceErr := validatePinnedUnifiSource(document, identity); sourceErr != nil {
			problems = append(problems, sourceErr)
		}
		if exemptionErr := validateUnifiPruneExemption(document, identity); exemptionErr != nil {
			problems = append(problems, exemptionErr)
		}
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
	return surfaceEntries, problems, substitutionProblems, nil
}

// surfaceEntryKey is the apiVersion|kind|namespace|name identity of an entry.
// An entry without all four identity fields shares one "malformed entry" key.
func surfaceEntryKey(entry string) string {
	fields := strings.SplitN(entry, "\x00", 5)
	if len(fields) < 5 {
		return "malformed entry"
	}
	return strings.Join(fields[:4], "|")
}

// surfaceLedgerFile names the approved ledger in every message that asks for it
// to be edited.
const surfaceLedgerFile = "scripts/validate-eks-ci-role-policy/approved-surface.txt"

// surfaceLedgerLinePattern is one approved line: the four identity fields, of
// which only the namespace may be empty, and the entry's SHA-256 digest.
var surfaceLedgerLinePattern = regexp.MustCompile(`^[^|\s]+\|[^|\s]+\|[^|\s]*\|[^|\s]+ [0-9a-f]{64}$`)

// surfaceLedgerLine is the approved-ledger line for one surface entry. The
// digest covers the whole entry, identity included, so equal ledgers mean equal
// entry lists.
func surfaceLedgerLine(entry string) string {
	return surfaceEntryKey(entry) + " " + fingerprint([]byte(entry))
}

// surfaceLedger returns the sorted ledger lines of a rendered surface.
func surfaceLedger(entries []string) []string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, surfaceLedgerLine(entry))
	}
	sort.Strings(lines)
	return lines
}

// parseSurfaceLedger reads the approved ledger. It refuses a malformed or
// unsorted file rather than guessing, so a bad merge resolution fails loudly
// and the file stays in the one order that keeps unrelated approvals apart.
//
// Comments are allowed only before the first entry, and entries are separated
// by exactly one blank line. Git refuses to merge edits to adjacent lines, so
// without the separator two approvals of neighbouring documents would conflict;
// with it, each edit is surrounded by unchanged lines and merges cleanly.
func parseSurfaceLedger(text string) ([]string, error) {
	lines := make([]string, 0)
	raw := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for number, line := range raw {
		if len(lines) == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if line == "" {
			if raw[number-1] == "" || number == len(raw)-1 {
				return nil, fmt.Errorf("%s line %d: separate entries with exactly one blank line",
					surfaceLedgerFile, number+1)
			}
			continue
		}
		if len(lines) > 0 && raw[number-1] != "" {
			return nil, fmt.Errorf("%s line %d: separate entries with exactly one blank line",
				surfaceLedgerFile, number+1)
		}
		if !surfaceLedgerLinePattern.MatchString(line) {
			return nil, fmt.Errorf("%s line %d is not `<apiVersion>|<kind>|<namespace>|<name> <sha256>`: %q",
				surfaceLedgerFile, number+1, line)
		}
		if count := len(lines); count > 0 && line < lines[count-1] {
			return nil, fmt.Errorf("%s line %d is out of order: keep the file sorted byte by byte", surfaceLedgerFile, number+1)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s approves no authorization surface", surfaceLedgerFile)
	}
	return lines, nil
}

// describeLedgerDelta lists, per identity, the approved lines the render no
// longer produces ("-") and the rendered lines nobody approved ("+"). Lines
// compare as a multiset, so a duplicated document is a delta too. It returns
// nothing only when the two ledgers are identical.
func describeLedgerDelta(approved []string, rendered []string) []string {
	remaining := make(map[string]int, len(approved))
	for _, line := range approved {
		remaining[line]++
	}
	added := make([]string, 0)
	for _, line := range rendered {
		if remaining[line] > 0 {
			remaining[line]--
			continue
		}
		added = append(added, line)
	}
	removed := make([]string, 0)
	for _, line := range approved {
		if remaining[line] > 0 {
			remaining[line]--
			removed = append(removed, line)
		}
	}
	delta := make([]string, 0, len(added)+len(removed))
	for _, line := range removed {
		delta = append(delta, "- "+line)
	}
	for _, line := range added {
		delta = append(delta, "+ "+line)
	}
	// Group each identity's "-" and "+" lines together, "-" first.
	sort.SliceStable(delta, func(first, second int) bool {
		return strings.SplitN(delta[first][2:], " ", 2)[0] < strings.SplitN(delta[second][2:], " ", 2)[0]
	})
	return delta
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
