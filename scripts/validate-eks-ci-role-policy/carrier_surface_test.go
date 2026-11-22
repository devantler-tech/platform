package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mutateRenderedDocument re-encodes a render with the one document named by
// key (`<apiVersion>|<kind>|<namespace>|<name>`) changed by mutate.
func mutateRenderedDocument(t *testing.T, rendered []byte, key string, mutate func(map[string]any)) []byte {
	t.Helper()
	documents, err := decodeDocuments(rendered)
	if err != nil {
		t.Fatal(err)
	}
	var changed bytes.Buffer
	encoder := yaml.NewEncoder(&changed)
	mutations := 0
	for _, document := range documents {
		identity := identityOf(document)
		if strings.Join([]string{identity.apiVersion, identity.kind, identity.namespace, identity.name}, "|") == key {
			mutate(document)
			mutations++
		}
		if err := encoder.Encode(document); err != nil {
			t.Fatal(err)
		}
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if mutations != 1 {
		t.Fatalf("render holds %d documents named %s, want 1", mutations, key)
	}
	return changed.Bytes()
}

// carrierFixtures only deliver other objects. Each joined the approved ledger
// whole until platform#6270, so every chart, tag or package move needed an
// approval; the RED cases they came from are now these GREEN ones.
var carrierFixtures = []struct{ name, manifest string }{
	{name: "Flux root handoff", manifest: `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./external
  sourceRef:
    kind: OCIRepository
    name: unreviewed
`},
	{name: "tenant Flux handoff in the AWS namespace", manifest: `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: aws-shadow
  namespace: aws
`},
	{name: "Flux source object", manifest: `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: aws
  namespace: aws
spec:
  interval: 5m
  url: oci://example.invalid/unreviewed
`},
	{name: "Helm controller RBAC emitter", manifest: `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: controller
  namespace: controllers
spec:
  chart:
    spec:
      chart: controller
      version: ${controller_version}
      sourceRef:
        kind: HelmRepository
        name: controller
  values:
    rbac:
      create: true
`},
	{name: "Crossplane package RBAC emitter", manifest: `apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-example
spec:
  package: ghcr.io/example/provider:v1.0.0
`},
}

// TestCarrierDocumentsStayOutOfTheApprovedSurface proves a carrier joins no
// ledger entry, alone or added to the committed render, even when it carries
// an unresolved substitution.
func TestCarrierDocumentsStayOutOfTheApprovedSurface(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	for _, tc := range carrierFixtures {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, substitutionProblems, err := evaluateRenderedSurface([]byte(tc.manifest))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 || len(substitutionProblems) != 0 {
				t.Fatalf("carrier joined the approved surface: entries=%q substitutions=%v",
					surfaceLedger(entries), substitutionProblems)
			}
			added := append(append(bytes.Clone(rendered), []byte("\n---\n")...), tc.manifest...)
			if err := validateAuthorization(role, boundary, added); err != nil {
				t.Fatalf("an added carrier needs an approval: %v", err)
			}
		})
	}
}

// TestDependencyUpdatesToCarriersNeedNoApproval moves the committed carriers
// the way dependency updates do. None of these may strand on the ledger.
func TestDependencyUpdatesToCarriersNeedNoApproval(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	spec := func(document map[string]any) map[string]any {
		t.Helper()
		value, ok := document["spec"].(map[string]any)
		if !ok {
			t.Fatal("carrier fixture has no spec")
		}
		return value
	}
	for _, tc := range []struct {
		name, key string
		mutate    func(map[string]any)
	}{
		{
			name: "Helm chart version and values", key: "helm.toolkit.fluxcd.io/v2|HelmRelease|longhorn-system|longhorn",
			mutate: func(document map[string]any) {
				chart, _ := spec(document)["chart"].(map[string]any)
				chartSpec, ok := chart["spec"].(map[string]any)
				if !ok {
					t.Fatal("longhorn HelmRelease has no chart spec")
				}
				chartSpec["version"] = "9.8.7"
				spec(document)["values"].(map[string]any)["renovateBump"] = true
			},
		},
		{
			name: "Helm repository URL", key: "source.toolkit.fluxcd.io/v1|HelmRepository|longhorn-system|longhorn",
			mutate: func(document map[string]any) { spec(document)["url"] = "https://charts.example.invalid" },
		},
		{
			name: "OCI source reference", key: "source.toolkit.fluxcd.io/v1|OCIRepository|world-at-ruin|world-at-ruin",
			mutate: func(document map[string]any) { spec(document)["ref"] = map[string]any{"tag": "v9.8.7"} },
		},
		{
			name: "Git source commit", key: "source.toolkit.fluxcd.io/v1|GitRepository|unifi|unifi",
			mutate: func(document map[string]any) {
				spec(document)["ref"] = map[string]any{"commit": strings.Repeat("a", 40)}
			},
		},
		{
			name: "Crossplane provider package", key: "pkg.crossplane.io/v1|Provider||provider-upjet-unifi",
			mutate: func(document map[string]any) {
				spec(document)["package"] = "ghcr.io/devantler-tech/provider-upjet-unifi:v9.8.7"
			},
		},
		{
			name: "Flux handoff path", key: "kustomize.toolkit.fluxcd.io/v1|Kustomization|flux-system|apps",
			mutate: func(document map[string]any) { spec(document)["path"] = "./moved" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := mutateRenderedDocument(t, rendered, tc.key, tc.mutate)
			if err := validateAuthorization(role, boundary, changed); err != nil {
				t.Fatalf("a carrier update needs an approval: %v", err)
			}
		})
	}
}

