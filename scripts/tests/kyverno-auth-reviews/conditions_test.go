package authreviews_test

import (
	"os"
	"testing"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"
)

// Evaluate the authored expression, not a second implementation of its predicate.
func TestOnlyAlreadyFilteredStandardReviewsSkipAdmission(t *testing.T) {
	data, err := os.ReadFile("../../../k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var release struct {
		Spec struct {
			Values struct {
				Config struct {
					MatchConditions []struct{ Name, Expression string } `yaml:"matchConditions"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &release); err != nil {
		t.Fatal(err)
	}
	conditions := release.Spec.Values.Config.MatchConditions
	if len(conditions) != 1 {
		t.Fatalf("want one narrow match condition, got %d", len(conditions))
	}
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(conditions[0].Expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		group, resource string
		dispatch        bool
	}{
		{"authentication.k8s.io", "tokenreviews", false},
		{"authorization.k8s.io", "subjectaccessreviews", false},
		{"authorization.k8s.io", "selfsubjectaccessreviews", false},
		{"authorization.k8s.io", "localsubjectaccessreviews", true},
		{"authorization.k8s.io", "selfsubjectrulesreviews", true},
		{"example.test", "tokenreviews", true},
		{"example.test", "subjectaccessreviews", true},
		{"authentication.k8s.io", "subjectaccessreviews", true},
		{"authorization.k8s.io", "tokenreviews", true},
		{"authorization.k8s.io", "subjectaccessreviews-extra", true},
		{"", "pods", true}, {"apps", "deployments", true},
		{"", "secrets", true}, {"rbac.authorization.k8s.io", "roles", true},
		{"rbac.authorization.k8s.io", "clusterrolebindings", true},
	} {
		t.Run(tc.group+"/"+tc.resource, func(t *testing.T) {
			out, _, err := program.Eval(map[string]any{"request": map[string]any{
				"resource": map[string]any{"group": tc.group, "resource": tc.resource},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := out.Value(); got != tc.dispatch {
				t.Errorf("dispatch = %v, want %t", got, tc.dispatch)
			}
		})
	}
	if _, _, err := program.Eval(map[string]any{"request": map[string]any{}}); err == nil {
		t.Fatal("malformed request must not silently become an exclusion")
	}
}
