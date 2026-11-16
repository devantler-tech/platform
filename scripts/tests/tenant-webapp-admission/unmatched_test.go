package main

import (
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	celengine "github.com/kyverno/kyverno/pkg/cel/engine"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
)

// Unmatched is an explicit coverage expectation, not a successful admission.
// A real pass, denial, evaluation error or condition skip cannot satisfy it.
func TestExplicitUnmatchedPolicy(t *testing.T) {
	base, object := skipPolicyControl(t)
	for _, name := range []string{"resource does not match", "match skip is not unmatched", "pass is not unmatched", "denial is not unmatched", "evaluation error is not unmatched", "missing policy is not unmatched"} {
		t.Run(name, func(t *testing.T) {
			policy := base.DeepCopy()
			f := fixture{Label: name, Policy: "skip-control", Actor: "synthetic", Object: object.DeepCopy(), Operation: "CREATE", Expected: "unmatched"}
			switch name {
			case "resource does not match":
				policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"services"}
			case "pass is not unmatched":
				policy.Spec.MatchConditions[0].Expression = "true"
				policy.Spec.Validations[0].Expression = "true"
			case "denial is not unmatched":
				policy.Spec.MatchConditions[0].Expression = "true"
			case "evaluation error is not unmatched":
				policy.Spec.MatchConditions[0].Expression = "object.missing == 'value'"
			case "missing policy is not unmatched":
				f.Policy = "missing"
			}
			err := evaluate(f, map[string]*policiesv1beta1.ValidatingPolicy{"skip-control": policy})
			if (err != nil) != (name != "resource does not match") {
				t.Fatalf("unexpected coverage result: %v", err)
			}
		})
	}
}

func TestGeneratedResponsesKeepPolicyIdentity(t *testing.T) {
	base, _ := skipPolicyControl(t)
	derived := base.DeepCopy()
	derived.Spec.Validations[0].Expression = "true"
	for _, change := range []string{"same family", "name", "namespace", "group/version", "kind", "nil"} {
		t.Run(change, func(t *testing.T) {
			other := derived.DeepCopy()
			switch change {
			case "name":
				other.Name = "unrelated"
			case "namespace":
				other.Namespace = "unrelated"
			case "group/version":
				other.APIVersion = "other.invalid/v1"
			case "kind":
				other.Kind = "NamespacedValidatingPolicy"
			}
			response := celengine.EngineResponse{Policies: []celengine.ValidatingPolicyResponse{
				{Policy: base, Rules: []engineapi.RuleResponse{{}}},
				{Policy: other, Rules: []engineapi.RuleResponse{{}}},
			}}
			if change == "nil" {
				response.Policies[1].Policy = nil
			}
			rules, err := policyResponseRules(base, response)
			if change == "same family" {
				if err != nil || len(rules) != 2 {
					t.Fatal("an originating or generated rule was discarded", len(rules), err)
				}
			} else if err == nil {
				t.Fatal("an unrelated policy family supplied a result")
			}
		})
	}
}
