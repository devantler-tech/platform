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

// expectedRenderedSurfaceSHA is the aggregate fingerprint of the whole selected
// authorization surface.
//
// It sits outside the const block above so this rationale can travel with it
// without re-aligning seven unrelated security constants.
//
// The approved surface includes the encrypted flux-system/variables-cluster
// substitution source and the staged Cilium homogeneous-device activation.
//
// Measured by comparing base 4673fe2d with manifest head d0f130a0 before
// approving this value: all five production render trees contain 1,019 documents
// on both sides, with membership IDENTICAL by
// apiVersion|kind|namespace|name set difference. The 64 Role /
// ClusterRole / RoleBinding / ClusterRoleBinding / ServiceAccount documents are
// byte-identical. Exactly ONE rendered entry moves:
//
//	helm.toolkit.fluxcd.io/v2  HelmRelease  kube-system/cilium
//
// Its only rendered delta changes authentication.enabled and
// authentication.mutual.spire.enabled from true to false. No scoped
// required-authentication policy consumes SPIRE, so disabling the unused
// integration removes continuous delegated-identity errors and grants no
// identity or permission.
//
// Measured against main 94714d94 before approving this value: 511 documents on
// both sides, with membership IDENTICAL -- zero added, removed, or renamed by
// set difference over every apiVersion|kind|namespace|name identity. Exactly
// two rendered entries move:
//
//	autoscaling.k8s.io/v1       VerticalPodAutoscaler  kyverno/kyverno-admission-controller
//	helm.toolkit.fluxcd.io/v2  HelmRelease           kyverno/kyverno
//
// The VPA delta narrows controlledValues from RequestsAndLimits to RequestsOnly
// and its memory ceiling from 6Gi to the authored 1Gi container limit. The
// HelmRelease delta moves that same request/limit block from the ignored
// admissionController.resources key to the chart-consumed
// admissionController.container.resources key. Both are resource-safety
// constraints; neither can grant an identity or permission.
//
// The rendered surface carries 64 Role / ClusterRole / RoleBinding /
// ClusterRoleBinding / ServiceAccount documents on BOTH sides, and their
// canonical byte stream is identical. Every `aws`-bearing line is likewise
// byte-identical, so nothing granted to the aws/aws service account moved.
//
// Measured against main 4cfe7d9f while fixing the Renovate KSail bump: the PR
// changes exactly the ksail-operator chart's pinned SemVer (7.176.4 -> 7.178.2)
// inside the authorization surface. This value migrates the projection so an
// exact Helm chart SemVer is represented by one sentinel; both versions produce
// this fingerprint. Every other HelmRelease field remains byte-exact after
// canonicalization, including chart/source identity, values, post-renderers,
// substitutions, and reconciliation policy. Missing, ranged, wildcarded, or
// substituted versions are not normalized. The required manifest job separately
// Helm-renders and security-scans the selected chart artifact before merge.
//
// Measured against main 9b1990de before approving this value: 510 documents
// on both sides, with membership IDENTICAL — zero added, zero removed, zero
// renamed (proven by set difference in both directions over the complete
// apiVersion|kind|namespace|name identity). Exactly ONE entry's content moved:
//
//	helm.toolkit.fluxcd.io/v2  HelmRelease  kube-system/cilium
//
// Its rendered delta adds only `nodePort.addresses: [10.0.0.0/16]`, narrowing
// kube-proxy-replacement NodePort listeners from every selected device address
// to the private node CIDR. The surface carries 64 Role / ClusterRole /
// RoleBinding / ClusterRoleBinding / ServiceAccount documents on BOTH sides;
// none moved, and no permission or identity grant changed.
//
// Measured against main 6e011890 before approving the previous value: 515 documents on
// both sides, membership IDENTICAL — proven by set difference in BOTH
// directions over apiVersion|kind|namespace|name across all five rendered
// trees, not by count alone, which cannot see a rename. Exactly ONE entry's
// content moved:
//
//	helm.toolkit.fluxcd.io/v2  HelmRelease  headlamp/headlamp
//
// Its rendered delta is a single line — `hostUsers: false` — activating the
// user-namespace pilot gated since #2650. That field places the pod in its own
// user namespace; it constrains the workload and grants nothing.
//
// No grant-bearing object moved: 64 Role / ClusterRole / RoleBinding /
// ClusterRoleBinding / ServiceAccount documents on BOTH sides, none of them in
// the moved set. All 116 `aws`-bearing lines are byte-identical across the two
// trees, so nothing granted to the aws/aws service account this validator
// exists to protect is touched.
//
// Measured against main 875f5dc3 before approving THIS value: the rendered
// surface moves from 514 -> 519 documents, purely additive. Membership was
// proven by set difference in BOTH directions over apiVersion|kind|namespace|name
// (not by count, which cannot see a rename): zero removed, zero renamed, and the
// five additions are exactly the degraded-CNPG-cluster alert —
//
//	v1                          ServiceAccount      observability/cnpg-degraded-alert
//	v1                          Secret              observability/cnpg-degraded-alert-webhook
//	rbac.authorization.k8s.io/v1 ClusterRole        cnpg-degraded-alert
//	rbac.authorization.k8s.io/v1 ClusterRoleBinding cnpg-degraded-alert
//	batch/v1                    CronJob             observability/cnpg-degraded-alert
//
// Grant-bearing objects DID move here, additively: 64 -> 67 Role / ClusterRole /
// RoleBinding / ClusterRoleBinding / ServiceAccount documents, being the three
// above. That is the same shape as the OpenCost usage-scraper approval below —
// one dedicated ServiceAccount, one narrow ClusterRole, one binding between
// exactly those two identities. The ClusterRole is list-only on a single resource
// type (`clusters.postgresql.cnpg.io`); it is cluster-scoped only because the
// check must see databases in every namespace that runs one.
//
// Exactly ONE existing entry's content moved:
//
//	kubescape.io/v1beta1  ClusterSecurityException  gitops-managed-cronjobs
//
// and its rendered delta is one prose sentence in `reason`, naming the new
// CronJob in the enumeration that field already carries. Its `match` scope is
// unchanged, so the exception grants nothing new.
//
// Every `aws`-bearing line in the rendered surface is byte-identical across the
// two trees (116 on both sides), so nothing granted to the aws/aws service
// account this validator exists to protect is touched.
//
// Measured against main fa041449 before approving the previous value: the
// production infrastructure overlay moves from 204 -> 210 documents, with
// exactly six additive OpenCost usage-scraper resources and no existing
// document modified or removed. The authorization delta is one dedicated
// ServiceAccount, one
// ClusterRole limited to get/list/watch on nodes plus get on nodes/metrics, and
// one binding between exactly those identities. The ConfigMap, Deployment, and
// scraper-scoped CiliumNetworkPolicy carry the bounded scrape/remote-write path
// but grant no Kubernetes authorization. The privileged nodes/proxy resource is
// absent. No aws/aws permission or existing grant-bearing object moved.
//
// Measured against main c5e2f307 before approving the previous value: 509
// documents on both sides, with membership IDENTICAL — zero added, zero removed, zero
// renamed (proven by set difference in BOTH directions over
// apiVersion|kind|namespace|name, not by count alone, which cannot see a
// rename). Exactly ONE entry's content moved:
//
//	helm.toolkit.fluxcd.io/v2  HelmRelease  longhorn-system/longhorn
//
// Its rendered delta is the `postRenderers` block added by this change, which
// pins longhorn-ui's non-root identity (runAsNonRoot/runAsUser/runAsGroup,
// seccomp RuntimeDefault, drop ALL, no privilege escalation). That block
// constrains the workload; it grants nothing.
//
// No grant-bearing object moved: the surface carries 61 Role / ClusterRole /
// RoleBinding / ClusterRoleBinding / ServiceAccount documents on BOTH sides, and
// none of them is in the moved set above. Every `aws`-bearing line in the
// rendered surface is byte-identical across the two trees, so nothing granted to
// the aws/aws service account this validator exists to protect is touched.
//
// (The value before this one covered the tenant `ResourceGraphDefinition`,
// measured against 1b204ded: 509 documents on both sides, membership identical,
// exactly one entry moved, and its rendered delta was a single line — the
// cosign `subject:` matcher narrows from
// `(reusable-workflows|actions)/…@.+` to `actions/…@([0-9a-f]{40}|refs/tags/v.+)`.
// That drops the archived `reusable-workflows` repo as an accepted signer and
// stops a floating ref (e.g. `@refs/heads/main`) from satisfying the rule. The
// result is byte-identical to the three live `publish-app` OCIRepository trust
// rules (wedding-app, ascoachingogvaner, doggy-countdown), so the template every
// tenant is generated from now carries the same rule its tenants do. It is
// strictly a tightening: every subject the new matcher accepts, the old one
// already accepted.
//
// moved line was the cosign subject, NOT one of the RGD's grant templates — the
// ServiceAccount and `tenant-edit` RoleBinding it expands to were untouched, no
// grant-bearing object moved, and the RGD being individually pinned meant its
// per-resource fingerprint moved with it and was re-approved from the same
// measurement. The value before that covered onboarding the `doggy-countdown`
// tenant, measured against e77fdb9c: a purely additive 503 -> 509 documents, one
// tenant skeleton, no existing entry modified. The one before that covered the
// cosign matcher tightening on the then-four live trust rules: 503 documents on
// both sides, four changed `subject:` lines, no grant-bearing object moved.)
//
// This value covers narrowing the github-config tenant Role from a resources
// wildcard to the managed-resource kinds its ManagedResourceActivationPolicy
// activates. Measured against main 72fe7919: 515 documents on both sides, with
// membership IDENTICAL — set difference in BOTH directions over
// apiVersion|kind|namespace|name returned zero, so nothing was added, removed
// or renamed. Exactly ONE entry's content moved:
//
//	rbac.authorization.k8s.io/v1  Role  github-config/github-config-managed-resources
//
// Its rendered delta replaces `resources: ['*']` on five
// `*.github.m.upbound.io` groups with the ten activated kinds, and adds
// `providerconfigs` for the ProviderConfig the tenant applies. It is strictly a
// tightening: every resource the new rules admit, the wildcard already admitted.
// The surface carries 133 Role / ClusterRole / RoleBinding / ClusterRoleBinding
// / ServiceAccount documents on BOTH sides, and every `aws`-bearing line is
// byte-identical across the two trees, so nothing granted to the aws/aws
// service account this validator exists to protect is touched.
//
// NOTE for whoever re-approves this next: an opaque ciphertext in the surface
// moves on ANY re-encryption — this value also absorbed a SOPS version bump
// (3.13.2 -> 3.13.3) — so a routine secret rotation reds this gate with no
// authorization change at all. Do NOT treat a moved hash as self-evidently
// benign: re-run the per-entry membership measurement above before re-approving.
// A Go toolchain is only needed to recompute the hash; MEMBERSHIP is a plain
// kubectl render diff — render k8s/providers/hetzner/{apps,infrastructure,
// infrastructure/controllers} plus k8s/clusters/prod/{bootstrap,} for both
// trees and diff them.
//
// Measured while releasing the Cilium homogeneous-device rollout gate, by
// rendering all five roots from both trees and diffing them. Four of the five
// roots — apps, infrastructure, bootstrap, prod — are byte-identical.
// Membership is unchanged: zero documents added, removed, or renamed. Exactly
// FOUR lines move in the whole production render, all of them inside the same
// document:
//
//	helm.toolkit.fluxcd.io/v2  HelmRelease  kube-system/cilium
//	  spec.upgrade.disableWait: true            (removed)
//	  spec.values.updateStrategy.type: OnDelete (removed)
//	  spec.values.updateStrategy.rollingUpdate  (removed)
//
// Both were temporary overrides carried only while the widened device set was
// being introduced one node at a time. `updateStrategy` selects how the Cilium
// DaemonSet replaces pods and `upgrade.disableWait` selects whether Helm waits
// for them; neither reaches an identity, binding, policy document, or service
// account, and neither is an `aws`-bearing line. Every Role / ClusterRole /
// RoleBinding / ClusterRoleBinding / ServiceAccount document is byte-identical
// — trivially so, since the four differing lines are the entire delta across
// the surface and all four belong to one HelmRelease's rollout mechanics.
//
// The fingerprint itself was read from the required job's own output on the
// approved renderer, because the local toolchain is refused as unapproved.
//
// This value covers activating the Crossplane sync-state exporter (#2986) — the
// first change to reference that component, so its three objects enter the
// rendered surface for the first time. Measured by rendering the production
// roots with and without the single `components:` reference: grant-bearing
// documents go 67 -> 70, the set difference in the removal direction is EMPTY,
// and the three additions are the component's own:
//
//	ServiceAccount       observability/crossplane-sync-exporter
//	ClusterRole          crossplane-sync-exporter
//	ClusterRoleBinding   crossplane-sync-exporter
//
// Nothing else is added, removed or renamed, and no existing grant changes.
//
// The ClusterRole is get/list/watch on `repo.github.m.upbound.io/repositories`
// and nothing else — one API group, one resource, read-only. That is narrower
// than the alternative the issue originally specified (binding the aggregated
// `crossplane-view`, which carries 49 rules), which is why the deviation was
// taken; `scripts/tests/test-crossplane-sync-exporter.sh` pins the read-only
// verb set and rejects wildcard access so it cannot widen unobserved.
//
// Nothing granted to the aws/aws service account this validator exists to
// protect is touched: the additions are in `observability`, and the exporter
// cannot read any AWS-bearing resource.
//
// This value additionally covers granting that same exporter the CRD read its
// discovery path requires (#3068). The component shipped unable to read
// anything: kube-state-metrics resolves a CustomResourceStateMetrics
// `groupVersionKind` through the CRD API before it can build a store for it, so
// the repositories rule above is necessary but not sufficient, and without the
// CRD read the reflector retry-loops on a forbidden list while the workload
// reports itself Ready.
//
// Measured by rendering the production infrastructure root on the base commit
// and on this change: the diff is EIGHT added lines and nothing else. No
// document is removed, renamed, or otherwise altered, no resource enters or
// leaves the surface, and the set difference in the removal direction is EMPTY.
// The eight lines are one rule on the existing `crossplane-sync-exporter`
// ClusterRole:
//
//	apiGroups: ["apiextensions.k8s.io"]
//	resources: ["customresourcedefinitions"]
//	verbs:     ["get", "list", "watch"]
//
// The grant reads CRD *definitions*, which carry schemas, not the managed
// resources themselves — so the scoping rationale above is preserved intact:
// managed-resource access, where provider configuration actually lives, stays
// pinned to `repo.github.m.upbound.io/repositories`. It is read-only, and
// `scripts/tests/test-crossplane-sync-exporter.sh` now pins this rule too, so
// the discovery half can no longer go missing unobserved — which is exactly how
// it went missing the first time, since that contract asserted the
// managed-resource half alone.
//
// `resourceNames` cannot narrow it further: RBAC honours that field only for
// get/update/delete/patch, never for list or watch, and the discovery path
// lists.
//
// Nothing granted to the aws/aws service account is touched by this change
// either. The rule is cluster-scoped because CRD definitions are, but it
// confers no access to any AWS-bearing resource, identity, binding, policy
// document or service account.
//
// The fingerprint was produced by two independent renderers that agree
// exactly — the required CI job on the approved toolchain, and a local render —
// which is what rules out a renderer-version artifact in the value.
//
// This value additionally covers conditioning `kms:CreateGrant` on
// `kms:GrantIsForAWSResource=true` in the eks-ci-smoke permissions boundary
// (#2704). Measured by restoring main's copy of the one changed manifest and
// re-rendering all five roots from both trees: 22933 -> 22943 lines, and the
// ENTIRE delta across the production surface is
//
//	GONE   "kms:CreateGrant",   (leaving the unconditioned action list)
//	NEW    "Sid": "KmsGrantsForAwsResourcesOnly",
//	NEW    "Action": "kms:CreateGrant", "Resource": "*",
//	NEW    "Condition": {"Bool": {"kms:GrantIsForAWSResource": "true"}}
//
// No `kind:` or `name:` line moves, so membership is identical — nothing was
// added, removed or renamed — and every other Role / ClusterRole / RoleBinding
// / ClusterRoleBinding / ServiceAccount document is byte-identical. It is
// strictly a tightening: the action was previously allowed unconditionally and
// is now allowed only for AWS-service-managed resources, so every grant the new
// form permits the old one already permitted. Only ONE per-resource fingerprint
// moves with it — the boundary Policy's own — which corroborates that the edit
// did not leak into any other document.
//
// One trap worth recording, because it looks exactly like a renderer-version
// artifact and is not: a `pull_request` CI job renders the PR's MERGE commit,
// while a local `go test` on the branch renders the branch against whatever
// base it was cut from. When main moves under a branch the two therefore
// disagree on the AGGREGATE surface value while still agreeing on every
// per-resource fingerprint, since the moved documents belong to main's change
// rather than the branch's. Compare like with like before concluding the
// toolchain is at fault.
//
// Measured against main 2f92d8ef before approving this value: the complete
// rendered authorization diff changes exactly one object, ClusterRole
// `cilium-tenant-edit`. Surface membership is unchanged. The only rendered
// change removes its built-in `aggregate-to-edit` label, so ordinary `edit`
// bindings no longer inherit Cilium policy access. The tenant-specific
// aggregation label and all rule verbs remain byte-identical to main: Flux can
// still server-side apply and prune tenant policies through `tenant-edit`.
// This strictly narrows which aggregate role inherits the grant without
// breaking that workflow. Platform#3150 separately tracks admission constraints
// for permissive or platform-owned tenant policy mutations. The committed-tree
// validation reported no per-resource mismatch; only this aggregate fingerprint
// moved.
//
// Measured by comparing base 0a97d6c7 with repair head 966ca01c before
// approving this value: the complete five-layer render diff changes exactly
// four selected entries. The mutateExisting ClusterPolicy is narrowed from
// arbitrary matching Deployments to umami/umami-umami and its fixed
// umami-umami-primary target. The cluster-wide aggregated ClusterRole that
// granted get/list/watch/update/patch on every Deployment is removed. In its
// place, one Role in umami grants only get/update/patch on the single
// umami-umami-primary Deployment, and one RoleBinding grants it only to
// Kyverno's background-controller ServiceAccount. No existing rendered entry
// changes, including the Umami Namespace, whose identical rendered form merely
// moves between two already-scanned ownership layers. The validator reported
// no per-resource mismatch; only this aggregate fingerprint moved.
//
// This value additionally covers the UniFi source containment repair in #2707,
// measured after rebasing the PR onto exact main 57ca1dbe. Four of the five
// authorization roots are byte-identical across main and this change:
// infrastructure, infrastructure/controllers, prod/bootstrap, and prod. The
// complete apps render has identical membership and exactly THREE changed
// fields across three existing documents:
//
//	rbac.authorization.k8s.io/v1    Role           unifi/unifi-managed-resources
//	  verbs: delete                                        (removed)
//	kustomize.toolkit.fluxcd.io/v1  Kustomization  unifi/unifi
//	  prune: true -> false
//	source.toolkit.fluxcd.io/v1      GitRepository  unifi/unifi
//	  ref.branch: main -> one full 40-hex ref.commit pin
//
// The first two changes remove deletion paths. The third replaces a moving
// branch with one full immutable commit from the expected repository, so a
// source-repository compromise cannot alter resources that retain patch/update
// authority without a separately reviewed Platform change. The semantic
// validatePinnedUnifiSource control and its negative tests reject branches,
// tags/mixed selectors, abbreviated hashes, substitutions, and alternate URLs
// before the aggregate hash is considered. No identity, binding, resource kind,
// or remaining verb moved.
const expectedRenderedSurfaceSHA = "1c4c1986a7b891b184d70a19bec6a85f33c0ea4c0ac174b26501000e38582cd1"

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
	return nil
}

