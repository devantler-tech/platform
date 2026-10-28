package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// A declared compatibility skip must come from the real match-condition path.
// It cannot stand in for an unevaluated policy, denial, pass or runtime error.
func skipPolicyControl(t *testing.T) (*policiesv1beta1.ValidatingPolicy, *unstructured.Unstructured) {
	t.Helper()
	var base policiesv1beta1.ValidatingPolicy
	if err := yaml.UnmarshalStrict([]byte(`apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata: {name: skip-control}
spec:
  failurePolicy: Fail
  validationActions: [Deny]
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE]
        resources: [configmaps]
  matchConditions:
    - name: deliberate-skip
      expression: "false"
  validations:
    - expression: "false"
      message: synthetic denial
`), &base); err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "synthetic", "namespace": "synthetic"},
	}}
	return &base, object
}

func TestExplicitMatchConditionSkip(t *testing.T) {
	base, object := skipPolicyControl(t)
	for _, name := range []string{"explicit skip", "skip is not pass", "skip is not denial", "denial is not skip", "pass is not skip", "nonmatching policy is not skip", "evaluation error is not skip"} {
		t.Run(name, func(t *testing.T) {
			policy := base.DeepCopy()
			f := fixture{Label: name, Policy: "skip-control", Actor: "synthetic", Object: object.DeepCopy(), Operation: "CREATE", Expected: "skip"}
			wantError := name != "explicit skip"
			switch name {
			case "skip is not pass":
				f.Expected = "pass"
			case "skip is not denial":
				f.Expected = "fail"
			case "denial is not skip":
				policy.Spec.MatchConditions[0].Expression = "true"
			case "pass is not skip":
				policy.Spec.MatchConditions[0].Expression = "true"
				policy.Spec.Validations[0].Expression = "true"
			case "nonmatching policy is not skip":
				policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"services"}
			case "evaluation error is not skip":
				policy.Spec.MatchConditions[0].Expression = "object.missing == 'value'"
			}
			err := evaluate(f, map[string]*policiesv1beta1.ValidatingPolicy{"skip-control": policy})
			if (err != nil) != wantError {
				t.Fatalf("want error=%v, got %v", wantError, err)
			}
		})
	}
}

func TestAllSkippedBatchIsRejected(t *testing.T) {
	policy, object := skipPolicyControl(t)
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "policy.yaml")
	policyData, err := yaml.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyPath, policyData, 0o600); err != nil {
		t.Fatal(err)
	}
	f := fixture{Label: "only skipped", Policy: "skip-control", Actor: "synthetic", Object: object, Operation: "CREATE", Expected: "skip"}
	input, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(dir, "input.jsonl")
	if err := os.WriteFile(inputPath, append(input, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldArgs, oldInput := os.Args, os.Stdin
	os.Args, os.Stdin = []string{"admission-driver", policyPath}, reader
	defer func() { os.Args, os.Stdin = oldArgs, oldInput }()
	if err := run(); err == nil || !strings.Contains(err.Error(), "no non-skip admission controls") {
		t.Fatalf("a batch that never validates an object must fail, got %v", err)
	}
}
