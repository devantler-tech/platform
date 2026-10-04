package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestActions(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"preserved", "validate: {failureAction: Enforce}", ""},
		{"downgrade", "validate: {failureAction: Audit}", "expected Enforce"},
		{"implicit", "validate: {message: deny}", "explicit rule failureAction"},
		{"removed-rule", "mutate: {patchStrategicMerge: {}}", "missing baseline rule"},
		{"extra-rule", "validate: {failureAction: Enforce}\n  - name: extra\n    validate: {failureAction: Enforce}", "unreviewed validation rule"},
		{"invalid", "validate: {failureAction: Unknown}", "explicit rule failureAction"},
		{"namespace-downgrade", "validate: {failureAction: Enforce, failureActionOverrides: [{action: Audit, namespaces: [tenant]}]}", "unreviewed rule failureActionOverrides"},
		{"empty-overrides", "validate: {failureAction: Enforce, failureActionOverrides: []}", "unreviewed rule failureActionOverrides"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			body := "kind: ClusterPolicy\nmetadata: {name: boundary}\nspec:\n  rules:\n  - name: deny\n    " + tt.body + "\n"
			if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			err := verify(dir, map[string]string{"boundary/deny": "Enforce"})
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
		})
	}
}

func TestRequiredCIWiring(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If    string   `yaml:"if"`
			Needs []string `yaml:"needs"`
			Steps []struct {
				If       string            `yaml:"if"`
				Continue bool              `yaml:"continue-on-error"`
				Run      string            `yaml:"run"`
				With     map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "merge_group"} {
		if _, found := workflow.On[event]; !found {
			t.Fatalf("missing %s trigger", event)
		}
	}
	changes, found := workflow.Jobs["changes"]
	if !found || changes.If != "" {
		t.Fatal("changes must run unconditionally")
	}
	foundGuard, foundPaths := false, false
	for _, step := range changes.Steps {
		const guardRun = `go test ./scripts/validate-policy-actions
go run ./scripts/validate-policy-actions
policy_render="$(mktemp -d)"
trap 'rm -rf "$policy_render"' EXIT
kubectl kustomize k8s/providers/hetzner/infrastructure > "$policy_render/production.yaml"
go run ./scripts/validate-policy-actions "$policy_render"
`
		if step.Run == guardRun && step.If == "" && !step.Continue {
			foundGuard = true
		}
		filter := step.With["filters"]
		if strings.Contains(filter, "'scripts/validate-policy-actions/**'") && strings.Contains(filter, "'tests/policy-failure-actions.json'") {
			foundPaths = true
		}
	}
	if !foundGuard || !foundPaths {
		t.Fatalf("required guard=%t, manifest path coverage=%t", foundGuard, foundPaths)
	}
	for _, dependency := range workflow.Jobs["ci-required-checks"].Needs {
		if dependency == "changes" {
			return
		}
	}
	t.Fatal("required CI does not wait for the policy action guard")
}

// Kustomize patches can change admission behavior without editing a policy file.
// Exercise the real renderer, so a raw-source-only check cannot stand in for this.
func TestRenderedActions(t *testing.T) {
	for _, downgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "patched-downgrade"}[downgrade], func(t *testing.T) {
			dir := t.TempDir()
			policy := "apiVersion: kyverno.io/v1\nkind: ClusterPolicy\nmetadata: {name: boundary}\nspec:\n  rules:\n  - name: deny\n    validate: {failureAction: Enforce}\n"
			config := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [policy.yaml]\n"
			if downgrade {
				config += "patches:\n- target: {kind: ClusterPolicy, name: boundary}\n  patch: |-\n    - op: replace\n      path: /spec/rules/0/validate/failureAction\n      value: Audit\n"
			}
			for name, data := range map[string]string{"policy.yaml": policy, "kustomization.yaml": config} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			rendered, err := exec.Command("kubectl", "kustomize", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("render failed: %v: %s", err, rendered)
			}
			renderFile := filepath.Join(t.TempDir(), "rendered.yaml")
			if err := os.WriteFile(renderFile, rendered, 0600); err != nil {
				t.Fatal(err)
			}
			err = verify(renderFile, map[string]string{"boundary/deny": "Enforce"})
			if !downgrade && err != nil {
				t.Fatal(err)
			}
			if downgrade && (err == nil || !strings.Contains(err.Error(), "expected Enforce, found Audit")) {
				t.Fatalf("rendered downgrade was not rejected: %v", err)
			}
		})
	}
}
func TestDeprecatedFields(t *testing.T) {
	for _, field := range []string{"validationFailureAction: Enforce", "validationFailureActionOverrides: []"} {
		dir := t.TempDir()
		body := "kind: ClusterPolicy\nmetadata: {name: boundary}\nspec:\n  " + field + "\n  rules:\n  - name: deny\n    validate: {failureAction: Enforce}\n"
		if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		err := verify(dir, map[string]string{"boundary/deny": "Enforce"})
		if err == nil || !strings.Contains(err.Error(), "deprecated policy action") {
			t.Fatalf("got %v", err)
		}
	}
}
func TestEmptyCensus(t *testing.T) {
	if err := verify(t.TempDir(), map[string]string{"boundary/deny": "Enforce"}); err == nil {
		t.Fatal("empty census passed")
	}
}
func TestDuplicateRule(t *testing.T) {
	dir := t.TempDir()
	body := "kind: ClusterPolicy\nmetadata: {name: boundary}\nspec:\n  rules:\n  - name: deny\n    validate: {failureAction: Enforce}\n  - name: deny\n    validate: {failureAction: Enforce}\n"
	if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	err := verify(dir, map[string]string{"boundary/deny": "Enforce"})
	if err == nil || !strings.Contains(err.Error(), "duplicate validation rule") {
		t.Fatalf("got %v", err)
	}
}

func TestCollectionAndAlias(t *testing.T) {
	const policy = "kind: ClusterPolicy\nmetadata: {name: boundary}\nspec:\n  rules:\n  - name: deny\n    validate: {failureAction: Enforce}\n"
	for _, tt := range []struct{ name, body, want string }{
		{"list", "kind: List\nitems:\n- " + strings.ReplaceAll(strings.TrimSpace(policy), "\n", "\n  ") + "\n", ""},
		{"typed-list", "kind: ClusterPolicyList\nitems:\n- " + strings.ReplaceAll(strings.TrimSpace(policy), "\n", "\n  ") + "\n", ""},
		{"hidden-extra", policy + "---\nkind: List\nitems:\n- kind: ClusterPolicy\n  metadata: {name: extra}\n  spec:\n    rules:\n    - name: deny\n      validate: {failureAction: Audit}\n", "unreviewed validation rule"},
		{"aliased-action", strings.ReplaceAll(strings.Replace(policy, "metadata: {name: boundary}", "metadata: {name: boundary, annotations: {action: &approved Enforce}}", 1), "failureAction: Enforce", "failureAction: *approved"), ""},
		{"cyclic-list", policy + "---\n&cycle\nkind: List\nitems: [*cycle]\n", "cyclic"},
		{"unrelated-patch", policy + "---\n- op: replace\n  path: /metadata/name\n  value: unrelated\n", ""},
		{"unrelated-scalar", policy + "---\nplain configuration\n", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "policy.yaml"), []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			err := verify(dir, map[string]string{"boundary/deny": "Enforce"})
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("wanted %q, got %v", tt.want, err)
			}
		})
	}
}