// expectedRenderedHashes preserves object-specific diagnostics for the core
// EKS CI identities while the aggregate surface hash pins every selected
// source, controller, binding, and indirect authorization object.
var expectedRenderedHashes = map[resourceIdentity]string{
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Role", namespace: "aws", name: "eks-ci"}:                                        "0967890d16316a8cfcb1cca8a52085c6989c42000fafbbd0ada6323d4e15c97c",
	{apiVersion: "iam.aws.m.upbound.io/v1beta1", kind: "Policy", namespace: "aws", name: "eks-ci-smoke-boundary"}:                       "6f14b5243c945d0d2230821733ea12096d6e92ab155a35482b20a6080c03c037",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "Role", namespace: "aws", name: "aws-managed-resources"}:                         "ff4c3264c519b1b4a7ec9b5145412f39ea2ba7b6163d8dc50fb029b1460edcda",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "aws", name: "aws-managed-resources"}:                  "d846c8d9810dd7c0cba33612d2de63183403ccb07c4d5a5c90d0563a444cd714",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "RoleBinding", namespace: "crossview", name: "crossview-portforward"}:            "78992d9727763fdcf1bda05969fdc881e6d0e54cc72efc07555304b47d25bc3a",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: "kro-tenant-rgd"}:                                           "4447f41c03e8297fafdabcadf4fdd8ca3260f2c84264c531b2179cb7df2c1556",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole", name: "opencost-usage-scraper"}:                                   "3cb22a5a2d178e9cc93ebc3995d936d124800c441785dc23d780281746569937",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-cluster-reader"}:                               "7d896404f02d6418c289065d73f9ad79345217d76c8d89eadca2c06e6066b487",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "oidc-view"}:                                         "4d07ba3a995cfc139351b4227739efeba9348777f7fe47ac69b87d08e70bd45f",
	{apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRoleBinding", name: "opencost-usage-scraper"}:                            "4b28e1da280a7940a1cb4d538bc31ede1b5d272c17189a81afeae48acbb8b7a0",
	{apiVersion: "kro.run/v1alpha1", kind: "ResourceGraphDefinition", name: "tenant.kro.run"}:                                           "072e4478cdad39c0a7d9f5119cad63d4c56a9fc96ba88d657fef97f6b91bae31",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "ascoachingogvaner", name: "ascoachingogvaner"}:    "89ea0484e37b691594b7a72be2ca2de285697818bf88a5b37b4fa8a9161c54fa",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "aws", name: "aws"}:                                "7bde9c682a81b752bdf9d2b14ce69ca1690008a39f2562d4887f8200447dea71",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "apps"}:                       "a0b12b336d39709cb2f491662a3c8dd98269485b6a33935101e0bf9f03ec8925",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "bootstrap"}:                  "7f674a1762f298330c7c9e4d9d4e8bf46108b10727e02a25ca5096d7913cc0a7",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure"}:             "d1bc403b6458bd22cf967bd570e24718341cbd584f58e7f0069aaffe1e187945",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "flux-system", name: "infrastructure-controllers"}: "9d9b62d3221442d6355d16a34d31c198619fb3b3728df960fd67222a531ece7b",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "github-config", name: "github-config"}:            "8e9f72b0f4f982d050aff0b97d246c68b538cbc397cdd45d031c95cfae981e7c",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "unifi", name: "unifi"}:                            "33a579299700de2467631854bac4982d3e14caa3bad8cbcd2613ac180b30af32",
	{apiVersion: "kustomize.toolkit.fluxcd.io/v1", kind: "Kustomization", namespace: "wedding-app", name: "wedding-app"}:                "8af27d4845565c57b9ebc618f186669f18ada89e070cf4e6514924717a2532f8",
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

// cloneStringAnyMap returns a shallow copy used to project one nested field
// without changing the decoded document used by the remaining checks.
func cloneStringAnyMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// authorizationSurfaceDocument removes only an immutable Helm dependency pin
// from the approval projection. HelmRelease identity, chart/source identity,
// values, post-renderers, substitutions, and reconciliation policy stay exact.
// The required manifest job separately Helm-renders and security-scans the
// selected chart version, so a routine Renovate pin does not require a manual
// refresh of an otherwise unchanged authorization fingerprint.
func authorizationSurfaceDocument(
	identity resourceIdentity,
	document map[string]any,
) map[string]any {
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
		if sourceErr := validatePinnedUnifiSource(document, identity); sourceErr != nil {
			problems = append(problems, sourceErr)
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