// TestHandoffSubstitutionInputsNeedApproval proves the one part of a Flux
// handoff the ledger keeps: an inline value or a reordered source changes what
// Flux substitutes into an RBAC subject such as `oidc:${admin_email}`, so it
// moves that handoff's entry and nothing else.
func TestHandoffSubstitutionInputsNeedApproval(t *testing.T) {
	role, boundary, rendered := repositoryInputs(t)
	const key = "kustomize.toolkit.fluxcd.io/v1|Kustomization|flux-system|apps"
	postBuild := func(t *testing.T, document map[string]any) map[string]any {
		t.Helper()
		spec, _ := document["spec"].(map[string]any)
		value, ok := spec["postBuild"].(map[string]any)
		if !ok {
			t.Fatal("flux-system/apps has no post-build substitution")
		}
		return value
	}
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{name: "inline value", mutate: func(t *testing.T, document map[string]any) {
			postBuild(t, document)["substitute"] = map[string]any{"admin_email": "attacker@example.invalid"}
		}},
		{name: "reordered sources", mutate: func(t *testing.T, document map[string]any) {
			sources, ok := postBuild(t, document)["substituteFrom"].([]any)
			if !ok || len(sources) < 2 {
				t.Fatal("flux-system/apps substitutes from fewer than two sources")
			}
			sources[0], sources[len(sources)-1] = sources[len(sources)-1], sources[0]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := mutateRenderedDocument(t, rendered, key, func(document map[string]any) { tc.mutate(t, document) })
			var mismatch *surfaceMismatchError
			if err := validateAuthorization(role, boundary, changed); !errors.As(err, &mismatch) {
				t.Fatalf("changed substitution error = %v, want a ledger mismatch", err)
			}
			if len(mismatch.delta) != 2 {
				t.Fatalf("changed substitution moved %d ledger lines, want 2: %v", len(mismatch.delta), mismatch.delta)
			}
			for _, line := range mismatch.delta {
				if !strings.Contains(line, " "+key+" ") {
					t.Fatalf("changed substitution moved an unrelated ledger line: %s", line)
				}
			}
		})
	}
}

