package authreviews_test

import (
	"os"
	"reflect"
	"testing"

	"cel.dev/cel-go/cel"
	"gopkg.in/yaml.v3"
)

// The existing live prune proof intentionally refuses config drift. Catch that
// drift locally too, before paying to create its disposable CI cluster.
func TestStaleReportProofKeepsProductionAdmissionConfig(t *testing.T) {
	type values struct {
		Config   map[string]any
		Features struct {
			BackgroundScan struct {
				SkipResourceFilters *bool `yaml:"skipResourceFilters"`
			} `yaml:"backgroundScan"`
		}
	}
	var production struct{ Spec struct{ Values values } }
	var proof values
	for _, input := range []struct {
		path   string
		target any
	}{
		{"../../../k8s/bases/infrastructure/controllers/kyverno/helm-release.yaml", &production},
		{"../../../tests/kyverno-stale-report-prune/values.yaml", &proof},
	} {
		data, err := os.ReadFile(input.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(data, input.target); err != nil {
			t.Fatal(err)
		}
	}
	if len(production.Spec.Values.Config) == 0 || !reflect.DeepEqual(production.Spec.Values.Config, proof.Config) {
		t.Fatal("stale-report proof config differs from production")
	}
	want, got := production.Spec.Values.Features.BackgroundScan.SkipResourceFilters, proof.Features.BackgroundScan.SkipResourceFilters
	if want == nil || got == nil || *want != *got {
		t.Fatal("stale-report proof resource-filter behavior differs from production")
	}
}

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
