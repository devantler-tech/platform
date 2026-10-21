package main

import (
	"os"
	"testing"

	policiesv1beta1 "github.com/kyverno/api/api/policies.kyverno.io/v1beta1"
	"golang.org/x/mod/modfile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// These acceptances adjudicate overbroad database ranges, not an unpatched
// runtime. A dependency update must re-read the upstream ranges before carrying
// them forward. Removing the acceptances after database correction is allowed.
func TestAdvisoryAcceptancePinsReviewedRelease(t *testing.T) {
	if _, err := os.Stat(".govulncheck-allow.txt"); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Replace {
		if r.Old.Path == "github.com/kyverno/kyverno" {
			t.Fatal("re-read advisory ranges before replacing the reviewed Kyverno release")
		}
	}
	for _, r := range m.Require {
		if r.Mod.Path == "github.com/kyverno/kyverno" && r.Mod.Version == "v1.19.1" {
			return
		}
	}
	t.Fatal("re-read advisory ranges before carrying the acceptances to another Kyverno release; see platform#5154")
}

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
	policies := map[string]*policiesv1beta1.ValidatingPolicy{"adapter-control": &policy}
	old := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1", "kind": "WebApp",
		"metadata": map[string]any{"name": "fixture", "namespace": "fixture"}, "spec": map[string]any{"value": "before"},
	}}
	changed := old.DeepCopy()
	changed.Object["spec"] = map[string]any{"value": "after"}
	f := fixture{Label: "distinct update", Policy: "adapter-control", Actor: "fixture", Object: changed, OldObject: old, Operation: "UPDATE", Subresource: "status", Expected: "fail"}
	if err := evaluate(f, policies); err != nil {
		t.Fatal(err)
	}
	f.Object, f.Expected = old.DeepCopy(), "pass"
	if err := evaluate(f, policies); err != nil {
		t.Fatal(err)
	}
	f.Subresource = ""
	if err := evaluate(f, policies); err == nil {
		t.Fatal("a non-matching policy must not count as an evaluated control")
	}
	f.Subresource, f.OldObject = "status", nil
	if err := evaluate(f, policies); err == nil {
		t.Fatal("an UPDATE without its old object must be refused")
	}
}
