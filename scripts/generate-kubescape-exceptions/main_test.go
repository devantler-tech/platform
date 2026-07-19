package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCSE writes a CR file into a temp dir and returns the dir.
func writeCSE(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exception.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write CR: %v", err)
	}

	return dir
}

// TestGenerateResourcesMatch verifies resource matches become anchored designators.
func TestGenerateResourcesMatch(t *testing.T) {
	dir := writeCSE(t, `
apiVersion: security.devantler.tech/v1alpha1
kind: ClusterSecurityException
metadata:
  name: exec-into-container-rbac
spec:
  reason: |
    Flux impersonates the tenant
    service account.
  posture:
    - controlID: C-0002
      action: ignore
  match:
    resources:
      - apiGroup: rbac.authorization.k8s.io
        kind: ClusterRole
        name: ^flux-operator$
`)

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(policies) != 1 {
		t.Fatalf("want 1 policy, got %d", len(policies))
	}

	got := policies[0]
	if got.Name != "exec-into-container-rbac" {
		t.Errorf("name = %q", got.Name)
	}

	if got.PolicyType != "postureExceptionPolicy" {
		t.Errorf("policyType = %q", got.PolicyType)
	}

	// controlID is a plain value => anchored; an already-anchored name is kept.
	if got.PosturePolicies[0].ControlID != "^C-0002$" {
		t.Errorf("controlID = %q, want ^C-0002$", got.PosturePolicies[0].ControlID)
	}

	attrs := got.Resources[0].Attributes
	if attrs["kind"] != "^ClusterRole$" {
		t.Errorf("kind = %q, want ^ClusterRole$", attrs["kind"])
	}

	if attrs["name"] != "^flux-operator$" {
		t.Errorf("name = %q, want ^flux-operator$ (explicit anchors preserved)", attrs["name"])
	}

	// apiGroup is intentionally dropped (no such designator attribute).
	if _, ok := attrs["apiGroup"]; ok {
		t.Error("apiGroup must not be emitted as a designator attribute")
	}

	// Reason is whitespace-collapsed onto one line.
	if got.Reason != "Flux impersonates the tenant service account." {
		t.Errorf("reason = %q", got.Reason)
	}
}

// TestGenerateNamespaceSelector verifies namespace values become one exact-match regex.
func TestGenerateNamespaceSelector(t *testing.T) {
	dir := writeCSE(t, `
kind: ClusterSecurityException
metadata:
  name: privileged-system-namespaces
spec:
  posture:
    - controlID: C-0013
      action: ignore
  match:
    namespaceSelector:
      matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: In
          values: [kube-system, cilium-secrets]
`)

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	got := policies[0].Resources[0].Attributes["namespace"]
	if got != "^(kube-system|cilium-secrets)$" {
		t.Errorf("namespace = %q, want ^(kube-system|cilium-secrets)$", got)
	}
}

// TestGenerateNoMatchIsClusterWide verifies an omitted match targets every
// resource, including cluster-scoped resources that have no namespace.
func TestGenerateNoMatchIsClusterWide(t *testing.T) {
	dir := writeCSE(t, `
kind: ClusterSecurityException
metadata:
  name: cluster-wide
spec:
  posture:
    - controlID: C-0034
      action: ignore
`)

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	attributes := policies[0].Resources[0].Attributes
	if got := attributes["kind"]; got != ".*" {
		t.Errorf("kind = %q, want .* (resource-wide default)", got)
	}

	if _, ok := attributes["namespace"]; ok {
		t.Error("cluster-wide default must not require a namespace")
	}
}

// TestGenerateFrameworkScopedPosture verifies a CSE framework constraint is
// preserved in Kubescape's native posture policy instead of being widened.
func TestGenerateFrameworkScopedPosture(t *testing.T) {
	dir := writeCSE(t, `
kind: ClusterSecurityException
metadata:
  name: nsa-only
spec:
  posture:
    - frameworkName: NSA
      controlID: C-0030
      action: ignore
`)

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	rendered, err := render(policies)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var got []struct {
		PosturePolicies []map[string]string `json:"posturePolicies"`
	}
	if err := json.Unmarshal(rendered, &got); err != nil {
		t.Fatalf("unmarshal rendered policy: %v", err)
	}

	if framework := got[0].PosturePolicies[0]["frameworkName"]; framework != "^NSA$" {
		t.Errorf("frameworkName = %q, want ^NSA$", framework)
	}
}

// TestGenerateSkipsNonExceptionDocumentsAndSorts verifies filtering and deterministic order.
func TestGenerateSkipsNonExceptionDocumentsAndSorts(t *testing.T) {
	dir := writeCSE(t, `
kind: ConfigMap
metadata:
  name: not-an-exception
---
kind: ClusterSecurityException
metadata:
  name: zzz-last
spec:
  posture:
    - controlID: C-0002
      action: ignore
---
kind: ClusterSecurityException
metadata:
  name: aaa-first
spec:
  posture:
    - controlID: C-0002
      action: ignore
`)

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if len(policies) != 2 {
		t.Fatalf("want 2 policies (ConfigMap skipped), got %d", len(policies))
	}

	if policies[0].Name != "aaa-first" || policies[1].Name != "zzz-last" {
		t.Errorf("policies not sorted by name: %q, %q", policies[0].Name, policies[1].Name)
	}
}

