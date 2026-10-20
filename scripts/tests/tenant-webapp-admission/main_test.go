package main

import (
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"github.com/kyverno/kyverno/pkg/cel/matching"
	"github.com/kyverno/kyverno/pkg/cel/policies/vpol/compiler"
	vpolengine "github.com/kyverno/kyverno/pkg/cel/policies/vpol/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestAdapterPreservesOldObjectAndSubresource(t *testing.T) {
	var policy policiesv1beta1.ValidatingPolicy
	if err := yaml.UnmarshalStrict([]byte(`apiVersion: policies.kyverno.io/v1
kind: ValidatingPolicy
metadata: {name: adapter-control}
spec:
  validationActions: [Deny]
  matchConstraints:
    resourceRules:
      - apiGroups: [kro.run]
        apiVersions: [v1alpha1]
        operations: [UPDATE]
        resources: [webapps/status]
  validations:
    - expression: "object.spec == oldObject.spec && request.subResource == 'status'"
      message: distinct updates are denied
`), &policy); err != nil {
		t.Fatal(err)
	}
	provider, err := vpolengine.NewProvider(compiler.NewCompiler(), []policiesv1beta1.ValidatingPolicyLike{&policy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	engines := map[string]vpolengine.Engine{"adapter-control": vpolengine.NewEngine(provider, func(string) *corev1.Namespace { return nil }, matching.NewMatcher())}
	old := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1", "kind": "WebApp",
		"metadata": map[string]any{"name": "fixture", "namespace": "fixture"}, "spec": map[string]any{"value": "before"},
	}}
	changed := old.DeepCopy()
	changed.Object["spec"] = map[string]any{"value": "after"}
	f := fixture{Label: "distinct update", Policy: "adapter-control", Actor: "fixture", Object: changed, OldObject: old, Operation: "UPDATE", Subresource: "status", Expected: "fail"}
	if err := evaluate(f, engines); err != nil {
		t.Fatal(err)
	}
	f.Object, f.Expected = old.DeepCopy(), "pass"
	if err := evaluate(f, engines); err != nil {
		t.Fatal(err)
	}
	f.Subresource = ""
	if err := evaluate(f, engines); err == nil {
		t.Fatal("a non-matching policy must not count as an evaluated control")
	}
	f.Subresource, f.OldObject = "status", nil
	if err := evaluate(f, engines); err == nil {
		t.Fatal("an UPDATE without its old object must be refused")
	}
}