// TestAuthorizationSurfaceEntryFingerprintsOnlyAuthorizationFields pins what
// each authorization object's ledger entry covers: its grant moves the entry,
// and its other metadata does not.
func TestAuthorizationSurfaceEntryFingerprintsOnlyAuthorizationFields(t *testing.T) {
	const role = `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: reader
  namespace: tenant
  labels:
    app.kubernetes.io/part-of: tenant
  annotations:
    description: reads pods
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get]
`
	const clusterRole = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: tenant-edit
  labels:
    rbac.authorization.k8s.io/aggregate-to-edit: "true"
  annotations:
    description: tenant editors
aggregationRule:
  clusterRoleSelectors:
    - matchLabels:
        devantler.tech/aggregate-to-tenant-edit: "true"
rules: []
`
	const binding = `apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: readers
  namespace: tenant
  labels:
    app.kubernetes.io/part-of: tenant
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: reader
subjects:
  - kind: ServiceAccount
    name: reader
    namespace: tenant
`
	const admissionPolicy = `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: guard
  annotations:
    description: guards bindings
spec:
  failurePolicy: Fail
`
	const kyvernoPolicy = `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: mutate-role
  annotations:
    policies.kyverno.io/title: Mutate roles
spec:
  rules:
    - name: mutate-role
      match:
        any:
          - resources:
              kinds: [Role]
`
	const handoff = `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./apps
  sourceRef:
    kind: OCIRepository
    name: flux-system
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: variables-base
      - kind: ConfigMap
        name: variables-cluster
`
	const iamRole = `apiVersion: iam.aws.m.upbound.io/v1beta1
kind: Role
metadata:
  name: eks-ci
  namespace: aws
  annotations:
    crossplane.io/external-name: eks-ci
spec:
  forProvider:
    maxSessionDuration: 3600
`

	entry := func(t *testing.T, contents string) string {
		t.Helper()
		documents, err := decodeDocuments([]byte(contents))
		if err != nil || len(documents) != 1 {
			t.Fatalf("decode fixture: documents=%d error=%v", len(documents), err)
		}
		actual, err := authorizationSurfaceEntry(identityOf(documents[0]), documents[0])
		if err != nil {
			t.Fatal(err)
		}
		return actual
	}

	for _, tc := range []struct {
		name, manifest, old, new string
		moves                    bool
	}{
		{"role rules", role, "verbs: [get]", "verbs: [get, list]", true},
		{"role substitution switch", role, "    description: reads pods\n", "    description: reads pods\n    kustomize.toolkit.fluxcd.io/substitute: disabled\n", true},
		{"role autoupdate switch", role, "    description: reads pods\n", "    description: reads pods\n    rbac.authorization.kubernetes.io/autoupdate: \"false\"\n", true},
		{"role apply skipped", role, "    description: reads pods\n", "    description: reads pods\n    kustomize.toolkit.fluxcd.io/ssa: Ignore\n", true},
		{"role substitution label", role, "    app.kubernetes.io/part-of: tenant\n", "    app.kubernetes.io/part-of: tenant\n    kustomize.toolkit.fluxcd.io/substitute: disabled\n", true},
		{"role label", role, "part-of: tenant", "part-of: platform", false},
		{"role description", role, "reads pods", "reads every pod", false},
		{"cluster role aggregation rule", clusterRole, "aggregate-to-tenant-edit", "aggregate-to-admin", true},
		{"cluster role aggregation label", clusterRole, "aggregate-to-edit", "aggregate-to-admin", true},
		{"cluster role rules", clusterRole, "rules: []", "rules: [{apiGroups: [\"\"], resources: [secrets], verbs: [get]}]", true},
		{"cluster role description", clusterRole, "tenant editors", "every tenant editor", false},
		{"binding subject", binding, "    name: reader\n    namespace: tenant", "    name: reader\n    namespace: aws", true},
		{"binding role", binding, "kind: Role\n  name: reader", "kind: ClusterRole\n  name: reader", true},
		{"binding label", binding, "part-of: tenant", "part-of: platform", false},
		{"binding substitution label", binding, "    app.kubernetes.io/part-of: tenant\n", "    app.kubernetes.io/part-of: tenant\n    kustomize.toolkit.fluxcd.io/substitute: disabled\n", true},
		{"handoff inline substitution", handoff, "  postBuild:\n", "  postBuild:\n    substitute:\n      admin_email: attacker@example.invalid\n", true},
		{"handoff substitution order", handoff, "name: variables-base\n      - kind: ConfigMap\n        name: variables-cluster", "name: variables-cluster\n      - kind: ConfigMap\n        name: variables-base", true},
		{"handoff path", handoff, "path: ./apps", "path: ./moved", false},
		{"handoff source", handoff, "    name: flux-system\n", "    name: unreviewed\n", false},
		{"admission policy spec", admissionPolicy, "failurePolicy: Fail", "failurePolicy: Ignore", true},
		{"admission policy description", admissionPolicy, "guards bindings", "guards every binding", false},
		{"Kyverno policy spec", kyvernoPolicy, "kinds: [Role]", "kinds: [ClusterRole]", true},
		{"Kyverno policy title", kyvernoPolicy, "Mutate roles", "Mutate every role", false},
		{"IAM external name", iamRole, "external-name: eks-ci", "external-name: administrator", true},
		{"IAM spec", iamRole, "maxSessionDuration: 3600", "maxSessionDuration: 43200", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(tc.manifest, tc.old, tc.new, 1)
			if changed == tc.manifest {
				t.Fatalf("fixture has no %q to change", tc.old)
			}
			if moved := entry(t, changed) != entry(t, tc.manifest); moved != tc.moves {
				t.Fatalf("ledger entry moved = %v, want %v", moved, tc.moves)
			}
		})
	}
}