// TestGenerateFailsClosed pins the fail-closed contract: every unrecognised CR
// shape aborts instead of silently dropping or widening an exception.
func TestGenerateFailsClosed(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body string
		want string
	}{
		"partially anchored value": {
			body: `
kind: ClusterSecurityException
metadata: {name: partial}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match:
    resources: [{kind: ClusterRole, name: ^open-ended}]
`,
			want: "partially anchored",
		},
		"unsupported posture action": {
			body: `
kind: ClusterSecurityException
metadata: {name: bad-action}
spec:
  posture: [{controlID: C-0002, action: alert}]
`,
			want: "unsupported posture action",
		},
		"expiration cannot be silently dropped": {
			body: `
kind: ClusterSecurityException
metadata: {name: temporary}
spec:
  expiresAt: "2026-12-01T00:00:00Z"
  posture: [{controlID: C-0002, action: ignore}]
`,
			want: "spec.expiresAt cannot be preserved",
		},
		"unknown match key": {
			body: `
kind: ClusterSecurityException
metadata: {name: bad-match-key}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match: {labelSelector: {app: web}}
`,
			want: "unsupported match keys",
		},
		"unknown resources key": {
			body: `
kind: ClusterSecurityException
metadata: {name: bad-resource-key}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match:
    resources: [{kind: Pod, namespace: kube-system}]
`,
			want: "unsupported match.resources keys",
		},
		"both resources and namespaceSelector": {
			body: `
kind: ClusterSecurityException
metadata: {name: both}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match:
    resources: [{kind: Pod}]
    namespaceSelector:
      matchExpressions: [{key: kubernetes.io/metadata.name, operator: In, values: [x]}]
`,
			want: "both match.resources and match.namespaceSelector set",
		},
		"empty match is not coerced to cluster-wide": {
			body: `
kind: ClusterSecurityException
metadata: {name: empty-match}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match: {}
`,
			want: "spec.match must be a non-empty mapping",
		},
		"resource without kind": {
			body: `
kind: ClusterSecurityException
metadata: {name: no-kind}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match:
    resources: [{apiGroup: apps}]
`,
			want: "match.resources entry without a kind",
		},
		"unsupported matchExpression operator": {
			body: `
kind: ClusterSecurityException
metadata: {name: bad-operator}
spec:
  posture: [{controlID: C-0002, action: ignore}]
  match:
    namespaceSelector:
      matchExpressions: [{key: kubernetes.io/metadata.name, operator: NotIn, values: [x]}]
`,
			want: "matchExpressions are supported",
		},
		"empty posture": {
			body: `
kind: ClusterSecurityException
metadata: {name: no-posture}
spec:
  posture: []
`,
			want: "spec.posture is empty",
		},
		"missing metadata.name": {
			body: `
kind: ClusterSecurityException
spec:
  posture: [{controlID: C-0002, action: ignore}]
`,
			want: "missing metadata.name",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := generate(writeCSE(t, tc.body))
			if err == nil {
				t.Fatalf("want a fail-closed error containing %q, got none", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestGenerateRejectsDuplicateNames verifies duplicate policy identities fail closed.
func TestGenerateRejectsDuplicateNames(t *testing.T) {
	dir := writeCSE(t, `
kind: ClusterSecurityException
metadata: {name: dupe}
spec:
  posture: [{controlID: C-0002, action: ignore}]
---
kind: ClusterSecurityException
metadata: {name: dupe}
spec:
  posture: [{controlID: C-0013, action: ignore}]
`)

	_, err := generate(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate exception name") {
		t.Fatalf("want duplicate-name error, got %v", err)
	}
}

// TestGenerateRejectsEmptyDirectory verifies an empty source cannot produce a permissive file.
func TestGenerateRejectsEmptyDirectory(t *testing.T) {
	_, err := generate(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no ClusterSecurityException documents found") {
		t.Fatalf("want empty-directory error, got %v", err)
	}
}

// TestGenerateAgainstRealExceptions is the behavioural check: the committed CRs
// must actually convert, and the rendered file must be the JSON array of
// PostureExceptionPolicy objects that `ksail workload scan --exceptions` reads.
func TestGenerateAgainstRealExceptions(t *testing.T) {
	dir := filepath.Join("..", "..", defaultDir)
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("exceptions dir not present: %v", err)
	}

	policies, err := generate(dir)
	if err != nil {
		t.Fatalf("the committed ClusterSecurityException CRs must convert: %v", err)
	}

	if len(policies) == 0 {
		t.Fatal("want at least one policy from the committed CRs")
	}

	rendered, err := render(policies)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var roundTripped []map[string]any
	if err := json.Unmarshal(rendered, &roundTripped); err != nil {
		t.Fatalf("rendered exceptions must be a JSON array: %v", err)
	}

	for i, got := range roundTripped {
		if got["policyType"] != "postureExceptionPolicy" {
			t.Errorf("policy %d: policyType = %v", i, got["policyType"])
		}

		if got["name"] == "" || got["name"] == nil {
			t.Errorf("policy %d: missing name", i)
		}

		if resources, ok := got["resources"].([]any); !ok || len(resources) == 0 {
			t.Errorf("policy %d: must carry at least one designator", i)
		}
	}
}
